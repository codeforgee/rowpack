package rowpack

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// committedSnapshot is one validated, committed snapshot in the single data
// file. The structure offsets come from the SnapshotFooter (BINARY_FORMAT_V2
// §7), so an IndexTxn rebuild never needs the (possibly corrupt) IndexTxn
// itself.
type committedSnapshot struct {
	snapshotID  uint64
	start       int64 // SnapshotHeader offset
	end         int64 // offset just after SnapshotFooter
	footerOff   int64 // SnapshotFooter offset
	blocksStart int64
	blocksEnd   int64
	txnStart    int64
	txnEnd      int64
	prevFooter  uint64 // PreviousFooterOffset as recorded by the footer
	ftrTxnCRC   uint32 // footer IndexTxnCRC32C (over stored txn bytes)
	footerCRC   uint32 // footer FooterCRC32C
	footerBytes []byte
	blockIDs    []uint64
}

// recoveryReport records what the most recent open repaired.
type recoveryReport struct {
	performed        bool
	dataTailIgnored  uint64
	indexTailIgnored uint64 // stored bytes of IndexTxns rebuilt in memory
	snapshotsRebuilt uint64 // snapshots whose IndexTxn was rebuilt in memory
}

// recover implements the single-file open/recovery state machine:
//
//  1. The file is scanned from the header, walking the four fixed structures
//     (SnapshotHeader / BlockHeader / IndexTxn / SnapshotFooter). All
//     committed snapshots are collected; the region after the last valid
//     footer is an uncommitted tail.
//  2. Each committed snapshot's IndexTxn is read from its footer-recorded
//     range and replayed into the view independently. A snapshot whose txn
//     fails validation (length, footer-bound CRC, parse, or apply) is rebuilt
//     from its own blocks in memory (R2: mid-file IndexTxn corruption never
//     stops later snapshots; a duplicate/inconsistent apply after rebuild is
//     mid-file corruption).
//  3. Any committed data snapshot that cannot be rebuilt (broken block
//     headers, authentication, decompression or CRC) is a hard error
//     (BINARY_FORMAT_V2 §10.3).
//  4. The uncommitted tail is truncated (read-write) or reported (read-only).
func (s *Store) recover() error {
	report := recoveryReport{}
	defer func() {
		s.recoveryStats.Store(report)
	}()

	// 1. Scan for committed snapshots and the recoverable tail.
	committed, tailStart, err := s.scanDataFile()
	if err != nil {
		return err
	}
	var lastSnapshot, maxBlock, lastSeq uint64
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

	// 2. Per-snapshot IndexTxn replay with in-memory rebuild fallback.
	view := index.EmptyView()
	for _, c := range committed {
		txn, seq, ok, err := s.readIndexTxn(&c)
		if err != nil {
			return err
		}
		if ok {
			if seq > lastSeq {
				lastSeq = seq
			}
			nv, aerr := view.Apply(txn, s.opts.Limits.MaxSnapshotDepth)
			if aerr == nil {
				view = nv
				continue
			}
			// A valid-looking txn that does not apply (e.g. parent missing):
			// fall through to the rebuild path; a rebuilt txn that also fails
			// to apply is mid-file corruption.
		}
		// IndexTxn missing/corrupt: rebuild from this snapshot's blocks.
		rtxn, err := s.buildIndexTxnFromData(&c)
		if err != nil {
			return fmt.Errorf("rowpack: rebuild snapshot %d: %w", c.snapshotID, err)
		}
		nv, err := view.Apply(rtxn, s.opts.Limits.MaxSnapshotDepth)
		if err != nil {
			return fmt.Errorf("rowpack: snapshot %d chain invalid after rebuild: %w", c.snapshotID, err)
		}
		view = nv
		report.performed = true
		report.snapshotsRebuilt++
		report.indexTailIgnored += uint64(c.txnEnd - c.txnStart)
	}
	s.txnSeq.Store(lastSeq)

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
				return fmt.Errorf("rowpack: truncate tail: %w", err)
			}
		}
	}

	s.lastSnapshotID.Store(lastSnapshot)
	s.lastBlockID.Store(maxBlock)
	if len(committed) > 0 {
		s.lastFooterOffset = uint64(committed[len(committed)-1].footerOff)
	}

	schemas, err := s.buildSchemaIndex(view)
	if err != nil {
		return err
	}
	s.state.Store(&publishedState{view: view, schemas: schemas})
	return nil
}

