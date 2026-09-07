package rowpack

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// committedSnapshot is one validated, committed snapshot in the data file.
type committedSnapshot struct {
	snapshotID uint64
	start      int64
	end        int64
	blockIDs   []uint64
	footerCRC  uint32
}

// recoveryReport records what the most recent open repaired.
type recoveryReport struct {
	performed        bool
	dataTailIgnored  uint64
	indexTailIgnored uint64
	snapshotsRebuilt uint64
}

// recover implements the open/recovery state machine:
//
//  1. The data file is scanned for all committed snapshots (authoritative).
//  2. The index is replayed; a tail that references invalid data or fails
//     validation is ignored (and truncated in read-write mode).
//  3. Committed data snapshots missing from the index are rebuilt from the
//     data file (persisted to .rpi in read-write mode).
//  4. A data tail without a valid footer is truncated (read-write) or
//     reported (read-only).
//
// Mid-file corruption (a broken region between two valid commit points) is a
// hard error and never skipped.
func (s *Store) recover() error {
	report := recoveryReport{}
	defer func() {
		s.recoveryStats.Store(report)
	}()

	// 1. Scan data for committed snapshots and detect the recoverable tail.
	committed, tailStart, err := s.scanDataFile()
	if err != nil {
		return err
	}
	var lastSnapshot, maxBlock uint64
	for _, c := range committed {
		if c.snapshotID > lastSnapshot {
			lastSnapshot = c.snapshotID
		}
		for _, b := range c.blockIDs {
			if b > maxBlock {
				maxBlock = b
			}
		}
	}
	committedByID := make(map[uint64]committedSnapshot, len(committed))
	for _, c := range committed {
		committedByID[c.snapshotID] = c
	}

	// 2. Replay the index.
	idxData, err := s.index.ReadAll()
	if err != nil {
		return err
	}
	res, err := index.Replay(idxData, fileformat.IndexFileHeaderSize, s.opts.Limits.MaxSnapshotDepth, &dataFooterVerifier{store: s})
	if err != nil {
		return fmt.Errorf("rowpack: index replay: %w", err)
	}
	// Seed the txn sequence before any rebuild append, so rebuilt txns get
	// strictly increasing sequences that replay accepts.
	s.txnSeq.Store(res.LastSeq)
	view := res.View
	if res.TailIgnored > 0 {
		report.performed = true
		report.indexTailIgnored = uint64(res.TailIgnored)
		if !s.readOnly {
			// Truncate the invalid index tail.
			if err := s.index.Truncate(res.LastOffset); err != nil {
				return fmt.Errorf("rowpack: truncate index tail: %w", err)
			}
		}
	}

	// 3. Rebuild committed data snapshots missing from the index.
	// Process in SnapshotID order so parent chains build correctly.
	for _, c := range committed {
		if view.Snapshot(c.snapshotID) != nil {
			continue
		}
		txn, err := s.buildIndexTxnFromData(&c)
		if err != nil {
			return fmt.Errorf("rowpack: rebuild snapshot %d: %w", c.snapshotID, err)
		}
		nv, err := view.Apply(txn, s.opts.Limits.MaxSnapshotDepth)
		if err != nil {
			return fmt.Errorf("rowpack: apply rebuilt snapshot %d: %w", c.snapshotID, err)
		}
		view = nv
		report.performed = true
		report.snapshotsRebuilt++
		if !s.readOnly {
			txnBytes := marshalTxn(txn, s.txnSeq.Add(1), s.index.Offset())
			if _, err := s.index.Append(txnBytes); err != nil {
				return fmt.Errorf("rowpack: append rebuilt index: %w", err)
			}
		}
	}
	if !s.readOnly && report.snapshotsRebuilt > 0 {
		if err := s.index.Sync(); err != nil {
			return err
		}
	}

	// 4. Handle the data tail.
	dataSize, err := s.data.Size()
	if err != nil {
		return err
	}
	if tailStart < dataSize {
		report.performed = true
		report.dataTailIgnored = uint64(dataSize - tailStart)
		if !s.readOnly {
			if err := s.data.Truncate(tailStart); err != nil {
				return fmt.Errorf("rowpack: truncate data tail: %w", err)
			}
		}
	}

	s.lastSnapshotID.Store(lastSnapshot)
	s.lastBlockID.Store(maxBlock)

	schemas, err := s.buildSchemaIndex(view)
	if err != nil {
		return err
	}
	s.state.Store(&publishedState{view: view, schemas: schemas})
	return nil
}

