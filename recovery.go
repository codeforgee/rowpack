package rowpack

import (
	"encoding/binary"
	"fmt"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/metadata"
	"github.com/codeforgee/rowpack/internal/seal"
)

// committedSnapshot is one validated, committed snapshot in the single data
// file. The structure offsets come from the SnapshotFooter (BINARY_FORMAT_V1
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
	blockCount  uint32
	metaCount   uint32
	rowCount    uint64
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
//     from its own blocks in memory (mid-file IndexTxn corruption never
//     stops later snapshots; a duplicate/inconsistent apply after rebuild is
//     mid-file corruption).
//  3. Any committed data snapshot that cannot be rebuilt (broken block
//     headers, authentication, decompression or CRC) is a hard error
//     (BINARY_FORMAT_V1 §10.3).
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
		data, crypto, seq, ok, err := s.readIndexTxn(&c)
		if err != nil {
			return err
		}
		if ok {
			if seq > lastSeq {
				lastSeq = seq
			}
			// Streaming apply: entries decode straight into the new view's
			// shards (no []RowIndexEntry intermediate). A parse/apply failure
			// means the txn is corrupt or inconsistent: fall through to the
			// rebuild path.
			nv, aerr := view.ApplyStreaming(data, crypto, s.opts.Limits.MaxSnapshotDepth)
			if aerr == nil {
				view = nv
				continue
			}
		}
		// IndexTxn missing/corrupt: rebuild from this snapshot's blocks. The
		// rebuilt txn is applied eagerly (its row shards are materialized) — in
		// The rebuilt txn is applied eagerly from the snapshot's data pages.
		rtxn, err := s.rebuildIndex(&c)
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
	dataSize := s.data.Size()
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
	var maxTableID uint32
	var maxObjectID uint64
	for _, sm := range view.Snapshots() {
		for _, oid := range view.MetadataObjects(sm.ID) {
			if oid > maxObjectID {
				maxObjectID = oid
			}
		}
		for _, oid := range view.MetadataByType(sm.ID, uint32(format.RecordTable)) {
			if tid, err := metadata.TableID(oid); err == nil && tid > maxTableID {
				maxTableID = tid
			}
		}
	}
	s.maxTableID.Store(maxTableID)
	s.maxObjectID.Store(maxObjectID)
	if len(committed) > 0 {
		s.lastFooterOffset = uint64(committed[len(committed)-1].footerOff)
	}

	schemas, err := s.buildIndex(view)
	if err != nil {
		return err
	}
	s.state.Store(&publishedState{view: view, schemas: schemas})
	return nil
}

// readIndexTxn reads and validates one committed snapshot's IndexTxn range.
// It returns the raw stored bytes (footer-CRC verified) plus the chunk-crypto
// context; no entries are decoded here. It returns ok=false when the txn is
// missing, length-inconsistent with the footer range, fails the footer-bound
// IndexTxnCRC32C check, or fails to parse; the caller then rebuilds from
// blocks. A range that cannot even be read (I/O error) is a hard error.
//
// S1: decoding is fused with application via View.ApplyStreaming, so the
// per-row ~40 B intermediate slice never exists on the Open path.
func (s *Store) readIndexTxn(c *committedSnapshot) (data []byte, crypto *index.ChunkCrypto, seq uint64, ok bool, err error) {
	span := c.txnEnd - c.txnStart
	if span <= 0 || span > int64(^uint32(0)) {
		return nil, nil, 0, false, nil // implausible range: rebuild
	}
	// The footer binds the STORED bytes (ciphertext when encrypted), so a
	// torn or bit-rotted txn is detected before any key is needed.
	buf := make([]byte, span)
	if _, rerr := s.data.ReadAt(buf, c.txnStart); rerr != nil {
		return nil, nil, 0, false, fmt.Errorf("rowpack: read IndexTxn of snapshot %d: %w", c.snapshotID, rerr)
	}
	if format.CRC32C(buf) != c.ftrTxnCRC {
		return nil, nil, 0, false, nil
	}
	var h format.IndexTxnHeader
	if herr := h.Unmarshal(buf); herr != nil {
		return nil, nil, 0, false, nil // unreadable header: rebuild
	}
	if s.header.EncryptionAlgorithm != format.EncNone {
		epoch := format.IndexTxnHeaderKeyEpoch(buf)
		crypto = &index.ChunkCrypto{
			TxnSequence: h.TxnSequence,
			SnapshotID:  h.SnapshotID,
			Epoch:       epoch,
			Open: func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error) {
				return s.decrypter.OpenIndexChunk(seal.ChunkContext{
					TxnSequence:   h.TxnSequence,
					SnapshotID:    h.SnapshotID,
					ChunkSequence: chunkSeq,
					FirstOrdinal:  firstOrdinal,
					RawBytes:      uint32(rawBytes),
					StoredBytes:   uint32(len(stored)),
					Kind:          kind,
					Epoch:         epoch,
				}, stored)
			},
		}
	}
	return buf, crypto, h.TxnSequence, true, nil
}