// readIndexTxn reads and validates one committed snapshot's IndexTxn range.
// It returns ok=false when the txn is missing, length-inconsistent with the
// footer range, fails the footer-bound IndexTxnCRC32C check, or fails
// parsing; the caller then rebuilds from blocks. A range that cannot even be
// read (I/O error) is a hard error.
func (s *Store) readIndexTxn(c *committedSnapshot) (txn *index.Txn, seq uint64, ok bool, err error) {
	span := c.txnEnd - c.txnStart
	if span <= 0 || span > int64(^uint32(0)) {
		return nil, 0, false, nil // implausible range: rebuild
	}
	// The footer binds the STORED bytes (ciphertext when encrypted), so a
	// torn or bit-rotted txn is detected before any key is needed (R12).
	buf := make([]byte, span)
	if _, rerr := s.data.ReadAt(buf, c.txnStart); rerr != nil {
		return nil, 0, false, fmt.Errorf("rowpack: read IndexTxn of snapshot %d: %w", c.snapshotID, rerr)
	}
	if fileformat.CRC32C(buf) != c.ftrTxnCRC {
		return nil, 0, false, nil
	}
	var crypto *index.ChunkCrypto
	var h fileformat.IndexTxnHeader
	if s.header.EncryptionAlgorithm != fileformat.EncNone {
		if herr := h.Unmarshal(buf); herr != nil {
			return nil, 0, false, nil // unreadable header: rebuild
		}
		epoch := fileformat.IndexTxnHeaderKeyEpoch(buf)
		crypto = &index.ChunkCrypto{
			TxnSequence: h.TxnSequence,
			SnapshotID:  h.SnapshotID,
			Epoch:       epoch,
			Open: func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error) {
				return s.decrypter.OpenIndexChunk(epoch, h.TxnSequence, h.SnapshotID, chunkSeq, kind, firstOrdinal, uint32(rawBytes), uint32(len(stored)), stored)
			},
		}
	}
	txn, perr := index.ParseTxnChunked(buf, crypto)
	if perr != nil {
		// Per-chunk authentication/parse failure means the txn is corrupt
		// (its stored extent already passed the footer CRC): rebuild in
		// memory (R2).
		return nil, 0, false, nil
	}
	return txn, txn.Header.TxnSequence, true, nil
}

// scanDataFile walks the single file from after the header, collecting
// committed snapshots and the start offset of any recoverable tail. It
// recognizes the four fixed structures (BINARY_FORMAT_V2 §10.1): Snapshot
// Header, Block Header, IndexTxn and Snapshot Footer. A broken region between
// two valid commit points is reported as mid-file corruption. The mid/tail
// discriminator is the presence of a later VALID SNAPSHOT FOOTER (the commit
// authority, R3), NOT a SnapshotHeader: an IndexTxn header whose magic is
// bit-rotted must not demote a committed snapshot to an uncommitted tail.
func (s *Store) scanDataFile() ([]committedSnapshot, int64, error) {
	size, err := s.data.Size()
	if err != nil {
		return nil, 0, err
	}
	if size < fileformat.DataFileHeaderSize {
		return nil, 0, fmt.Errorf("rowpack: store file %d bytes too small", size)
	}
	var out []committedSnapshot
	pos := int64(fileformat.DataFileHeaderSize)
	for pos < size {
		c, complete, next, err := s.walkSnapshot(pos)
		if err != nil {
			return nil, 0, err
		}
		if complete {
			out = append(out, c)
			pos = next
			continue
		}
		// The snapshot at pos broke: a valid Footer later means mid-file
		// corruption; otherwise this is the recoverable tail.
		if s.hasLaterValidFooter(pos, size) {
			return nil, 0, fmt.Errorf("rowpack: mid-file corruption at offset %d", pos)
		}
		break
	}
	return out, pos, nil
}

