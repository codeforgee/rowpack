package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// Index txn chunking (docs/INDEX_TXN_CHUNK_COMPRESSION.md): the txn body is
// a sequence of independently compressed/authenticated chunks followed by a
// plaintext chunk directory. Row chunks carry a delta/varint encoding that
// removes per-entry context (SnapshotID is txn-wide, TableID/BlockID are
// run-length tagged, RowID/ItemOrdinal are zigzag deltas); snapshot, metadata
// and block chunks keep their fixed-size entries concatenated. The snapshot
// chunk is always uncompressed: its 72-byte stored size is content-independent,
// which keeps the total body length resolvable in a single build pass (the
// snapshot entry's DataEnd depends on the body length).
//
// Frozen v1 row-chunk encoding (every chunk is independently decodable; the
// first entry is the absolute base):
//
//	first entry: uvarint(tableID) uvarint(rowID) uvarint(blockID)
//	             uvarint(itemOrdinal) byte(changeType)
//	next entry:  byte tag            // bit0 = tableID same as previous
//	                                   bit1 = blockID same as previous
//	             [uvarint zigzag tableDelta]   // only when bit0 == 0
//	             [uvarint zigzag blockDelta]   // only when bit1 == 0
//	             uvarint zigzag rowDelta
//	             uvarint zigzag ordinalDelta
//	             byte changeType

// ChunkCrypto carries the txn-scoped encryption context for chunk sealing and
// opening. A nil *ChunkCrypto means the txn is stored plain; when non-nil,
// every chunk MUST be encrypted.
type ChunkCrypto struct {
	TxnSequence uint64
	SnapshotID  uint64
	Epoch       uint32
	// Seal receives the compressed chunk payload and returns the final stored
	// payload (ciphertext including the GCM tag).
	Seal func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error)
	// Open authenticates and decrypts one stored payload back to its
	// compressed form.
	Open func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error)
}

var errChunkTruncated = errors.New("index chunk data truncated")

// ---- row delta codec ----

func putChunkUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func zigzag(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

// crcConcat extends a streaming CRC-32C with one more slice.
func crcConcat(crc uint32, data []byte) uint32 { return fileformat.CRC32CConcat2(crc, data) }

func unzigzag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

// rowEncoder is the stateful incremental encoder for one row chunk: the
// first entry is written as the absolute base, later entries as deltas.
type rowEncoder struct {
	buf     []byte
	started bool
	prev    fileformat.RowIndexEntry
}

// encode appends the frozen delta encoding of one entry.
func (re *rowEncoder) encode(e *fileformat.RowIndexEntry) {
	if !re.started {
		re.buf = putChunkUvarint(re.buf, uint64(e.TableID))
		re.buf = putChunkUvarint(re.buf, e.RowID)
		re.buf = putChunkUvarint(re.buf, e.BlockID)
		re.buf = putChunkUvarint(re.buf, uint64(e.ItemOrdinal))
		re.buf = append(re.buf, byte(e.ChangeType))
		re.started = true
		re.prev = *e
		return
	}
	p := &re.prev
	var tag byte
	if e.TableID == p.TableID {
		tag |= 1
	}
	if e.BlockID == p.BlockID {
		tag |= 2
	}
	re.buf = append(re.buf, tag)
	if tag&1 == 0 {
		re.buf = putChunkUvarint(re.buf, zigzag(int64(e.TableID)-int64(p.TableID)))
	}
	if tag&2 == 0 {
		re.buf = putChunkUvarint(re.buf, zigzag(int64(e.BlockID)-int64(p.BlockID)))
	}
	re.buf = putChunkUvarint(re.buf, zigzag(int64(e.RowID)-int64(p.RowID)))
	re.buf = putChunkUvarint(re.buf, zigzag(int64(e.ItemOrdinal)-int64(p.ItemOrdinal)))
	re.buf = append(re.buf, byte(e.ChangeType))
	re.prev = *e
}

// decodeRowChunk decodes count entries from the frozen delta layout into dst with
// strict bounds checks: truncated or malformed input is an error, never a
// panic. SnapshotID is txn-wide and stamped by the caller.
func decodeRowChunk(dst []fileformat.RowIndexEntry, raw []byte, count uint32, snapshotID uint64) ([]fileformat.RowIndexEntry, error) {
	pos := 0
	readUvarint := func() (uint64, error) {
		v, n := binary.Uvarint(raw[pos:])
		if n <= 0 {
			return 0, errChunkTruncated
		}
		pos += n
		return v, nil
	}
	readByte := func() (byte, error) {
		if pos >= len(raw) {
			return 0, errChunkTruncated
		}
		b := raw[pos]
		pos++
		return b, nil
	}
	var prevTable, prevRowID, prevBlockID, prevOrdinal uint64
	for i := uint32(0); i < count; i++ {
		var e fileformat.RowIndexEntry
		if i == 0 {
			table, err := readUvarint()
			if err != nil {
				return nil, err
			}
			rowID, err := readUvarint()
			if err != nil {
				return nil, err
			}
			blockID, err := readUvarint()
			if err != nil {
				return nil, err
			}
			ordinal, err := readUvarint()
			if err != nil {
				return nil, err
			}
			change, err := readByte()
			if err != nil {
				return nil, err
			}
			prevTable, prevRowID, prevBlockID, prevOrdinal = table, rowID, blockID, ordinal
			e.TableID, e.RowID, e.BlockID, e.ItemOrdinal = uint32(table), rowID, blockID, uint32(ordinal)
			e.ChangeType = fileformat.ChangeType(change)
		} else {
			tag, err := readByte()
			if err != nil {
				return nil, err
			}
			if tag&^byte(3) != 0 {
				return nil, fmt.Errorf("rowpack: row chunk entry %d: unknown tag bits %#x", i, tag)
			}
			if tag&1 == 0 {
				d, err := readUvarint()
				if err != nil {
					return nil, err
				}
				prevTable = uint64(int64(prevTable) + unzigzag(d))
				if prevTable > 0xFFFFFFFF {
					return nil, fmt.Errorf("rowpack: row chunk entry %d: table id overflow", i)
				}
			}
			if tag&2 == 0 {
				d, err := readUvarint()
				if err != nil {
					return nil, err
				}
				prevBlockID = uint64(int64(prevBlockID) + unzigzag(d))
			}
			d, err := readUvarint()
			if err != nil {
				return nil, err
			}
			prevRowID = uint64(int64(prevRowID) + unzigzag(d))
			d, err = readUvarint()
			if err != nil {
				return nil, err
			}
			prevOrdinal = uint64(int64(prevOrdinal) + unzigzag(d))
			change, err := readByte()
			if err != nil {
				return nil, err
			}
			e.TableID, e.RowID, e.BlockID, e.ItemOrdinal = uint32(prevTable), prevRowID, prevBlockID, uint32(prevOrdinal)
			e.ChangeType = fileformat.ChangeType(change)
		}
		if e.ChangeType != fileformat.ChangeInsert && e.ChangeType != fileformat.ChangeUpdate && e.ChangeType != fileformat.ChangeDelete {
			return nil, fmt.Errorf("rowpack: row chunk entry %d: bad change type %d", i, e.ChangeType)
		}
		e.SnapshotID = snapshotID
		dst = append(dst, e)
	}
	if pos != len(raw) {
		return nil, fmt.Errorf("rowpack: row chunk has %d trailing bytes", len(raw)-pos)
	}
	return dst, nil
}

// ---- chunk body assembly (write path) ----

// chunkCompressor accumulates the stored body: chunk headers + payloads in
// frozen order (snapshot, metadata, block, rows), then the plaintext
// directory. The first 136 bytes (64B header + 72B uncompressed snapshot
// entry) are reserved up front so chunk region offsets are final without a
// second pass.
type chunkCompressor struct {
	crypto *ChunkCrypto
	level  int
	seq    uint32

	out         []byte // stored body bytes (starts with the reserved snapshot chunk)
	dir         []byte // directory entries (32B each)
	rawParts    [][]byte
	snapPayload int // reserved snapshot chunk payload size (72B / 88B sealed)

	// plainCRC is the CRC over the plaintext body in physical order:
	// snapshot raw, streamed chunk raws, directory. Computed once at the end
	// of the build (the snapshot raw is only available after bounds resolve).
}

func newChunkCompressor(crypto *ChunkCrypto, level int) *chunkCompressor {
	// Chunk sequence 0 belongs to the snapshot chunk (reserved head); the
	// streamed chunks number from 1 in physical order. The head payload
	// region is content-independent in size: 72B uncompressed, +16B tag when
	// encrypted.
	snapPayload := fileformat.SnapshotIndexEntrySize
	if crypto != nil {
		snapPayload += fileformat.AESGCMTagLen
	}
	cc := &chunkCompressor{crypto: crypto, level: level, seq: 1, snapPayload: snapPayload}
	cc.out = make([]byte, fileformat.IndexChunkHeaderSize+snapPayload)
	return cc
}

type chunkBuild struct {
	kind         uint8
	firstOrdinal uint32
	entries      int
	raw          []byte
}

// add emits one chunk: compress (unless snapshot), seal, append
// header+payload, record the directory entry, and fold the raw payload into
// the plaintext-body CRC.
func (cc *chunkCompressor) add(cb *chunkBuild) error {
	stored := cb.raw
	compression := fileformat.IndexChunkCompressionNone
	if cb.kind != fileformat.IndexChunkKindSnapshot && len(cb.raw) > 0 {
		c, err := block.Compress(fileformat.CompressionZstd, cc.level, cb.raw)
		if err != nil {
			return err
		}
		stored = c
		compression = fileformat.IndexChunkCompressionZstd
	}
	encryption := fileformat.IndexChunkEncryptionNone
	if cc.crypto != nil {
		sealed, err := cc.crypto.Seal(cc.seq, cb.kind, cb.firstOrdinal, len(cb.raw), stored)
		if err != nil {
			return err
		}
		stored = sealed
		encryption = fileformat.IndexChunkEncryptionAESGCM
	}
	keyEpoch := uint32(0)
	if cc.crypto != nil {
		keyEpoch = cc.crypto.Epoch
	}
	h := fileformat.IndexChunkHeader{
		EntryKind:         cb.kind,
		Compression:       compression,
		Encryption:        encryption,
		KeyEpoch:          keyEpoch,
		ChunkSequence:     cc.seq,
		EntryCount:        uint32(cb.entries),
		FirstEntryOrdinal: cb.firstOrdinal,
		RawBytes:          uint32(len(cb.raw)),
		StoredBytes:       uint32(len(stored)),
		PayloadCRC32C:     fileformat.CRC32C(stored),
	}
	off := uint64(len(cc.out))
	if err := h.MarshalTo(cc.reserve(fileformat.IndexChunkHeaderSize)); err != nil {
		return err
	}
	cc.out = append(cc.out, stored...)
	var de fileformat.IndexChunkDirEntry
	de.ChunkSequence = cc.seq
	de.EntryCount = h.EntryCount
	de.FirstEntryOrdinal = cb.firstOrdinal
	de.RawBytes = h.RawBytes
	de.StoredBytes = h.StoredBytes
	de.EntryKind = cb.kind
	de.RegionOffset = off
	de.MarshalTo(cc.reserveDir(fileformat.IndexChunkDirEntrySize))
	cc.rawParts = append(cc.rawParts, cb.raw)
	cc.seq++
	return nil
}

func (cc *chunkCompressor) reserve(n int) []byte {
	pos := len(cc.out)
	cc.out = append(cc.out, make([]byte, n)...)
	return cc.out[pos : pos+n : pos+n]
}

func (cc *chunkCompressor) reserveDir(n int) []byte {
	pos := len(cc.dir)
	cc.dir = append(cc.dir, make([]byte, n)...)
	return cc.dir[pos : pos+n : pos+n]
}

// emitFixedChunks emits n fixed-size entries as chunks with the cut rules.
func emitFixedChunks(cc *chunkCompressor, kind uint8, n, entrySize int, marshal func(i int, dst []byte) error) error {
	if n == 0 {
		return nil
	}
	cb := &chunkBuild{kind: kind}
	scratch := make([]byte, entrySize)
	for i := 0; i < n; i++ {
		for j := range scratch {
			scratch[j] = 0
		}
		if err := marshal(i, scratch); err != nil {
			return err
		}
		cb.raw = append(cb.raw, scratch...)
		cb.entries++
		if cb.entries >= fileformat.IndexChunkTargetEntries || len(cb.raw) >= fileformat.IndexChunkTargetRawBytes {
			if err := cc.add(cb); err != nil {
				return err
			}
			cb = &chunkBuild{kind: kind, firstOrdinal: uint32(i + 1)}
		}
	}
	if cb.entries > 0 {
		return cc.add(cb)
	}
	return nil
}

// emitChunks splits the builder's entry streams into chunks. The snapshot
// chunk head is reserved (not yet filled); metadata/block/row chunks are
// emitted in frozen order.
func (b *Builder) emitChunks(cc *chunkCompressor) error {
	if err := emitFixedChunks(cc, fileformat.IndexChunkKindMetadata, len(b.metadata), fileformat.MetadataIndexEntrySize,
		func(i int, dst []byte) error { return b.metadata[i].MarshalTo(dst) }); err != nil {
		return err
	}
	if err := emitFixedChunks(cc, fileformat.IndexChunkKindBlock, len(b.blocks), fileformat.BlockIndexEntrySize,
		func(i int, dst []byte) error { return b.blocks[i].MarshalTo(dst) }); err != nil {
		return err
	}
	firstOrd := uint32(0)
	cb := &chunkBuild{kind: fileformat.IndexChunkKindRow, firstOrdinal: firstOrd}
	enc := &rowEncoder{}
	flush := func() error {
		if cb.entries == 0 {
			return nil
		}
		cb.raw = enc.buf
		if err := cc.add(cb); err != nil {
			return err
		}
		firstOrd += uint32(cb.entries)
		cb = &chunkBuild{kind: fileformat.IndexChunkKindRow, firstOrdinal: firstOrd}
		enc = &rowEncoder{}
		return nil
	}
	for i := range b.rows {
		enc.encode(&b.rows[i])
		cb.entries++
		if cb.entries >= fileformat.IndexChunkTargetEntries || len(enc.buf) >= fileformat.IndexChunkTargetRawBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return nil
}

// BuildStoredBody serializes the builder's entries into the chunked stored
// body (chunk headers + payloads + directory, WITHOUT the IndexTxnHeader and
// IndexTxnFooter) and returns it together with the plaintext-body CRC and the
// materialized Txn. resolveDataBounds receives the final stored body length
// and returns the snapshot's [DataStart, DataEnd]; the snapshot chunk's
// stored size is fixed at 72 bytes, so the resolution is stable in one pass.
// A nil resolver passes the snapshot entry's own values through (tests).
func (b *Builder) BuildStoredBody(crypto *ChunkCrypto, level int, resolveBounds func(bodyLen int) (dataStart, dataEnd uint64, txnStart, txnEnd int64)) (body []byte, plainCRC uint32, txn *Txn, err error) {
	if b.snapshot == nil {
		return nil, 0, nil, errors.New("rowpack: no snapshot entry to build")
	}
	cc := newChunkCompressor(crypto, level)
	if err := b.emitChunks(cc); err != nil {
		return nil, 0, nil, err
	}
	// +32B: the snapshot chunk's directory entry (always present) is
	// prepended after the bounds resolve, so it is counted here. The
	// snapshot chunk payload itself is already counted in the reserved head.
	bodyLen := len(cc.out) + len(cc.dir) + fileformat.IndexChunkDirEntrySize
	dataStart, dataEnd := b.snapshot.DataStart, b.snapshot.DataEnd
	txnStart, txnEnd := int64(0), int64(0)
	if resolveBounds != nil {
		dataStart, dataEnd, txnStart, txnEnd = resolveBounds(bodyLen)
	}
	se := *b.snapshot
	se.DataStart = dataStart
	se.DataEnd = dataEnd
	var raw [fileformat.SnapshotIndexEntrySize]byte
	if err := se.MarshalTo(raw[:]); err != nil {
		return nil, 0, nil, err
	}
	stored := raw[:]
	encryption := fileformat.IndexChunkEncryptionNone
	if crypto != nil {
		sealed, err := crypto.Seal(0, fileformat.IndexChunkKindSnapshot, 0, len(raw), stored)
		if err != nil {
			return nil, 0, nil, err
		}
		stored = sealed
		encryption = fileformat.IndexChunkEncryptionAESGCM
	}
	if len(stored) != cc.snapPayload {
		return nil, 0, nil, fmt.Errorf("rowpack: snapshot chunk stored %d bytes, want fixed %d", len(stored), cc.snapPayload)
	}
	keyEpoch := uint32(0)
	if crypto != nil {
		keyEpoch = crypto.Epoch
	}
	h := fileformat.IndexChunkHeader{
		EntryKind:     fileformat.IndexChunkKindSnapshot,
		Encryption:    encryption,
		KeyEpoch:      keyEpoch,
		ChunkSequence: 0,
		EntryCount:    1,
		RawBytes:      uint32(len(raw)),
		StoredBytes:   uint32(len(stored)),
		PayloadCRC32C: fileformat.CRC32C(stored),
	}
	if err := h.MarshalTo(cc.out[:fileformat.IndexChunkHeaderSize]); err != nil {
		return nil, 0, nil, err
	}
	copy(cc.out[fileformat.IndexChunkHeaderSize:], stored)
	// Snapshot directory entry first, then the streamed chunks' entries; the
	// directory bytes join the plaintext CRC.
	var sde fileformat.IndexChunkDirEntry
	sde.ChunkSequence = 0
	sde.EntryCount = 1
	sde.RawBytes = h.RawBytes
	sde.StoredBytes = h.StoredBytes
	sde.EntryKind = fileformat.IndexChunkKindSnapshot
	sde.RegionOffset = 0
	var sdeBuf [fileformat.IndexChunkDirEntrySize]byte
	sde.MarshalTo(sdeBuf[:])
	cc.dir = append(sdeBuf[:], cc.dir...)
	parts := make([][]byte, 0, len(cc.rawParts)+2)
	parts = append(parts, raw[:])
	parts = append(parts, cc.rawParts...)
	parts = append(parts, cc.dir)
	plainCRC = fileformat.CRC32CConcat(parts...)
	body = cc.out
	body = append(body, cc.dir...)
	txn = &Txn{Snapshot: se, Metadata: b.metadata, Blocks: b.blocks, Rows: b.rows}
	txn.dataStart, txn.dataEnd, txn.txnStart, txn.txnEnd = dataStart, dataEnd, txnStart, txnEnd
	return body, plainCRC, txn, nil
}

// AssembleIndexTxn wraps a stored chunk body with the IndexTxnHeader and
// IndexTxnFooter. h.BodyBytes is forced to len(body); keyEpoch != 0 is
// stamped into the header reserved word (encrypted stores).
func AssembleIndexTxn(h fileformat.IndexTxnHeader, keyEpoch uint32, body []byte, f fileformat.IndexTxnFooter) ([]byte, error) {
	h.BodyBytes = uint64(len(body))
	out := make([]byte, 0, fileformat.IndexTxnHeaderSize+len(body)+fileformat.IndexTxnFooterSize)
	var hb [fileformat.IndexTxnHeaderSize]byte
	if err := h.MarshalTo(hb[:]); err != nil {
		return nil, err
	}
	if keyEpoch != 0 {
		if err := fileformat.PatchIndexTxnHeaderForStorage(hb[:], uint64(len(body)), keyEpoch); err != nil {
			return nil, err
		}
	}
	out = append(out, hb[:]...)
	out = append(out, body...)
	var fb [fileformat.IndexTxnFooterSize]byte
	if err := f.MarshalTo(fb[:]); err != nil {
		return nil, err
	}
	out = append(out, fb[:]...)
	return out, nil
}

// ---- chunked body parsing (read path) ----

// storedBody is the decoded content of one chunked txn body.
type storedBody struct {
	metadata []fileformat.MetadataIndexEntry
	blocks   []fileformat.BlockIndexEntry
	rows     []fileformat.RowIndexEntry
	snapshot fileformat.SnapshotIndexEntry
	hasSnap  bool
	dir      []fileformat.IndexChunkDirEntry
	plainCRC uint32
}

// parseStoredBody walks the chunk sequence and directory, authenticating and
// decoding every chunk. crypto must be non-nil iff the chunks are encrypted.
func parseStoredBody(region []byte, snapshotID uint64, metadataCount, blockCount, rowCount int, crypto *ChunkCrypto) (*storedBody, error) {
	sb := &storedBody{
		metadata: make([]fileformat.MetadataIndexEntry, 0, metadataCount),
		blocks:   make([]fileformat.BlockIndexEntry, 0, blockCount),
		rows:     make([]fileformat.RowIndexEntry, 0, rowCount),
		plainCRC: fileformat.CRC32C(nil),
	}
	pos := 0
	seq := uint32(0)
	nextOrd := map[uint8]uint32{}
	for pos < len(region) {
		if !bytes.HasPrefix(region[pos:], []byte(fileformat.MagicIndexChunkHdr)) {
			break // directory region follows
		}
		if pos+fileformat.IndexChunkHeaderSize > len(region) {
			return nil, fmt.Errorf("rowpack: chunk %d header truncated", seq)
		}
		var h fileformat.IndexChunkHeader
		if err := h.Unmarshal(region[pos:]); err != nil {
			return nil, fmt.Errorf("rowpack: chunk %d header: %w", seq, err)
		}
		if err := h.CheckLimits(); err != nil {
			return nil, err
		}
		if h.ChunkSequence != seq {
			return nil, fmt.Errorf("rowpack: chunk sequence %d, want %d", h.ChunkSequence, seq)
		}
		if (crypto != nil) != (h.Encryption == fileformat.IndexChunkEncryptionAESGCM) {
			return nil, fmt.Errorf("rowpack: chunk %d encryption %d inconsistent with store", seq, h.Encryption)
		}
		stored := region[pos+fileformat.IndexChunkHeaderSize : pos+fileformat.IndexChunkHeaderSize+int(h.StoredBytes)]
		if uint32(len(stored)) != h.StoredBytes {
			return nil, fmt.Errorf("rowpack: chunk %d payload truncated", seq)
		}
		if fileformat.CRC32C(stored) != h.PayloadCRC32C {
			return nil, fmt.Errorf("rowpack: chunk %d payload CRC mismatch", seq)
		}
		var raw []byte
		if h.Encryption == fileformat.IndexChunkEncryptionAESGCM {
			if h.KeyEpoch != crypto.Epoch {
				return nil, fmt.Errorf("rowpack: chunk %d key epoch %d, want %d", seq, h.KeyEpoch, crypto.Epoch)
			}
			pt, err := crypto.Open(h.ChunkSequence, h.EntryKind, h.FirstEntryOrdinal, int(h.RawBytes), stored)
			if err != nil {
				return nil, fmt.Errorf("rowpack: chunk %d open: %w", seq, err)
			}
			raw = pt
		} else {
			raw = stored
		}
		if h.Compression == fileformat.IndexChunkCompressionZstd {
			pt, err := block.Decompress(fileformat.CompressionZstd, nil, raw, h.RawBytes)
			if err != nil {
				return nil, fmt.Errorf("rowpack: chunk %d decompress: %w", seq, err)
			}
			raw = pt
		}
		if uint32(len(raw)) != h.RawBytes {
			return nil, fmt.Errorf("rowpack: chunk %d raw %d bytes, want %d", seq, len(raw), h.RawBytes)
		}
		if h.FirstEntryOrdinal != nextOrd[h.EntryKind] {
			return nil, fmt.Errorf("rowpack: chunk %d first ordinal %d, want %d", seq, h.FirstEntryOrdinal, nextOrd[h.EntryKind])
		}
		switch h.EntryKind {
		case fileformat.IndexChunkKindSnapshot:
			if h.EntryCount != 1 || len(raw) != fileformat.SnapshotIndexEntrySize {
				return nil, fmt.Errorf("rowpack: snapshot chunk %d: %d entries, %d bytes", seq, h.EntryCount, len(raw))
			}
			if sb.hasSnap {
				return nil, fmt.Errorf("rowpack: duplicate snapshot chunk %d", seq)
			}
			if err := sb.snapshot.Unmarshal(raw); err != nil {
				return nil, fmt.Errorf("rowpack: snapshot chunk %d: %w", seq, err)
			}
			sb.hasSnap = true
		case fileformat.IndexChunkKindMetadata:
			if len(raw) != int(h.EntryCount)*fileformat.MetadataIndexEntrySize {
				return nil, fmt.Errorf("rowpack: metadata chunk %d: %d bytes for %d entries", seq, len(raw), h.EntryCount)
			}
			for i := uint32(0); i < h.EntryCount; i++ {
				var e fileformat.MetadataIndexEntry
				off := int(i) * fileformat.MetadataIndexEntrySize
				if err := e.Unmarshal(raw[off : off+fileformat.MetadataIndexEntrySize]); err != nil {
					return nil, fmt.Errorf("rowpack: metadata chunk %d entry %d: %w", seq, i, err)
				}
				sb.metadata = append(sb.metadata, e)
			}
		case fileformat.IndexChunkKindBlock:
			if len(raw) != int(h.EntryCount)*fileformat.BlockIndexEntrySize {
				return nil, fmt.Errorf("rowpack: block chunk %d: %d bytes for %d entries", seq, len(raw), h.EntryCount)
			}
			for i := uint32(0); i < h.EntryCount; i++ {
				var e fileformat.BlockIndexEntry
				off := int(i) * fileformat.BlockIndexEntrySize
				if err := e.Unmarshal(raw[off : off+fileformat.BlockIndexEntrySize]); err != nil {
					return nil, fmt.Errorf("rowpack: block chunk %d entry %d: %w", seq, i, err)
				}
				sb.blocks = append(sb.blocks, e)
			}
		case fileformat.IndexChunkKindRow:
			rows, err := decodeRowChunk(sb.rows, raw, h.EntryCount, snapshotID)
			if err != nil {
				return nil, fmt.Errorf("rowpack: row chunk %d: %w", seq, err)
			}
			sb.rows = rows
		default:
			return nil, fmt.Errorf("rowpack: chunk %d unknown kind %d", seq, h.EntryKind)
		}
		nextOrd[h.EntryKind] += h.EntryCount
		sb.plainCRC = crcConcat(sb.plainCRC, raw)
		pos += fileformat.IndexChunkHeaderSize + int(h.StoredBytes)
		seq++
	}
	// Directory: everything after the last chunk, before the footer.
	rest := region[pos:]
	if len(rest) == 0 || len(rest)%fileformat.IndexChunkDirEntrySize != 0 {
		return nil, fmt.Errorf("rowpack: chunk directory %d bytes malformed", len(rest))
	}
	dir, err := fileformat.ParseIndexChunkDirectory(rest)
	if err != nil {
		return nil, err
	}
	if len(dir) != int(seq) {
		return nil, fmt.Errorf("rowpack: directory has %d entries for %d chunks", len(dir), seq)
	}
	// Re-walk to cross-check directory records against the chunk headers.
	checkPos := 0
	for i := range dir {
		de := &dir[i]
		if de.ChunkSequence != uint32(i) {
			return nil, fmt.Errorf("rowpack: directory entry %d sequence %d", i, de.ChunkSequence)
		}
		if de.RegionOffset != uint64(checkPos) {
			return nil, fmt.Errorf("rowpack: directory entry %d offset %d, want %d", i, de.RegionOffset, checkPos)
		}
		var h fileformat.IndexChunkHeader
		if err := h.Unmarshal(region[checkPos:]); err != nil {
			return nil, err
		}
		if de.EntryKind != h.EntryKind || de.EntryCount != h.EntryCount ||
			de.FirstEntryOrdinal != h.FirstEntryOrdinal || de.RawBytes != h.RawBytes || de.StoredBytes != h.StoredBytes {
			return nil, fmt.Errorf("rowpack: directory entry %d disagrees with chunk header", i)
		}
		checkPos += fileformat.IndexChunkHeaderSize + int(h.StoredBytes)
	}
	sb.dir = dir
	sb.plainCRC = crcConcat(sb.plainCRC, rest)
	return sb, nil
}