// scanDataFile walks the .rpk from after the header, collecting committed
// snapshots and the start offset of any recoverable tail. A broken region
// between two valid commit points is reported as mid-file corruption.
func (s *Store) scanDataFile() ([]committedSnapshot, int64, error) {
	size, err := s.data.Size()
	if err != nil {
		return nil, 0, err
	}
	if size < fileformat.DataFileHeaderSize {
		return nil, 0, fmt.Errorf("rowpack: data file %d bytes too small", size)
	}
	var out []committedSnapshot
	pos := int64(fileformat.DataFileHeaderSize)
	for pos < size {
		if size-pos < fileformat.SnapshotHeaderSize {
			break // trailing partial header
		}
		var shBuf [fileformat.SnapshotHeaderSize]byte
		if _, err := s.data.ReadAt(shBuf[:], pos); err != nil {
			return nil, 0, err
		}
		var sh fileformat.SnapshotHeader
		if err := sh.Unmarshal(shBuf[:]); err != nil {
			break // not a valid snapshot header: recoverable tail
		}
		snapStart := pos
		var blockIDs []uint64
		cur := snapStart + fileformat.SnapshotHeaderSize
		foundFooter := false
		for {
			if size-cur < fileformat.BlockHeaderSize {
				break // snapshot truncated: recoverable at snapStart
			}
			probeLen := int(size - cur)
			if probeLen > fileformat.SnapshotFooterSize {
				probeLen = fileformat.SnapshotFooterSize
			}
			probe := make([]byte, probeLen)
			if _, err := s.data.ReadAt(probe, cur); err != nil {
				return nil, 0, err
			}
			if string(probe[0:8]) == fileformat.MagicSnapshotFtr {
				if probeLen < fileformat.SnapshotFooterSize {
					break // truncated footer
				}
				var ftr fileformat.SnapshotFooter
				if err := ftr.Unmarshal(probe[:fileformat.SnapshotFooterSize]); err != nil {
					break // interrupted footer write: recoverable at snapStart
				}
				if ftr.SnapshotID != sh.SnapshotID {
					return nil, 0, fmt.Errorf("rowpack: mid-file corruption: footer snapshot %d != header snapshot %d at %d", ftr.SnapshotID, sh.SnapshotID, cur)
				}
				snapEnd := cur + fileformat.SnapshotFooterSize
				out = append(out, committedSnapshot{snapshotID: sh.SnapshotID, start: snapStart, end: snapEnd, blockIDs: blockIDs, footerCRC: footerCRCValue(probe[:fileformat.SnapshotFooterSize])})
				pos = snapEnd
				foundFooter = true
				break
			}
			var bh fileformat.BlockHeader
			if err := bh.Unmarshal(probe[:fileformat.BlockHeaderSize]); err != nil {
				// Neither a footer nor a valid block header. If this is a
				// valid snapshot header, the current snapshot was truncated;
				// otherwise check whether a committed snapshot exists later.
				if probeLen >= fileformat.SnapshotHeaderSize {
					var nsh fileformat.SnapshotHeader
					if err := nsh.Unmarshal(probe[:fileformat.SnapshotHeaderSize]); err == nil {
						break // truncated snapshot at snapStart
					}
				}
				if hasLaterValidSnapshot(s.data, cur, size) {
					return nil, 0, fmt.Errorf("rowpack: mid-file corruption at offset %d", cur)
				}
				break // garbage tail
			}
			payload := int64(bh.StoredSize)
			if cur+fileformat.BlockHeaderSize+payload > size {
				break // truncated block: recoverable at snapStart
			}
			blockIDs = append(blockIDs, bh.BlockID)
			cur += fileformat.BlockHeaderSize + payload
		}
		if !foundFooter {
			// The current snapshot is incomplete. If a valid commit exists
			// after the break point, this is mid-file corruption.
			if hasLaterValidSnapshot(s.data, cur, size) {
				return nil, 0, fmt.Errorf("rowpack: mid-file corruption at offset %d", cur)
			}
			break // recoverable tail starting at snapStart
		}
	}
	return out, pos, nil
}

