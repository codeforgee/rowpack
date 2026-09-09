package index

import (
	"bytes"
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

func zigzag(v int64) uint64 { return uint64((v << 1) ^ (v >> 63)) }

func unzigzag(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }

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

// emitChunks splits the builder's metadata and block entry streams into
// chunks. The snapshot chunk head is reserved (not yet filled); snapshot,
// metadata and block chunks are emitted here in frozen order. Row entries are
// NOT chunked: they are written as sorted Row Index Pages + a Fence Directory
// by `buildRowIndexPages` (S3-⑦), and `cc.seq` is left at the next free chunk
// sequence so pages can seal under distinct chunk sequences.
func (b *Builder) emitChunks(cc *chunkCompressor) error {
	if err := emitFixedChunks(cc, fileformat.IndexChunkKindMetadata, len(b.metadata), fileformat.MetadataIndexEntrySize,
		func(i int, dst []byte) error { return b.metadata[i].MarshalTo(dst) }); err != nil {
		return err
	}
	if err := emitFixedChunks(cc, fileformat.IndexChunkKindBlock, len(b.blocks), fileformat.BlockIndexEntrySize,
		func(i int, dst []byte) error { return b.blocks[i].MarshalTo(dst) }); err != nil {
		return err
	}
	return nil
}

// BuildStoredBody serializes the builder's entries into the stored body
// (chunk headers + payloads + directory + Row Index Pages + Fence Directory,
// WITHOUT the IndexTxnHeader and IndexTxnFooter) and returns it together with
// the plaintext-body CRC and the materialized Txn. resolveDataBounds receives
// the final stored body length and returns the snapshot's [DataStart, DataEnd];
// the snapshot chunk's stored size is fixed at 72 bytes, so the resolution is
// stable in one pass. A nil resolver passes the snapshot entry's own values
// through (tests).
//
// Body layout (ADR-005, S3-⑦ 落盘②):
//
//	[SnapshotChunk][MetadataChunks][BlockChunks][ChunkDirectory]
//	[IndexPage × N][RowIndexFenceEntry × N]
func (b *Builder) BuildStoredBody(crypto *ChunkCrypto, level int, resolveBounds func(bodyLen int) (dataStart, dataEnd uint64, txnStart, txnEnd int64)) (body []byte, plainCRC uint32, txn *Txn, err error) {
	if b.snapshot == nil {
		return nil, 0, nil, errors.New("rowpack: no snapshot entry to build")
	}
	cc := newChunkCompressor(crypto, level)
	if err := b.emitChunks(cc); err != nil {
		return nil, 0, nil, err
	}
	// Build the sorted Row Index Pages + Fence Directory. pageSeqBase is the
	// next free chunk sequence so pages seal under distinct nonces.
	pages, err := b.buildRowIndexPages(crypto, level, cc.seq)
	if err != nil {
		return nil, 0, nil, err
	}
	pageRegionLen := 0
	for i := range pages {
		pageRegionLen += len(pages[i].stored)
	}
	fenceRegionLen := len(pages) * fileformat.IndexFenceEntrySize
	// +32B: the snapshot chunk's directory entry (always present) is
	// prepended after the bounds resolve, so it is counted here. The
	// snapshot chunk payload itself is already counted in the reserved head.
	bodyLen := len(cc.out) + len(cc.dir) + fileformat.IndexChunkDirEntrySize + pageRegionLen + fenceRegionLen
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
	// Patch each fence's StoredOffset now that the chunk region + directory
	// length is final, and serialize the fence directory.
	pageOff := uint64(len(cc.out) + len(cc.dir))
	fenceBytes := make([]byte, 0, fenceRegionLen)
	var fbuf [fileformat.IndexFenceEntrySize]byte
	for i := range pages {
		pages[i].fence.StoredOffset = pageOff
		pageOff += uint64(len(pages[i].stored))
		if err := pages[i].fence.MarshalTo(fbuf[:]); err != nil {
			return nil, 0, nil, err
		}
		fenceBytes = append(fenceBytes, fbuf[:]...)
	}
	// Plaintext-body CRC covers the re-readable content in physical order:
	// snapshot raw, metadata/block chunk raws, directory, raw pages, fences.
	parts := make([][]byte, 0, len(cc.rawParts)+2+2*len(pages))
	parts = append(parts, raw[:])
	parts = append(parts, cc.rawParts...)
	parts = append(parts, cc.dir)
	for i := range pages {
		parts = append(parts, pages[i].raw)
	}
	parts = append(parts, fenceBytes)
	plainCRC = uint32(0)
	for _, p := range parts {
		plainCRC = fileformat.CRC32CConcat(plainCRC, p)
	}
	body = cc.out
	body = append(body, cc.dir...)
	for i := range pages {
		body = append(body, pages[i].stored...)
	}
	body = append(body, fenceBytes...)
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
	metadata   []fileformat.MetadataIndexEntry
	blocks     []fileformat.BlockIndexEntry
	rows       []fileformat.RowIndexEntry // buffered mode only; nil when streaming
	snapshot   fileformat.SnapshotIndexEntry
	hasSnap    bool
	dir        []fileformat.IndexChunkDirEntry
	plainCRC   uint32
	metaCount  uint32
	blockCount uint32
	rowCount   uint64
	chunkCount uint32 // number of chunks (snapshot + metadata + block); page seal base
}

// rowBatchSize bounds the streaming row batch handed to TxnSink.AddRows: a
// fixed ~20 KiB scratch (40 B x 512), so streaming stays O(1) memory while
// keeping per-row work inside a tight, devirtualized loop.
const rowBatchSize = 512

// TxnSink receives index-txn entries as they are decoded, instead of letting
// the parser materialize the full []RowIndexEntry. Methods are called in
// chunk order; SetSnapshot always arrives before the Add* methods.
// AddRows receives bounded batches (never larger than rowBatchSize) that are
// only valid for the duration of the call; implementors must copy what they
// keep. This builds final structures directly (View streaming apply) and
// skips the ~40 B/row intermediate slice without per-entry virtual calls.
type TxnSink interface {
	SetSnapshot(e fileformat.SnapshotIndexEntry) error
	AddMetadata(e fileformat.MetadataIndexEntry) error
	AddBlock(e fileformat.BlockIndexEntry) error
	AddRows(batch []fileformat.RowIndexEntry) error
}

// RowHintSink is an optional TxnSink extension: the parser reports the
// header's row entry count (bounded to the prealloc guard) before the first
// row so the sink can preallocate exactly.
type RowHintSink interface {
	ReserveRows(hint int)
}

// RowEntrySink is an optional TxnSink extension for row entries: instead of
// handing the parser bounded []RowIndexEntry batches via AddRows, the sink
// receives each decoded entry individually via AddRowEntry. The page decoder
// (walkRowIndexPage) feeds entries straight to this sink — one at a time, in
// (TableID, RowID) sorted order — so the Eager rowShard builder appends into
// its columnar arrays without materializing a []RowIndexEntry page or a
// []RowKeyLoc intermediate (S3-⑦ 落盘② Open 峰值优化).
type RowEntrySink interface {
	AddRowEntry(e fileformat.RowIndexEntry) error
}

// FenceCaptureSink is an optional TxnSink extension for the Lazy index mode:
// the parser hands the sink the validated Row Index Fence Directory and then
// stops WITHOUT decoding any page payload (no page is OPENed/decompressed).
// Implementing it makes parseRowIndexPages skip page decoding, so a Lazy Open
// never materializes a page and never touches the row bytes — only the 52 B
// fence entries become resident. The sink must run count validation itself.
type FenceCaptureSink interface {
	SetRowIndexFences(fences []fileformat.RowIndexFenceEntry) error
}

// bufferedSink is the default TxnSink collecting into storedBody slices
// (historical behavior).
type bufferedSink struct{ sb *storedBody }

func (s bufferedSink) SetSnapshot(e fileformat.SnapshotIndexEntry) error {
	if s.sb.hasSnap {
		return fmt.Errorf("rowpack: duplicate snapshot chunk")
	}
	s.sb.snapshot = e
	s.sb.hasSnap = true
	return nil
}

func (s bufferedSink) AddMetadata(e fileformat.MetadataIndexEntry) error {
	s.sb.metadata = append(s.sb.metadata, e)
	return nil
}

func (s bufferedSink) AddBlock(e fileformat.BlockIndexEntry) error {
	s.sb.blocks = append(s.sb.blocks, e)
	return nil
}

func (s bufferedSink) AddRows(batch []fileformat.RowIndexEntry) error {
	s.sb.rows = append(s.sb.rows, batch...)
	return nil
}

// parseStoredBody walks the chunk sequence and directory, authenticating and
// decoding every chunk. crypto must be non-nil iff the chunks are encrypted.
// A non-nil sink receives every entry as it is decoded (streaming mode; rows
// are never buffered); nil collects everything into the returned storedBody.
func parseStoredBody(region []byte, snapshotID uint64, metadataCount, blockCount, rowCount int, rowIndexPageCount uint32, crypto *ChunkCrypto, sink TxnSink) (*storedBody, error) {
	sb := &storedBody{
		plainCRC: fileformat.CRC32C(nil),
	}
	if sink == nil {
		sb.metadata = make([]fileformat.MetadataIndexEntry, 0, metadataCount)
		sb.blocks = make([]fileformat.BlockIndexEntry, 0, blockCount)
		sb.rows = make([]fileformat.RowIndexEntry, 0, rowCount)
		sink = bufferedSink{sb: sb}
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
			if err := sink.SetSnapshot(sb.snapshot); err != nil {
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
				if err := sink.AddMetadata(e); err != nil {
					return nil, fmt.Errorf("rowpack: metadata chunk %d entry %d: %w", seq, i, err)
				}
			}
			sb.metaCount += h.EntryCount
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
				if err := sink.AddBlock(e); err != nil {
					return nil, fmt.Errorf("rowpack: block chunk %d entry %d: %w", seq, i, err)
				}
			}
			sb.blockCount += h.EntryCount
		case fileformat.IndexChunkKindRow:
			return nil, fmt.Errorf("rowpack: row chunk %d: obsolete v1 row-chunk layout (S3-⑦ 落盘 switch to Index Pages; refusing to decode)", seq)
		default:
			return nil, fmt.Errorf("rowpack: chunk %d unknown kind %d", seq, h.EntryKind)
		}
		nextOrd[h.EntryKind] += h.EntryCount
		sb.plainCRC = fileformat.CRC32CConcat(sb.plainCRC, raw)
		pos += fileformat.IndexChunkHeaderSize + int(h.StoredBytes)
		seq++
	}
	// Directory: after the last chunk, region[pos:] = directory (seq*32) + row
	// index pages + fence directory. The directory is plaintext (even in
	// encrypted stores) so the scanner can reach it without a key.
	rest := region[pos:]
	dirLen := int(seq) * fileformat.IndexChunkDirEntrySize
	if len(rest) < dirLen || dirLen == 0 {
		return nil, fmt.Errorf("rowpack: chunk directory %d bytes malformed", len(rest))
	}
	dirBytes := rest[:dirLen]
	dir, err := fileformat.ParseIndexChunkDirectory(dirBytes)
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
	sb.plainCRC = fileformat.CRC32CConcat(sb.plainCRC, dirBytes)
	sb.chunkCount = seq
	if err := parseRowIndexPages(region, pos+dirLen, rowIndexPageCount, snapshotID, crypto, sink, seq, &sb.plainCRC, &sb.rowCount); err != nil {
		return nil, err
	}
	return sb, nil
}