// walkSnapshot attempts to walk one complete snapshot starting at a valid
// SnapshotHeader position. It returns complete=false when the snapshot is
// truncated or structurally broken (the caller distinguishes tail vs mid-file
// corruption). Structural inconsistencies between the header and a later
// committed footer (IDs disagreeing) are reported as an error here.
func (s *Store) walkSnapshot(start int64) (c committedSnapshot, complete bool, next int64, err error) {
	size, err := s.data.Size()
	if err != nil {
		return c, false, 0, err
	}
	var sh [fileformat.SnapshotHeaderSize]byte
	if size-start < fileformat.SnapshotHeaderSize {
		return c, false, 0, nil
	}
	if _, err := s.data.ReadAt(sh[:], start); err != nil {
		return c, false, 0, err
	}
	var hdr fileformat.SnapshotHeader
	if err := hdr.Unmarshal(sh[:]); err != nil {
		return c, false, 0, nil // not a valid header: tail
	}
	c.snapshotID = hdr.SnapshotID
	c.start = start
	c.blocksStart = start + fileformat.SnapshotHeaderSize
	cur := c.blocksStart
	for cur < size {
		if size-cur < 8 {
			return c, false, 0, nil // truncated
		}
		var magic [8]byte
		if _, err := s.data.ReadAt(magic[:], cur); err != nil {
			return c, false, 0, err
		}
		switch string(magic[:]) {
		case fileformat.MagicBlockHdr:
			if size-cur < fileformat.BlockHeaderSize {
				return c, false, 0, nil
			}
			var bhBuf [fileformat.BlockHeaderSize]byte
			if _, err := s.data.ReadAt(bhBuf[:], cur); err != nil {
				return c, false, 0, err
			}
			var bh fileformat.BlockHeader
			if err := bh.Unmarshal(bhBuf[:]); err != nil {
				return c, false, 0, nil // broken block header: break
			}
			if cur+fileformat.BlockHeaderSize+int64(bh.StoredSize) > size {
				return c, false, 0, nil // truncated payload
			}
			c.blockIDs = append(c.blockIDs, bh.BlockID)
			cur += fileformat.BlockHeaderSize + int64(bh.StoredSize)
		case fileformat.MagicIndexTxnHdr:
			if size-cur < fileformat.IndexTxnHeaderSize {
				return c, false, 0, nil
			}
			var thBuf [fileformat.IndexTxnHeaderSize]byte
			if _, err := s.data.ReadAt(thBuf[:], cur); err != nil {
				return c, false, 0, err
			}
			var th fileformat.IndexTxnHeader
			if err := th.Unmarshal(thBuf[:]); err != nil {
				return c, false, 0, nil
			}
			body := int64(th.BodyBytes)
			if body < 0 || cur+fileformat.IndexTxnHeaderSize+body+fileformat.IndexTxnFooterSize > size {
				return c, false, 0, nil // truncated txn
			}
			ftrOff := cur + fileformat.IndexTxnHeaderSize + body
			if size-ftrOff < fileformat.IndexTxnFooterSize {
				return c, false, 0, nil
			}
			var tfBuf [fileformat.IndexTxnFooterSize]byte
			if _, err := s.data.ReadAt(tfBuf[:], ftrOff); err != nil {
				return c, false, 0, err
			}
			var tf fileformat.IndexTxnFooter
			if err := tf.Unmarshal(tfBuf[:]); err != nil {
				return c, false, 0, nil
			}
			c.txnStart = cur
			c.txnEnd = ftrOff + fileformat.IndexTxnFooterSize
			cur = c.txnEnd
		case fileformat.MagicSnapshotFtr:
			if size-cur < fileformat.SnapshotFooterSize {
				return c, false, 0, nil // interrupted footer write
			}
			var fb [fileformat.SnapshotFooterSize]byte
			if _, err := s.data.ReadAt(fb[:], cur); err != nil {
				return c, false, 0, err
			}
			var ftr fileformat.SnapshotFooter
			if err := ftr.Unmarshal(fb[:]); err != nil {
				return c, false, 0, nil
			}
			if ftr.SnapshotID != c.snapshotID {
				return c, false, 0, fmt.Errorf("rowpack: mid-file corruption: footer snapshot %d != header snapshot %d at %d", ftr.SnapshotID, c.snapshotID, cur)
			}
			// Footer range disambiguation: an empty txn (no blocks) sees the
			// IndexTxnHeader immediately after the SnapshotHeader, so
			// c.txnStart is set while c.blocksEnd is still the header end.
			blocksEnd := c.txnStart
			if c.txnStart == 0 {
				blocksEnd = cur
			}
			c.blocksEnd = blocksEnd
			c.footerOff = cur
			c.end = cur + fileformat.SnapshotFooterSize
			c.prevFooter = ftr.PreviousFooterOffset
			c.ftrTxnCRC = ftr.IndexTxnCRC32C
			c.footerCRC = footerCRCValue(fb[:])
			c.footerBytes = append([]byte(nil), fb[:]...)
			return c, true, c.end, nil
		default:
			return c, false, 0, nil // unknown structure: break
		}
	}
	return c, false, 0, nil // ran out of file without a footer
}