// hasLaterValidSnapshot reports whether a fully valid snapshot (header +
// blocks + footer) exists at or after offset. Used to distinguish recoverable
// tail truncation from mid-file corruption.
func hasLaterValidSnapshot(ra interface {
	ReadAt([]byte, int64) (int, error)
}, from, size int64) bool {
	pos := from
	for pos+fileformat.SnapshotHeaderSize <= size {
		var shBuf [fileformat.SnapshotHeaderSize]byte
		if _, err := ra.ReadAt(shBuf[:], pos); err != nil {
			return false
		}
		var sh fileformat.SnapshotHeader
		if err := sh.Unmarshal(shBuf[:]); err != nil {
			pos += 8 // magic is 8 bytes; step to find the next header
			continue
		}
		// Found a header; walk to see if it completes.
		cur := pos + fileformat.SnapshotHeaderSize
		for cur+fileformat.BlockHeaderSize <= size {
			var probe [fileformat.SnapshotFooterSize]byte
			n, err := ra.ReadAt(probe[:], cur)
			if err != nil && n < fileformat.BlockHeaderSize {
				break
			}
			if string(probe[0:8]) == fileformat.MagicSnapshotFtr {
				var ftr fileformat.SnapshotFooter
				if ftr.Unmarshal(probe[:]) == nil && ftr.SnapshotID == sh.SnapshotID {
					return true
				}
				return false
			}
			var bh fileformat.BlockHeader
			if err := bh.Unmarshal(probe[:fileformat.BlockHeaderSize]); err != nil {
				return false
			}
			cur += fileformat.BlockHeaderSize + int64(bh.StoredSize)
		}
		return false
	}
	return false
}