// parseRowIndexPages decodes the Row Index Pages + Fence Directory that
// follow the chunk directory in a txn body (S3-⑦). region is the full body;
// pageStart is the absolute offset (within region) where the pages region
// begins. crypto must be non-nil iff the pages are sealed (encrypted store);
// pages seal under the chunk sequence continuing past seq (the chunk count).
// Entries are handed to sink in RowID-sorted order in bounded batches, the
// plaintext-body CRC gains the raw page bytes and the fence bytes, and the
// running row count is accumulated into rowCount. Forged page counts, offsets
// or sizes are rejected before any attacker-sized allocation.
// parseRowIndexFences parses and validates the Row Index Fence Directory that
// follows the pages region of a txn body (S3-⑦). It returns the fences and the
// absolute offset (within region) where the fence directory begins = where the
// pages region ends. Used by both the page-decoding path and the Lazy path
// (which stops after the fence, never decoding a page payload).
func parseRowIndexFences(region []byte, pageStart int, pageCount uint32, snapshotID uint64) (fences []fileformat.RowIndexFenceEntry, pageEnd int, err error) {
	if pageCount == 0 {
		return nil, pageStart, nil
	}
	fenceLen := int(pageCount) * fileformat.IndexFenceEntrySize
	if pageStart < 0 || pageStart >= len(region) || fenceLen > len(region)-pageStart {
		return nil, 0, fmt.Errorf("rowpack: row index fence directory %d bytes exceeds body %d", fenceLen, len(region)-pageStart)
	}
	pageEnd = len(region) - fenceLen
	if pageEnd < pageStart {
		return nil, 0, fmt.Errorf("rowpack: row index pages region %d..%d malformed", pageStart, pageEnd)
	}
	fenceRegion := region[pageEnd:]
	fences = make([]fileformat.RowIndexFenceEntry, int(pageCount))
	for i := range fences {
		off := i * fileformat.IndexFenceEntrySize
		if err := fences[i].Unmarshal(fenceRegion[off : off+fileformat.IndexFenceEntrySize]); err != nil {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d: %w", i, err)
		}
	}
	// Fence cohesion: snapshot ownership, strictly ordered and contiguous
	// within the pages region, with non-zero sizes.
	expectOff := uint64(pageStart)
	for i := range fences {
		f := &fences[i]
		if f.SnapshotID != snapshotID {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d snapshot %d, want %d", i, f.SnapshotID, snapshotID)
		}
		if f.StoredSize == 0 || f.RawSize == 0 || f.EntryCount == 0 {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d zero size/entry", i)
		}
		if f.StoredOffset < uint64(pageStart) || f.StoredOffset > uint64(pageEnd) ||
			uint64(f.StoredSize) > uint64(pageEnd)-f.StoredOffset {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d page out of bounds", i)
		}
		if f.StoredOffset != expectOff {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d offset %d, want %d", i, f.StoredOffset, expectOff)
		}
		expectOff += uint64(f.StoredSize)
	}
	if expectOff != uint64(pageEnd) {
		return nil, 0, fmt.Errorf("rowpack: row index pages span %d bytes, want %d", expectOff-uint64(pageStart), pageEnd-pageStart)
	}
	return fences, pageEnd, nil
}