// scanDataFile walks the single file from after the header, collecting
// committed snapshots and the start offset of any recoverable tail. It
// recognizes the four fixed structures (BINARY_FORMAT_V1 §10.1): Snapshot
// Header, Block Header, IndexTxn and Snapshot Footer. A broken region between
// two valid commit points is reported as mid-file corruption. The mid/tail
// discriminator is the presence of a later VALID SNAPSHOT FOOTER (the commit
// authority), NOT a SnapshotHeader: an IndexTxn header whose magic is
// bit-rotted must not demote a committed snapshot to an uncommitted tail.
func (s *Store) scanDataFile() ([]committedSnapshot, int64, error) {
	// The fixed data header was validated at open, so size >= header size
	// always holds here.
	size := s.data.Size()
	var out []committedSnapshot
	pos := int64(format.DataFileHeaderSize)
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
		if s.hasValidFooterAfter(pos, size) {
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
	size := s.data.Size()
	var sh [format.SnapshotHeaderSize]byte
	if size-start < format.SnapshotHeaderSize {
		return c, false, 0, nil
	}
	if _, err := s.data.ReadAt(sh[:], start); err != nil {
		return c, false, 0, err
	}
	var hdr format.SnapshotHeader
	if err := hdr.Unmarshal(sh[:]); err != nil {
		return c, false, 0, nil // not a valid header: tail
	}
	c.snapshotID = hdr.SnapshotID
	c.start = start
	c.blocksStart = start + format.SnapshotHeaderSize
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
		case format.MagicBlockHdr:
			if size-cur < format.BlockHeaderSize {
				return c, false, 0, nil
			}
			var bhBuf [format.BlockHeaderSize]byte
			if _, err := s.data.ReadAt(bhBuf[:], cur); err != nil {
				return c, false, 0, err
			}
			var bh format.BlockHeader
			if err := bh.Unmarshal(bhBuf[:]); err != nil {
				return c, false, 0, nil // broken block header: break
			}
			if cur+format.BlockHeaderSize+int64(bh.StoredSize) > size {
				return c, false, 0, nil // truncated payload
			}
			c.blockIDs = append(c.blockIDs, bh.BlockID)
			cur += format.BlockHeaderSize + int64(bh.StoredSize)
		case format.MagicIndexTxnHdr:
			if size-cur < format.IndexTxnHeaderSize {
				return c, false, 0, nil
			}
			var thBuf [format.IndexTxnHeaderSize]byte
			if _, err := s.data.ReadAt(thBuf[:], cur); err != nil {
				return c, false, 0, err
			}
			var th format.IndexTxnHeader
			if err := th.Unmarshal(thBuf[:]); err != nil {
				return c, false, 0, nil
			}
			body := int64(th.BodyBytes)
			if body < 0 || cur+format.IndexTxnHeaderSize+body+format.IndexTxnFooterSize > size {
				return c, false, 0, nil // truncated txn
			}
			ftrOff := cur + format.IndexTxnHeaderSize + body
			var tfBuf [format.IndexTxnFooterSize]byte
			if _, err := s.data.ReadAt(tfBuf[:], ftrOff); err != nil {
				return c, false, 0, err
			}
			var tf format.IndexTxnFooter
			if err := tf.Unmarshal(tfBuf[:]); err != nil {
				return c, false, 0, nil
			}
			c.txnStart = cur
			c.txnEnd = ftrOff + format.IndexTxnFooterSize
			cur = c.txnEnd
		case format.MagicSnapshotFtr:
			if size-cur < format.SnapshotFooterSize {
				return c, false, 0, nil // interrupted footer write
			}
			var fb [format.SnapshotFooterSize]byte
			if _, err := s.data.ReadAt(fb[:], cur); err != nil {
				return c, false, 0, err
			}
			var ftr format.SnapshotFooter
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
			c.end = cur + format.SnapshotFooterSize
			c.prevFooter = ftr.PreviousFooterOffset
			c.ftrTxnCRC = ftr.IndexTxnCRC32C
			c.footerCRC = binary.LittleEndian.Uint32(fb[format.SnapshotFooterCRC32COffset:])
			c.footerBytes = append([]byte(nil), fb[:]...)
			c.blockCount = ftr.BlockCount
			c.metaCount = ftr.MetadataBlockCount
			c.rowCount = ftr.RowRecordCount
			return c, true, c.end, nil
		default:
			return c, false, 0, nil // unknown structure: break
		}
	}
	return c, false, 0, nil // ran out of file without a footer
}

