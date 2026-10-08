package rowpack

import (
	"context"
	"fmt"
	"time"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/metadata"
)

// VerifyMode selects quick or full verification.
type VerifyMode uint8

const (
	VerifyQuick VerifyMode = iota
	VerifyFull
)

// VerifyReport summarizes a Verify run.
type VerifyReport struct {
	SnapshotsChecked uint64
	BlocksChecked    uint64
	RowsChecked      uint64
	DataBytesRead    uint64
	Duration         time.Duration
}

// VerifyScope bounds a Verify run. The zero value checks everything: every
// snapshot and every table. Snapshot limits the run to one snapshot (its own
// blocks and header; parent-chain checks still cover the chain it sits on).
// Tables limits the run to rows blocks of the given table addresses (same
// strings as Get/Scan); other blocks of matching snapshots still get their
// structural checks but not row decoding.
type VerifyScope struct {
	Snapshot SnapshotID
	Tables   []string
}

// coversSnapshot reports whether the scope includes snap.
func (sc VerifyScope) coversSnapshot(snap SnapshotID) bool {
	return sc.Snapshot == 0 || sc.Snapshot == snap
}

// coversTable reports whether the scope includes the table address; an empty
// table set means no table filtering.
func (sc VerifyScope) coversTable(addr string) bool {
	if len(sc.Tables) == 0 {
		return true
	}
	for _, t := range sc.Tables {
		if t == addr {
			return true
		}
	}
	return false
}

// addressOfTable resolves a table's address at the newest snapshot that knows
// it, for scope filtering; ok is false when unknown.
func addressOfTable(schemas *schemaIndex, snapshots []SnapshotInfo, tid TableID) (string, bool) {
	for i := len(snapshots) - 1; i >= 0; i-- {
		snap := snapshots[i].ID
		if schemas.nameOf(snap, tid) == "" {
			continue
		}
		return Qualify(schemas.nsOf(snap, tid), schemas.nameOf(snap, tid)), true
	}
	return "", false
}

// Verify checks store integrity. Quick mode validates headers, index
// transactions, footers, boundaries and the parent chain. Full mode
// additionally decompresses every block and verifies every row's CRC and
// decodability. The zero-value scope checks the whole store; see VerifyScope.
func (s *Store) Verify(ctx context.Context, mode VerifyMode, scope VerifyScope) (VerifyReport, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	start := time.Now()
	var rep VerifyReport
	st, err := s.captureState()
	if err != nil {
		return rep, err
	}
	view := st.view

	snapshots := view.Snapshots()

	// Header + parent chain + index/footer cross checks.
	for _, sm := range snapshots {
		if !scope.coversSnapshot(sm.ID) {
			continue
		}
		rep.SnapshotsChecked++
		switch sm.Type {
		case format.SnapshotFull:
			if sm.Parent != 0 {
				return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptData, Reason: "FULL snapshot has a parent"}
			}
		case format.SnapshotDelta:
			if view.Snapshot(sm.Parent) == nil {
				return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptIndex, Reason: fmt.Sprintf("DELTA parent %d missing", sm.Parent)}
			}
		}
		if sm.Depth > s.opts.Limits.MaxSnapshotDepth {
			return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptIndex, Reason: "snapshot chain too deep"}
		}
		seenAddr := make(map[string]TableID, len(st.schemas.bySnapshot[sm.ID]))
		for tid := range st.schemas.bySnapshot[sm.ID] {
			name := st.schemas.nameOf(sm.ID, tid)
			if name == "" {
				continue
			}
			addr := Qualify(st.schemas.nsOf(sm.ID, tid), name)
			if prev, dup := seenAddr[addr]; dup {
				return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, TableID: tid, Kind: ErrCorruptIndex,
					Reason: fmt.Sprintf("address %q claimed by tables %d and %d", addr, prev, tid)}
			}
			seenAddr[addr] = tid
		}
	}

	infos := make([]SnapshotInfo, len(snapshots))
	for i, sm := range snapshots {
		infos[i] = snapshotInfo(sm)
	}
	for _, bl := range view.Blocks() {
		if !scope.coversSnapshot(bl.SnapshotID) {
			continue
		}
		if bl.Kind == format.BlockKindRows {
			addr, _ := addressOfTable(st.schemas, infos, TableID(bl.TableID))
			if !scope.coversTable(addr) {
				continue
			}
		}
		rep.BlocksChecked++
		rep.DataBytesRead += uint64(bl.StoredSize)
		if bl.Kind == format.BlockKindRows {
			// Rows blocks validate the page container (header/directory CRC
			// and per-page bounds) without decompressing; full mode then walks
			// every page, verifying each record's page CRC and decodability.
			rc, err := s.loader.LoadRows(int64(bl.DataOffset), bl.BlockID)
			if err != nil {
				return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: err, Reason: reasonOf(err)}
			}
			if mode == VerifyFull {
				verr := rc.ForEach(func(rec codec.PageRecord) error {
					rep.RowsChecked++
					if rec.ChangeType != format.ChangeDelete {
						// A record whose schema version is absent from the catalog is
						// an index/metadata inconsistency just like an undecodable
						// body: skipping it would let Verify certify a store whose
						// rows can never be read.
						decoder, err := st.schemas.decoderFor(bl, rec.SchemaVersion)
						if err != nil {
							return fmt.Errorf("row %d: %w", rec.RowID, err)
						}
						if _, err := decoder.DecodeInto(nil, rec.Body, nil); err != nil {
							return fmt.Errorf("row %d: %w", rec.RowID, err)
						}
					}
					return nil
				})
				if verr != nil {
					// Preserve the underlying cause (e.g. ErrAuthFailed) so
					// errors.Is(ErrAuthFailed) works on the tamper path.
					return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: verr, Reason: reasonOf(verr)}
				}
			}
			continue
		}
		blk, err := s.loader.Load(int64(bl.DataOffset), bl.BlockID)
		if err != nil {
			return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: err, Reason: reasonOf(err)}
		}
		if mode == VerifyFull && bl.Kind == format.BlockKindMetadata {
			if _, err := metadata.Parse(blk.Raw); err != nil {
				return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, Kind: ErrCorruptData, Cause: err, Reason: reasonOf(err)}
			}
		}
	}
	rep.Duration = time.Since(start)
	if rep.Duration <= 0 {
		// Some platforms have a coarse wall-clock resolution and can report
		// zero for a very small verification. Keep the report meaningful.
		rep.Duration = time.Nanosecond
	}
	return rep, nil
}