// hasLaterValidFooter reports whether a valid SnapshotFooter exists at or
// after offset. Footer positions are NOT 8-aligned (block payload lengths are
// arbitrary), so this walks byte-by-byte; it runs only on the corrupt/tail
// path, and a false positive requires an 8-byte magic collision plus a
// passing CRC-32C over 144 bytes (~2^-32 per candidate), which is
// negligible. The footer is the commit authority: its presence means earlier
// bytes in this region are mid-file corruption, never an uncommitted tail
// (R3).
func (s *Store) hasLaterValidFooter(from, size int64) bool {
	for p := from; p+fileformat.SnapshotFooterSize <= size; p++ {
		var magic [8]byte
		if _, err := s.data.ReadAt(magic[:], p); err != nil {
			return false
		}
		if string(magic[:]) != fileformat.MagicSnapshotFtr {
			continue
		}
		var fb [fileformat.SnapshotFooterSize]byte
		if _, err := s.data.ReadAt(fb[:], p); err != nil {
			return false
		}
		var ftr fileformat.SnapshotFooter
		if err := ftr.Unmarshal(fb[:]); err != nil {
			continue
		}
		if ftr.SnapshotID == 0 {
			continue
		}
		return true
	}
	return false
}

// buildIndexTxnFromData reconstructs the index transaction for a committed
// snapshot whose IndexTxn is missing or corrupt, by reading and parsing its
// blocks in [BlocksStartOffset, BlocksEndOffset).
func (s *Store) buildIndexTxnFromData(c *committedSnapshot) (*index.Txn, error) {
	var blockEntries []fileformat.BlockIndexEntry
	var metaEntries []fileformat.MetadataIndexEntry
	var rowEntries []fileformat.RowIndexEntry
	var rowCount uint64

	cur := c.blocksStart
	for cur < c.blocksEnd {
		var bhBuf [fileformat.BlockHeaderSize]byte
		if _, err := s.data.ReadAt(bhBuf[:], cur); err != nil {
			return nil, err
		}
		var bh fileformat.BlockHeader
		if err := bh.Unmarshal(bhBuf[:]); err != nil {
			return nil, fmt.Errorf("rowpack: block header at %d: %w", cur, err)
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

	var shBuf [fileformat.SnapshotHeaderSize]byte
	if _, err := s.data.ReadAt(shBuf[:], c.start); err != nil {
		return nil, err
	}
	var sh fileformat.SnapshotHeader
	if err := sh.Unmarshal(shBuf[:]); err != nil {
		return nil, err
	}

	builder := index.NewBuilder(0) // sequence filled by the writer on commit
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
	_, txn, err := builder.Build(uint64(c.start), uint64(c.end), c.footerCRC, c.txnStart, c.txnEnd)
	if err != nil {
		return nil, err
	}
	return txn, nil
}
