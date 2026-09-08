package rowpack

import (
	"context"
	"fmt"
	"time"

	"github.com/rowpack/rowpack/internal/block"
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
		blk, err := s.loader.Load(int64(bl.DataOffset), bl.BlockID)
		if err != nil {
			return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Cause: err, Reason: err.Error()}
		}
		if mode == VerifyFull {
			switch bl.Kind {
			case fileformat.BlockKindRows:
				rp, err := block.ParseRowsPayload(blk.Raw, bl.ItemCount)
				if err != nil {
					return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, SnapshotID: bl.SnapshotID, TableID: bl.TableID, Kind: ErrCorruptData, Reason: err.Error()}
				}
				for i := range rp.Entries {
					rep.RowsChecked++
					if rp.Entries[i].ChangeType != fileformat.ChangeDelete {
						if got := fileformat.CRC32C(rp.RowBytes(i)); got != rp.RowCRC(i) {
							return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, TableID: bl.TableID, Kind: ErrCorruptData, Reason: fmt.Sprintf("row %d CRC mismatch", rp.Entries[i].RowID)}
						}
						// Decode against the schema when resolvable.
						schema := st.schemas.schema(bl.SnapshotID, bl.TableID, rp.Entries[i].SchemaVersion)
						if schema != nil {
							if _, err := decodeRowBytes(rp.RowBytes(i), schema, s.opts); err != nil {
								return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, TableID: bl.TableID, Kind: ErrCorruptData, Reason: fmt.Sprintf("row %d: %v", rp.Entries[i].RowID, err)}
							}
						}
					}
				}
			case fileformat.BlockKindMetadata:
				if _, err := parseMetadataPayload(blk.Raw); err != nil {
					return rep, &CorruptionError{File: s.dataPath, BlockID: bl.BlockID, Kind: ErrCorruptData, Reason: err.Error()}
				}
			}
		}
	}
	rep.Duration = time.Since(start)
	return rep, nil
}

func decodeRowBytes(b []byte, schema *codec.Schema, opts Options) (Row, error) {
	return codec.Decode(b, schema, opts.codecLimits())
}

func parseMetadataPayload(raw []byte) (*metadata.Payload, error) {
	return metadata.Parse(raw)
}
