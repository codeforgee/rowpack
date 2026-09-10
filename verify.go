package rowpack

import (
	"context"
	"fmt"
	"time"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
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
		if sm.Type == fileformat.SnapshotFull {
			if sm.Parent != 0 {
				return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptData, Reason: "FULL snapshot has a parent"}
			}
		} else if sm.Type == fileformat.SnapshotDelta {
			if view.Snapshot(sm.Parent) == nil {
				return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptIndex, Reason: fmt.Sprintf("DELTA parent %d missing", sm.Parent)}
			}
		}
		if sm.Depth > s.opts.Limits.MaxSnapshotDepth {
			return rep, &CorruptionError{File: s.dataPath, SnapshotID: sm.ID, Kind: ErrCorruptIndex, Reason: "snapshot chain too deep"}
		}
	}

	for _, bl := range view.Blocks() {
		rep.BlocksChecked++
		rep.DataBytesRead += uint64(bl.StoredSize)
		if bl.Kind == fileformat.BlockKindRows {
			// Rows blocks validate the page container (header/directory CRC
			// and per-page bounds) without decompressing; full mode then walks
			// every page, verifying each record's page CRC and decodability.
			rc, err := s.loader.LoadRows(int64(bl.DataOffset), bl.BlockID)
			if err != nil {
				return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: err, Reason: err.Error()}
			}
			if mode == VerifyFull {
				verr := rc.ForEach(func(rec codec.PageRecord) error {
					rep.RowsChecked++
					if rec.ChangeType != fileformat.ChangeDelete {
						schema := st.schemas.schema(bl.SnapshotID, bl.TableID, rec.SchemaVersion)
						if schema != nil {
							if _, err := codec.DecodeBodyInto(nil, rec.Body, schema, s.opts.codecLimits(), nil); err != nil {
								return fmt.Errorf("row %d: %v", rec.RowID, err)
							}
						}
					}
					return nil
				})
				if verr != nil {
					// Preserve the underlying cause (e.g. ErrAuthFailed) so
					// errors.Is(ErrAuthFailed) works on the tamper path.
					return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: verr, Reason: verr.Error()}
				}
			}
			continue
		}
		blk, err := s.loader.Load(int64(bl.DataOffset), bl.BlockID)
		if err != nil {
			return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: err, Reason: err.Error()}
		}
		if mode == VerifyFull && bl.Kind == fileformat.BlockKindMetadata {
			if _, err := metadata.Parse(blk.Raw); err != nil {
				return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, Kind: ErrCorruptData, Reason: err.Error()}
			}
		}
	}
	rep.Duration = time.Since(start)
	return rep, nil
}