// hasValidFooterAfter reports whether a valid SnapshotFooter exists at or
// after offset. Footer positions are NOT 8-aligned (block payload lengths are
// arbitrary), so this walks byte-by-byte; it runs only on the corrupt/tail
// path, and a false positive requires an 8-byte magic collision plus a
// passing CRC-32C over 144 bytes (~2^-32 per candidate), which is
// negligible. The footer is the commit authority: its presence means earlier
// bytes in this region are mid-file corruption, never an uncommitted tail.
func (s *Store) hasValidFooterAfter(from, size int64) bool {
	for p := from; p+format.SnapshotFooterSize <= size; p++ {
		var magic [8]byte
		if _, err := s.data.ReadAt(magic[:], p); err != nil {
			return false
		}
		if string(magic[:]) != format.MagicSnapshotFtr {
			continue
		}
		var fb [format.SnapshotFooterSize]byte
		if _, err := s.data.ReadAt(fb[:], p); err != nil {
			return false
		}
		var ftr format.SnapshotFooter
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

// rebuildIndex reconstructs the index transaction for a committed
// snapshot whose IndexTxn is missing or corrupt, by reading and parsing its
// blocks in [BlocksStartOffset, BlocksEndOffset).
func (s *Store) rebuildIndex(c *committedSnapshot) (*index.Txn, error) {
	var blockEntries []format.BlockIndexEntry
	var metaEntries []format.MetadataIndexEntry
	var rowEntries []format.RowIndexEntry
	// Footer counts passed CRC validation, but remain untrusted input. Bound
	// capacity hints to avoid turning a forged footer into an OOM request.
	const maxPreallocBytes = uint64(64 << 20)
	boundedCap := func(count uint64, entrySize int) int {
		limit := maxPreallocBytes / uint64(entrySize)
		if count > limit {
			count = limit
		}
		return int(count)
	}
	blockEntries = make([]format.BlockIndexEntry, 0, boundedCap(uint64(c.blockCount), format.BlockIndexEntrySize))
	metaEntries = make([]format.MetadataIndexEntry, 0, boundedCap(uint64(c.metaCount), format.MetadataIndexEntrySize))
	rowEntries = make([]format.RowIndexEntry, 0, boundedCap(c.rowCount, format.RowIndexEntrySize))
	var rowCount uint64

	cur := c.blocksStart
	for cur < c.blocksEnd {
		var bhBuf [format.BlockHeaderSize]byte
		if _, err := s.data.ReadAt(bhBuf[:], cur); err != nil {
			return nil, err
		}
		var bh format.BlockHeader
		if err := bh.Unmarshal(bhBuf[:]); err != nil {
			return nil, fmt.Errorf("rowpack: block header at %d: %w", cur, err)
		}
		blockEntries = append(blockEntries, format.BlockIndexEntry{
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
		case format.BlockKindRows:
			// Rebuild the row index from the page container stream: iterate
			// every record (decompressing one page at a time) and emit the
			// RowID/ChangeType each record carries. The whole block is never
			// decompressed at once, so the rebuild peak stays bounded.
			rc, err := s.loader.LoadRows(cur, bh.BlockID)
			if err != nil {
				return nil, err
			}
			// ItemOrdinal is the record's position inside its own block (that is what
			// RowsContainer.RecordAt/PageFor resolve), so the counter restarts per
			// block. A snapshot spanning more than one rows block must not carry a
			// running total across blocks.
			var ordinal uint32
			err = rc.ForEach(func(rec codec.PageRecord) error {
				rowEntries = append(rowEntries, format.RowIndexEntry{
					SnapshotID: bh.SnapshotID, TableID: bh.TableID,
					ChangeType: rec.ChangeType, RowID: rec.RowID,
					BlockID: bh.BlockID, ItemOrdinal: ordinal,
				})
				ordinal++
				rowCount++
				return nil
			})
			if err != nil {
				return nil, err
			}
		case format.BlockKindMetadata:
			blk, err := s.loader.Load(cur, bh.BlockID)
			if err != nil {
				return nil, err
			}
			mp, err := metadata.Parse(blk.Raw)
			if err != nil {
				return nil, err
			}
			for i := range mp.Entries {
				metaEntries = append(metaEntries, format.MetadataIndexEntry{
					SnapshotID: bh.SnapshotID, ObjectID: mp.Entries[i].ObjectID,
					Revision: mp.Entries[i].Revision, RecordType: mp.Entries[i].RecordType,
					BlockID: bh.BlockID, ItemOrdinal: uint32(i),
					Operation: mp.Entries[i].Operation, Critical: mp.Entries[i].Critical,
				})
			}
		}
		cur += format.BlockHeaderSize + int64(bh.StoredSize)
	}

	var shBuf [format.SnapshotHeaderSize]byte
	if _, err := s.data.ReadAt(shBuf[:], c.start); err != nil {
		return nil, err
	}
	var sh format.SnapshotHeader
	if err := sh.Unmarshal(shBuf[:]); err != nil {
		return nil, err
	}

	builder := index.NewBuilder(0) // sequence filled by the writer on commit
	builder.Reserve(len(metaEntries), len(blockEntries), len(rowEntries))
	snapEntry := format.SnapshotIndexEntry{
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
	_, txn, err := builder.Build(index.BodyBounds{
		DataStart: uint64(c.start), DataEnd: uint64(c.end),
		TxnStart: c.txnStart, TxnEnd: c.txnEnd,
	}, c.footerCRC)
	if err != nil {
		return nil, err
	}
	return txn, nil
}
