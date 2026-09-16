package rowpack

import (
	"context"
	"fmt"
	"time"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/metadata"
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

// Verify checks store integrity. Quick mode validates headers, index
// transactions, footers, boundaries and the parent chain. Full mode
// additionally decompresses every block and verifies every row's CRC and
// decodability. Any failure returns a structured CorruptionError.
func (s *Store) Verify(ctx context.Context, mode VerifyMode) (VerifyReport, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	start := time.Now()
	var rep VerifyReport
	st, err := s.captureState()
	if err != nil {
		return rep, err
	}
	view := st.view

	// Header + parent chain + index/footer cross checks.
	for _, sm := range view.Snapshots() {
		rep.SnapshotsChecked++
		if sm.Type == format.SnapshotFull {
			if sm.Parent != 0 {
				return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptData, Reason: "FULL snapshot has a parent"}
			}
		} else if sm.Type == format.SnapshotDelta {
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

	for _, bl := range view.Blocks() {
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
	return rep, nil
}