// parseRowIndexPages decodes the Row Index Pages + Fence Directory that
// follow the chunk directory in a txn body (S3-⑦). region is the full body;
// pageStart is the absolute offset (within region) where the pages region
// begins. crypto must be non-nil iff the pages are sealed (encrypted store);
// pages seal under the chunk sequence continuing past seq (the chunk count).
// Entries are handed to sink in RowID-sorted order in bounded batches, the
// plaintext-body CRC gains the raw page bytes and the fence bytes, and the
// running row count is accumulated into rowCount. Forged page counts, offsets
// or sizes are rejected before any attacker-sized allocation.
func parseRowIndexPages(region []byte, pageStart int, pageCount uint32, snapshotID uint64, crypto *ChunkCrypto, sink TxnSink, seq uint32, plainCRC *uint32, rowCount *uint64) error {
	if pageCount == 0 {
		return nil
	}
	// Fence directory parsed + validated first; pageEnd is where the fence
	// directory begins (= where the pages region ends).
	fences, pageEnd, err := parseRowIndexFences(region, pageStart, pageCount, snapshotID)
	if err != nil {
		return err
	}
	// Lazy mode: the sink captures the fence and stops without decoding any
	// page payload. The running row count is derived from the fence; the
	// plaintext-body CRC is left incomplete (the Lazy Open deliberately skips
	// the full-body CRC — integrity comes from the stored-byte CRC plus each
	// page's own PageCRC32C / AEAD).
	if fs, ok := sink.(FenceCaptureSink); ok {
		if err := fs.SetRowIndexFences(fences); err != nil {
			return err
		}
		for i := range fences {
			*rowCount += uint64(fences[i].EntryCount)
		}
		return nil
	}
	// Decode each page and hand its entries to the sink.
	for i := range fences {
		f := &fences[i]
		stored := region[int(f.StoredOffset) : int(f.StoredOffset)+int(f.StoredSize)]
		raw := stored
		if crypto != nil {
			pt, err := crypto.Open(seq+uint32(i), rowIndexPageChunkKind, uint32(i), int(f.RawSize), stored)
			if err != nil {
				return fmt.Errorf("rowpack: row index page %d open: %w", i, err)
			}
			raw = pt
		}
		pageRaw, err := block.Decompress(fileformat.CompressionZstd, nil, raw, f.RawSize)
		if err != nil {
			return fmt.Errorf("rowpack: row index page %d decompress: %w", i, err)
		}
		if uint32(len(pageRaw)) != f.RawSize {
			return fmt.Errorf("rowpack: row index page %d raw %d bytes, want %d", i, len(pageRaw), f.RawSize)
		}
		if fileformat.CRC32C(pageRaw[fileformat.IndexPageHeaderSize:]) != f.PageCRC32C {
			return fmt.Errorf("rowpack: row index page %d CRC mismatch", i)
		}
		// The page encodes (TableID, RowID, BlockID, ItemOrdinal, ChangeType)
		// without SnapshotID (txn-wide); stamp it before handing to the sink.
		// A RowEntrySink receives entries one at a time via walkRowIndexPage —
		// no []RowIndexEntry page materialization, no []RowKeyLoc intermediate
		// (S3-⑦ 落盘② Open 峰值优化). Older sinks fall back to the batched path.
		if es, ok := sink.(RowEntrySink); ok {
			emitted := 0
			if err := walkRowIndexPage(pageRaw, func(e fileformat.RowIndexEntry) error {
				e.SnapshotID = snapshotID
				emitted++
				return es.AddRowEntry(e)
			}); err != nil {
				return fmt.Errorf("rowpack: row index page %d: %w", i, err)
			}
			if emitted != int(f.EntryCount) {
				return fmt.Errorf("rowpack: row index page %d %d entries, fence says %d", i, emitted, f.EntryCount)
			}
			*rowCount += uint64(emitted)
			*plainCRC = fileformat.CRC32CConcat(*plainCRC, pageRaw)
			continue
		}
		entries, err := decodeRowIndexPage(pageRaw)
		if err != nil {
			return fmt.Errorf("rowpack: row index page %d: %w", i, err)
		}
		if uint32(len(entries)) != f.EntryCount {
			return fmt.Errorf("rowpack: row index page %d %d entries, fence says %d", i, len(entries), f.EntryCount)
		}
		for j := range entries {
			entries[j].SnapshotID = snapshotID
		}
		*rowCount += uint64(len(entries))
		*plainCRC = fileformat.CRC32CConcat(*plainCRC, pageRaw)
		for start := 0; start < len(entries); start += rowBatchSize {
			end := start + rowBatchSize
			if end > len(entries) {
				end = len(entries)
			}
			if err := sink.AddRows(entries[start:end]); err != nil {
				return fmt.Errorf("rowpack: row index page %d add rows: %w", i, err)
			}
		}
	}
	*plainCRC = fileformat.CRC32CConcat(*plainCRC, region[pageEnd:])
	return nil
}