// buildIndexTxnFromData reconstructs the index transaction for a committed
// data snapshot that is missing from the index, by reading and parsing its
// blocks.
func (s *Store) buildIndexTxnFromData(c *committedSnapshot) (*index.Txn, error) {
	var shBuf [fileformat.SnapshotHeaderSize]byte
	if _, err := s.data.ReadAt(shBuf[:], c.start); err != nil {
		return nil, err
	}
	var sh fileformat.SnapshotHeader
	if err := sh.Unmarshal(shBuf[:]); err != nil {
		return nil, err
	}
	var blockEntries []fileformat.BlockIndexEntry
	var metaEntries []fileformat.MetadataIndexEntry
	var rowEntries []fileformat.RowIndexEntry
	var rowCount uint64

	cur := c.start + fileformat.SnapshotHeaderSize
	for cur < c.end-fileformat.SnapshotFooterSize {
		var bhBuf [fileformat.BlockHeaderSize]byte
		if _, err := s.data.ReadAt(bhBuf[:], cur); err != nil {
			return nil, err
		}
		var bh fileformat.BlockHeader
		if err := bh.Unmarshal(bhBuf[:]); err != nil {
			return nil, err
		}
		if string(bhBuf[0:8]) == fileformat.MagicSnapshotFtr {
			break
		}
		blk, err := s.loader.Load(cur, bh.BlockID)
		if err != nil {
			return nil, err
		}
		blockEntries = append(blockEntries, fileformat.BlockIndexEntry{
			BlockID:     bh.BlockID,
			SnapshotID:  bh.SnapshotID,
			TableID:     bh.TableID,
			BlockKind:   bh.BlockKind,
			Compression: bh.Compression,
			DataOffset:  uint64(cur),
			RawSize:     bh.RawSize,
			StoredSize:  bh.StoredSize,
			ItemCount:   bh.ItemCount,
			RawCRC32C:   bh.RawCRC32C,
		})
		switch bh.BlockKind {
		case fileformat.BlockKindRows:
			// The lightweight directory view carries RowID/ChangeType per
			// record, which is all the index needs; the full payload parse
			// would materialize every record slice for nothing.
			rp, err := block.ParseRowsDirectory(blk.Raw, bh.ItemCount, nil)
			if err != nil {
				return nil, err
			}
			for i := range rp.Entries {
				rowEntries = append(rowEntries, fileformat.RowIndexEntry{
					SnapshotID: bh.SnapshotID, TableID: bh.TableID,
					ChangeType: rp.Entries[i].ChangeType, RowID: rp.Entries[i].RowID,
					BlockID: bh.BlockID, ItemOrdinal: uint32(i),
				})
				rowCount++
			}
		case fileformat.BlockKindMetadata:
			mp, err := metadata.Parse(blk.Raw)
			if err != nil {
				return nil, err
			}
			for i := range mp.Entries {
				metaEntries = append(metaEntries, fileformat.MetadataIndexEntry{
					SnapshotID: bh.SnapshotID, ObjectID: mp.Entries[i].ObjectID,
					Revision: mp.Entries[i].Revision, RecordType: mp.Entries[i].RecordType,
					BlockID: bh.BlockID, ItemOrdinal: uint32(i),
					Operation: mp.Entries[i].Operation, Critical: mp.Entries[i].Critical,
				})
			}
		}
		cur += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}

	builder := index.NewBuilder(0) // sequence filled by caller
	builder.Reserve(len(metaEntries), len(blockEntries), len(rowEntries))
	snapEntry := fileformat.SnapshotIndexEntry{
		SnapshotID:       sh.SnapshotID,
		ParentSnapshotID: sh.ParentSnapshotID,
		SnapshotType:     sh.SnapshotType,
		BlockCount:       uint32(len(blockEntries)),
		RowRecordCount:   rowCount,
		CreatedUnixNano:  sh.CreatedUnixNano,
		DataStart:        uint64(c.start),
		DataEnd:          uint64(c.end),
		DataFooterCRC32C: c.footerCRC,
	}
	if err := builder.SetSnapshot(snapEntry); err != nil {
		return nil, err
	}
	for i := range blockEntries {
		if err := builder.AddBlock(blockEntries[i]); err != nil {
			return nil, err
		}
	}
	for i := range metaEntries {
		if err := builder.AddMetadata(metaEntries[i]); err != nil {
			return nil, err
		}
	}
	for i := range rowEntries {
		if err := builder.AddRow(rowEntries[i]); err != nil {
			return nil, err
		}
	}
	_, txn, err := builder.Build(uint64(c.start), uint64(c.end), c.footerCRC, 0, 0)
	if err != nil {
		return nil, err
	}
	return txn, nil
}

// marshalTxn serializes a rebuilt txn with the given sequence and offsets.
// TxnStartOffset/TxnEndOffset are informational (replay derives positions by
// walking). The end offset is computed from the header's BodyBytes (the
// serialized length is independent of the offset fields), so a single Build
// suffices.
func marshalTxn(txn *index.Txn, seq uint64, start int64) []byte {
	b := index.NewBuilder(seq)
	b.Reserve(len(txn.Metadata), len(txn.Blocks), len(txn.Rows))
	se := txn.Snapshot
	_ = b.SetSnapshot(se)
	for i := range txn.Blocks {
		_ = b.AddBlock(txn.Blocks[i])
	}
	for i := range txn.Metadata {
		_ = b.AddMetadata(txn.Metadata[i])
	}
	for i := range txn.Rows {
		_ = b.AddRow(txn.Rows[i])
	}
	end := start + int64(fileformat.IndexTxnHeaderSize+txn.Header.BodyBytes) + fileformat.IndexTxnFooterSize
	out, _, err := b.Build(se.DataStart, se.DataEnd, se.DataFooterCRC32C, start, end)
	if err != nil {
		panic(err)
	}
	return out
}
