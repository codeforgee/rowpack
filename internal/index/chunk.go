package index

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
)

// Index txn chunking (docs/INDEX_TXN_FORMAT_V1.md): the txn body is
// a sequence of independently compressed/authenticated chunks followed by a
// plaintext chunk directory. Metadata and block chunks carry the delta/varint
// encodings in chunk_entries.go; the snapshot chunk keeps its fixed 72-byte
// uncompressed entry, whose content-independent size keeps the body length
// resolvable in a single build pass (the snapshot's DataEnd depends on it).
//
// Historical note — the obsolete row-chunk encoding (no longer produced; the
// parse path rejects IndexChunkKindRow). Row entries live in sorted Row Index
// Pages instead (row_index_page.go):
//
//	first entry: uvarint(tableID) uvarint(rowID) uvarint(blockID)
//	             uvarint(itemOrdinal) byte(changeType)
//	next entry:  byte tag + [zigzag tableDelta] + [zigzag blockDelta]
//	             + zigzag rowDelta + zigzag ordinalDelta + byte changeType

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

// chunkWriter accumulates the stored body: chunk headers + payloads in
// frozen order (snapshot, metadata, block, rows), then the plaintext
// directory. The first 136 bytes (64B header + 72B uncompressed snapshot
// entry) are reserved up front so chunk region offsets are final without a
// second pass.
type chunkWriter struct {
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

func newChunkCompressor(crypto *ChunkCrypto, level int) *chunkWriter {
	// Chunk sequence 0 belongs to the snapshot chunk (reserved head); the
	// streamed chunks number from 1 in physical order. The head payload
	// region is content-independent in size: 72B uncompressed, +16B tag when
	// encrypted.
	snapPayload := format.SnapshotIndexEntrySize
	if crypto != nil {
		snapPayload += format.AESGCMTagLen
	}
	cc := &chunkWriter{crypto: crypto, level: level, seq: 1, snapPayload: snapPayload}
	cc.out = make([]byte, format.IndexChunkHeaderSize+snapPayload)
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
func (cc *chunkWriter) add(cb *chunkBuild) error {
	stored := cb.raw
	compression := format.IndexChunkCompressionNone
	if cb.kind != format.IndexChunkKindSnapshot && len(cb.raw) > 0 {
		c, err := block.Compress(format.CompressionZstd, cc.level, cb.raw)
		if err != nil {
			return err
		}
		stored = c
		compression = format.IndexChunkCompressionZstd
	}
	encryption := format.IndexChunkEncryptionNone
	if cc.crypto != nil {
		sealed, err := cc.crypto.Seal(cc.seq, cb.kind, cb.firstOrdinal, len(cb.raw), stored)
		if err != nil {
			return err
		}
		stored = sealed
		encryption = format.IndexChunkEncryptionAESGCM
	}
	keyEpoch := uint32(0)
	if cc.crypto != nil {
		keyEpoch = cc.crypto.Epoch
	}
	h := format.IndexChunkHeader{
		EntryKind:         cb.kind,
		Compression:       compression,
		Encryption:        encryption,
		KeyEpoch:          keyEpoch,
		ChunkSequence:     cc.seq,
		EntryCount:        uint32(cb.entries),
		FirstEntryOrdinal: cb.firstOrdinal,
		RawBytes:          uint32(len(cb.raw)),
		StoredBytes:       uint32(len(stored)),
		PayloadCRC32C:     format.CRC32C(stored),
	}
	off := uint64(len(cc.out))
	_ = h.MarshalTo(cc.reserve(format.IndexChunkHeaderSize)) // exact-size buffer: cannot fail
	cc.out = append(cc.out, stored...)
	var de format.IndexChunkDirEntry
	de.ChunkSequence = cc.seq
	de.EntryCount = h.EntryCount
	de.FirstEntryOrdinal = cb.firstOrdinal
	de.RawBytes = h.RawBytes
	de.StoredBytes = h.StoredBytes
	de.EntryKind = cb.kind
	de.RegionOffset = off
	_ = de.MarshalTo(cc.reserveDir(format.IndexChunkDirEntrySize)) // exact-size buffer: cannot fail
	cc.rawParts = append(cc.rawParts, cb.raw)
	cc.seq++
	return nil
}

func (cc *chunkWriter) reserve(n int) []byte {
	pos := len(cc.out)
	cc.out = append(cc.out, make([]byte, n)...)
	return cc.out[pos : pos+n : pos+n]
}

func (cc *chunkWriter) reserveDir(n int) []byte {
	pos := len(cc.dir)
	cc.dir = append(cc.dir, make([]byte, n)...)
	return cc.dir[pos : pos+n : pos+n]
}

// emitEntryChunks streams n entries through add into chunks cut by the shared
// rules (≤ IndexChunkTargetEntries entries and ≤ IndexChunkTargetRawBytes raw
// bytes per chunk). restart re-bases the encoder's delta chain at every chunk
// boundary so the next chunk's first entry encodes absolutely.
func emitEntryChunks(cc *chunkWriter, kind uint8, n int, add func(dst []byte, i int) []byte, restart func()) error {
	if n == 0 {
		return nil
	}
	cb := &chunkBuild{kind: kind}
	for i := range n {
		cb.raw = add(cb.raw, i)
		cb.entries++
		if cb.entries >= format.IndexChunkTargetEntries || len(cb.raw) >= format.IndexChunkTargetRawBytes {
			if err := cc.add(cb); err != nil {
				return err
			}
			cb = &chunkBuild{kind: kind, firstOrdinal: uint32(i + 1)}
			restart()
		}
	}
	if cb.entries > 0 {
		return cc.add(cb)
	}
	return nil
}

// emitChunks splits the builder's metadata and block entry streams into
// delta-encoded chunks. The snapshot chunk head is reserved (not yet filled);
// snapshot, metadata and block chunks are emitted here in frozen order. Row
// entries are NOT chunked: they are written as sorted Row Index Pages + a
// Fence Directory by `buildPages`, and `cc.seq` is left at the next free
// chunk sequence so pages can seal under distinct chunk sequences.
func (b *Builder) emitChunks(cc *chunkWriter) error {
	meta := &metaEncoder{}
	if err := emitEntryChunks(cc, format.IndexChunkKindMetadata, len(b.metadata),
		func(dst []byte, i int) []byte { return meta.add(dst, b.metadata[i]) },
		meta.restart); err != nil {
		return err
	}
	blk := &blockEncoder{}
	return emitEntryChunks(cc, format.IndexChunkKindBlock, len(b.blocks),
		func(dst []byte, i int) []byte { return blk.add(dst, b.blocks[i]) },
		blk.restart)
}

// BodyBounds is the resolved layout of one stored index-txn body: the
// snapshot's data byte range plus the txn's own file offsets.
type BodyBounds struct {
	DataStart uint64
	DataEnd   uint64
	TxnStart  int64
	TxnEnd    int64
}

// BoundsResolver resolves the snapshot entry's layout bounds once the final
// stored body length is known.
type BoundsResolver func(bodyLen int) BodyBounds

// BuildStoredBody serializes the builder's entries into the stored body
// (chunk headers + payloads + directory + Row Index Pages + Fence Directory,
// WITHOUT the IndexTxnHeader and IndexTxnFooter) and returns it together with
// the plaintext-body CRC and the materialized Txn. resolveBounds receives the
// final stored body length and returns the snapshot's bounds; the snapshot
// chunk's stored size is fixed at 72 bytes, so the resolution is stable in one
// pass. A nil resolver passes the snapshot entry's own values through (tests).
//
// Body layout:
//
//	[SnapshotChunk][MetadataChunks][BlockChunks][ChunkDirectory]
//	[IndexPage × N][RowIndexFenceEntry × N]
func (b *Builder) BuildStoredBody(crypto *ChunkCrypto, level int, resolveBounds BoundsResolver) (body []byte, plainCRC uint32, txn *Txn, err error) {
	if b.snapshot == nil {
		return nil, 0, nil, errors.New("rowpack: no snapshot entry to build")
	}
	cc := newChunkCompressor(crypto, level)
	if err := b.emitChunks(cc); err != nil {
		return nil, 0, nil, err
	}
	// Build the sorted Row Index Pages + Fence Directory. pageSeqBase is the
	// next free chunk sequence so pages seal under distinct nonces.
	pages, err := b.buildPages(crypto, level, cc.seq)
	if err != nil {
		return nil, 0, nil, err
	}
	pageRegionLen := 0
	for i := range pages {
		pageRegionLen += len(pages[i].stored)
	}
	fenceRegionLen := len(pages) * format.IndexFenceEntrySize
	// +32B: the snapshot chunk's directory entry (always present) is
	// prepended after the bounds resolve, so it is counted here. The
	// snapshot chunk payload itself is already counted in the reserved head.
	bodyLen := len(cc.out) + len(cc.dir) + format.IndexChunkDirEntrySize + pageRegionLen + fenceRegionLen
	bounds := BodyBounds{DataStart: b.snapshot.DataStart, DataEnd: b.snapshot.DataEnd}
	if resolveBounds != nil {
		bounds = resolveBounds(bodyLen)
	}
	se := *b.snapshot
	se.DataStart = bounds.DataStart
	se.DataEnd = bounds.DataEnd
	var raw [format.SnapshotIndexEntrySize]byte
	_ = se.MarshalTo(raw[:]) // exact-size buffer: cannot fail
	stored := raw[:]
	encryption := format.IndexChunkEncryptionNone
	if crypto != nil {
		sealed, err := crypto.Seal(0, format.IndexChunkKindSnapshot, 0, len(raw), stored)
		if err != nil {
			return nil, 0, nil, err
		}
		stored = sealed
		encryption = format.IndexChunkEncryptionAESGCM
	}
	if len(stored) != cc.snapPayload {
		return nil, 0, nil, fmt.Errorf("rowpack: snapshot chunk stored %d bytes, want fixed %d", len(stored), cc.snapPayload)
	}
	keyEpoch := uint32(0)
	if crypto != nil {
		keyEpoch = crypto.Epoch
	}
	h := format.IndexChunkHeader{
		EntryKind:     format.IndexChunkKindSnapshot,
		Encryption:    encryption,
		KeyEpoch:      keyEpoch,
		ChunkSequence: 0,
		EntryCount:    1,
		RawBytes:      uint32(len(raw)),
		StoredBytes:   uint32(len(stored)),
		PayloadCRC32C: format.CRC32C(stored),
	}
	_ = h.MarshalTo(cc.out[:format.IndexChunkHeaderSize]) // exact-size buffer: cannot fail
	copy(cc.out[format.IndexChunkHeaderSize:], stored)
	// Snapshot directory entry first, then the streamed chunks' entries; the
	// directory bytes join the plaintext CRC.
	var sde format.IndexChunkDirEntry
	sde.ChunkSequence = 0
	sde.EntryCount = 1
	sde.RawBytes = h.RawBytes
	sde.StoredBytes = h.StoredBytes
	sde.EntryKind = format.IndexChunkKindSnapshot
	sde.RegionOffset = 0
	var sdeBuf [format.IndexChunkDirEntrySize]byte
	_ = sde.MarshalTo(sdeBuf[:]) // exact-size buffer: cannot fail
	cc.dir = append(sdeBuf[:], cc.dir...)
	// Patch each fence's StoredOffset now that the chunk region + directory
	// length is final, and serialize the fence directory.
	pageOff := uint64(len(cc.out) + len(cc.dir))
	fenceBytes := make([]byte, 0, fenceRegionLen)
	var fbuf [format.IndexFenceEntrySize]byte
	for i := range pages {
		pages[i].fence.StoredOffset = pageOff
		pageOff += uint64(len(pages[i].stored))
		_ = pages[i].fence.MarshalTo(fbuf[:]) // exact-size buffer: cannot fail
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
		plainCRC = format.CRC32CConcat(plainCRC, p)
	}
	body = cc.out
	body = append(body, cc.dir...)
	for i := range pages {
		body = append(body, pages[i].stored...)
	}
	body = append(body, fenceBytes...)
	txn = &Txn{Snapshot: se, Metadata: b.metadata, Blocks: b.blocks, Rows: b.rows}
	txn.dataStart, txn.dataEnd, txn.txnStart, txn.txnEnd = bounds.DataStart, bounds.DataEnd, bounds.TxnStart, bounds.TxnEnd
	return body, plainCRC, txn, nil
}

// AssembleTxn wraps a stored chunk body with the IndexTxnHeader and
// IndexTxnFooter. h.BodyBytes is forced to len(body); keyEpoch != 0 is
// stamped into the header reserved word (encrypted stores).
func AssembleTxn(h format.IndexTxnHeader, keyEpoch uint32, body []byte, f format.IndexTxnFooter) ([]byte, error) {
	h.BodyBytes = uint64(len(body))
	out := make([]byte, 0, format.IndexTxnHeaderSize+len(body)+format.IndexTxnFooterSize)
	var hb [format.IndexTxnHeaderSize]byte
	_ = h.MarshalTo(hb[:]) // exact-size buffer: cannot fail
	if keyEpoch != 0 {
		if err := format.PatchIndexTxnHeaderForStorage(hb[:], uint64(len(body)), keyEpoch); err != nil {
			return nil, err
		}
	}
	out = append(out, hb[:]...)
	out = append(out, body...)
	var fb [format.IndexTxnFooterSize]byte
	_ = f.MarshalTo(fb[:]) // exact-size buffer: cannot fail
	out = append(out, fb[:]...)
	return out, nil
}

// ---- chunked body parsing (read path) ----

// storedBody is the decoded content of one chunked txn body.
type storedBody struct {
	metadata   []format.MetadataIndexEntry
	blocks     []format.BlockIndexEntry
	rows       []format.RowIndexEntry // buffered mode only; nil when streaming
	snapshot   format.SnapshotIndexEntry
	hasSnap    bool
	dir        []format.IndexChunkDirEntry
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
	SetSnapshot(e format.SnapshotIndexEntry) error
	AddMetadata(e format.MetadataIndexEntry) error
	AddBlock(e format.BlockIndexEntry) error
	AddRows(batch []format.RowIndexEntry) error
}

// RowHintSink is an optional TxnSink extension: the parser reports the
// header's row entry count (bounded to the prealloc guard) before the first
// row so the sink can preallocate exactly.
type RowHintSink interface {
	ReserveRows(hint int)
}

// rowBatchSink is the optional bulk path for the Eager Open: the parser
// decodes a whole index page into a reusable columnar pageRows and hands it
// over in one call, instead of invoking a callback and an interface method
// per entry.
type rowBatchSink interface {
	AddRowBatch(b *pageRows, snapshotID uint64) error
}

// RowEntrySink is an optional TxnSink extension for row entries: instead of
// handing the parser bounded []RowIndexEntry batches via AddRows, the sink
// receives each decoded entry individually via AddRowEntry. The page decoder
// (walkPage) feeds entries straight to this sink — one at a time, in
// (TableID, RowID) sorted order — so the Eager rowShard builder appends into
// its columnar arrays without materializing a []RowIndexEntry page or a
// []RowKeyLoc intermediate.
type RowEntrySink interface {
	AddRowEntry(e format.RowIndexEntry) error
}

// FenceCaptureSink is an optional TxnSink extension for the Lazy index mode:
// the parser hands the sink the validated Row Index Fence Directory and then
// stops WITHOUT decoding any page payload (no page is OPENed/decompressed).
// Implementing it makes pageParser skip page decoding, so a Lazy Open
// never materializes a page and never touches the row bytes — only the 52 B
// fence entries become resident. The sink must run count validation itself.
type FenceCaptureSink interface {
	SetRowIndexFences(fences []format.RowIndexFenceEntry) error
}

// bufferedSink is the default TxnSink collecting into storedBody slices
// (historical behavior).
type bufferedSink struct{ sb *storedBody }

func (s bufferedSink) SetSnapshot(e format.SnapshotIndexEntry) error {
	if s.sb.hasSnap {
		return fmt.Errorf("rowpack: duplicate snapshot chunk")
	}
	s.sb.snapshot = e
	s.sb.hasSnap = true
	return nil
}

func (s bufferedSink) AddMetadata(e format.MetadataIndexEntry) error {
	s.sb.metadata = append(s.sb.metadata, e)
	return nil
}

func (s bufferedSink) AddBlock(e format.BlockIndexEntry) error {
	s.sb.blocks = append(s.sb.blocks, e)
	return nil
}

func (s bufferedSink) AddRows(batch []format.RowIndexEntry) error {
	s.sb.rows = append(s.sb.rows, batch...)
	return nil
}

// bodyParser holds the txn-scoped inputs of one chunked-body parse. The
// counters are capacity hints only (the caller bounds them against the header
// before use); crypto must be non-nil iff the chunks are encrypted.
type bodyParser struct {
	region            []byte
	snapshotID        uint64
	metadataCount     int
	blockCount        int
	rowCount          int
	rowIndexPageCount uint32
	crypto            *ChunkCrypto
	sink              TxnSink
}

// parse walks the chunk sequence and directory, authenticating and decoding
// every chunk. A non-nil sink receives every entry as it is decoded (streaming
// mode; rows are never buffered); a nil sink collects everything into the
// returned storedBody.
func (p *bodyParser) parse() (*storedBody, error) {
	sb := &storedBody{
		plainCRC: format.CRC32C(nil),
	}
	sink := p.sink
	if sink == nil {
		sb.metadata = make([]format.MetadataIndexEntry, 0, p.metadataCount)
		sb.blocks = make([]format.BlockIndexEntry, 0, p.blockCount)
		sb.rows = make([]format.RowIndexEntry, 0, p.rowCount)
		sink = bufferedSink{sb: sb}
	}
	pos := 0
	seq := uint32(0)
	nextOrd := map[uint8]uint32{}
	for pos < len(p.region) {
		if !bytes.HasPrefix(p.region[pos:], []byte(format.MagicIndexChunkHdr)) {
			break // directory region follows
		}
		if pos+format.IndexChunkHeaderSize > len(p.region) {
			return nil, fmt.Errorf("rowpack: chunk %d header truncated", seq)
		}
		var h format.IndexChunkHeader
		if err := h.Unmarshal(p.region[pos:]); err != nil {
			return nil, fmt.Errorf("rowpack: chunk %d header: %w", seq, err)
		}
		if err := h.CheckLimits(); err != nil {
			return nil, err
		}
		if h.ChunkSequence != seq {
			return nil, fmt.Errorf("rowpack: chunk sequence %d, want %d", h.ChunkSequence, seq)
		}
		if (p.crypto != nil) != (h.Encryption == format.IndexChunkEncryptionAESGCM) {
			return nil, fmt.Errorf("rowpack: chunk %d encryption %d inconsistent with store", seq, h.Encryption)
		}
		chunkEnd := pos + format.IndexChunkHeaderSize + int(h.StoredBytes)
		if chunkEnd > len(p.region) {
			return nil, fmt.Errorf("rowpack: chunk %d payload %d bytes overruns body %d", seq, h.StoredBytes, len(p.region))
		}
		stored := p.region[pos+format.IndexChunkHeaderSize : chunkEnd]
		if format.CRC32C(stored) != h.PayloadCRC32C {
			return nil, fmt.Errorf("rowpack: chunk %d payload CRC mismatch", seq)
		}
		var raw []byte
		if h.Encryption == format.IndexChunkEncryptionAESGCM {
			if h.KeyEpoch != p.crypto.Epoch {
				return nil, fmt.Errorf("rowpack: chunk %d key epoch %d, want %d", seq, h.KeyEpoch, p.crypto.Epoch)
			}
			pt, err := p.crypto.Open(h.ChunkSequence, h.EntryKind, h.FirstEntryOrdinal, int(h.RawBytes), stored)
			if err != nil {
				return nil, fmt.Errorf("rowpack: chunk %d open: %w", seq, err)
			}
			raw = pt
		} else {
			raw = stored
		}
		if h.Compression == format.IndexChunkCompressionZstd {
			pt, err := block.Decompress(format.CompressionZstd, nil, raw, h.RawBytes)
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
		case format.IndexChunkKindSnapshot:
			if h.EntryCount != 1 || len(raw) != format.SnapshotIndexEntrySize {
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
		case format.IndexChunkKindMetadata:
			err := decodeMetadataChunk(raw, h.EntryCount, p.snapshotID, func(e format.MetadataIndexEntry) error {
				return sink.AddMetadata(e)
			})
			if err != nil {
				return nil, fmt.Errorf("rowpack: metadata chunk %d: %w", seq, err)
			}
			sb.metaCount += h.EntryCount
		case format.IndexChunkKindBlock:
			err := decodeBlockChunk(raw, h.EntryCount, p.snapshotID, func(e format.BlockIndexEntry) error {
				return sink.AddBlock(e)
			})
			if err != nil {
				return nil, fmt.Errorf("rowpack: block chunk %d: %w", seq, err)
			}
			sb.blockCount += h.EntryCount
		case format.IndexChunkKindRow:
			return nil, fmt.Errorf("rowpack: row chunk %d: obsolete row-chunk layout (the row index is stored as sorted Index Pages; refusing to decode)", seq)
		}
		// No default arm: h.Unmarshal has already validated the kind enum, so
		// the four cases above are exhaustive.
		nextOrd[h.EntryKind] += h.EntryCount
		sb.plainCRC = format.CRC32CConcat(sb.plainCRC, raw)
		pos += format.IndexChunkHeaderSize + int(h.StoredBytes)
		seq++
	}
	// Directory: after the last chunk, region[pos:] = directory (seq*32) + row
	// index pages + fence directory. The directory is plaintext (even in
	// encrypted stores) so the scanner can reach it without a key.
	rest := p.region[pos:]
	dirLen := int(seq) * format.IndexChunkDirEntrySize
	if len(rest) < dirLen || dirLen == 0 {
		return nil, fmt.Errorf("rowpack: chunk directory %d bytes malformed", len(rest))
	}
	dirBytes := rest[:dirLen]
	// The directory region is exactly seq*32 bytes and each 32-byte entry
	// unmarshals unconditionally (no CRC, no per-field bounds checks beyond
	// the size word), so Parse cannot fail here and always yields seq entries.
	dir, _ := format.ParseIndexChunkDirectory(dirBytes)
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
		var h format.IndexChunkHeader
		// The same header bytes unmarshalled successfully in the walk above.
		_ = h.Unmarshal(p.region[checkPos:])
		if de.EntryKind != h.EntryKind || de.EntryCount != h.EntryCount ||
			de.FirstEntryOrdinal != h.FirstEntryOrdinal || de.RawBytes != h.RawBytes || de.StoredBytes != h.StoredBytes {
			return nil, fmt.Errorf("rowpack: directory entry %d disagrees with chunk header", i)
		}
		checkPos += format.IndexChunkHeaderSize + int(h.StoredBytes)
	}
	sb.dir = dir
	sb.plainCRC = format.CRC32CConcat(sb.plainCRC, dirBytes)
	sb.chunkCount = seq
	crc, rows, err := (&pageParser{
		region:     p.region,
		pageStart:  pos + dirLen,
		pageCount:  p.rowIndexPageCount,
		snapshotID: p.snapshotID,
		crypto:     p.crypto,
		sink:       sink,
		seq:        seq,
		plainCRC:   sb.plainCRC,
		rowCount:   sb.rowCount,
	}).parse()
	if err != nil {
		return nil, err
	}
	sb.plainCRC, sb.rowCount = crc, rows
	return sb, nil
}

// parseFences parses and validates the Row Index Fence Directory that follows
// the pages region of the txn body. It returns the fences and the absolute
// offset where the fence directory begins (= where the pages region ends), so
// the Lazy path can stop before decoding any page payload.
func (p *pageParser) parseFences() (fences []format.RowIndexFenceEntry, pageEnd int, err error) {
	if p.pageCount == 0 {
		return nil, p.pageStart, nil
	}
	fenceLen := int(p.pageCount) * format.IndexFenceEntrySize
	if p.pageStart < 0 || p.pageStart >= len(p.region) || fenceLen > len(p.region)-p.pageStart {
		return nil, 0, fmt.Errorf("rowpack: row index fence directory %d bytes exceeds body %d", fenceLen, len(p.region)-p.pageStart)
	}
	pageEnd = len(p.region) - fenceLen
	if pageEnd < p.pageStart {
		return nil, 0, fmt.Errorf("rowpack: row index pages region %d..%d malformed", p.pageStart, pageEnd)
	}
	fenceRegion := p.region[pageEnd:]
	fences = make([]format.RowIndexFenceEntry, int(p.pageCount))
	for i := range fences {
		off := i * format.IndexFenceEntrySize
		// Each slice is exactly IndexFenceEntrySize bytes; the only failing
		// case is the zero-entry guard, which the cohesion check below covers.
		_ = fences[i].Unmarshal(fenceRegion[off : off+format.IndexFenceEntrySize])
	}
	// Fence cohesion: snapshot ownership, strictly ordered and contiguous
	// within the pages p.region, with non-zero sizes.
	expectOff := uint64(p.pageStart)
	for i := range fences {
		f := &fences[i]
		if f.SnapshotID != p.snapshotID {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d snapshot %d, want %d", i, f.SnapshotID, p.snapshotID)
		}
		if f.StoredSize == 0 || f.RawSize == 0 || f.EntryCount == 0 {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d zero size/entry", i)
		}
		if f.RawSize < format.IndexPageHeaderSize {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d raw size %d below page header %d", i, f.RawSize, format.IndexPageHeaderSize)
		}
		if f.StoredOffset < uint64(p.pageStart) || f.StoredOffset > uint64(pageEnd) ||
			uint64(f.StoredSize) > uint64(pageEnd)-f.StoredOffset {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d page out of bounds", i)
		}
		if f.StoredOffset != expectOff {
			return nil, 0, fmt.Errorf("rowpack: row index fence %d offset %d, want %d", i, f.StoredOffset, expectOff)
		}
		expectOff += uint64(f.StoredSize)
	}
	if expectOff != uint64(pageEnd) {
		return nil, 0, fmt.Errorf("rowpack: row index pages span %d bytes, want %d", expectOff-uint64(p.pageStart), pageEnd-p.pageStart)
	}
	return fences, pageEnd, nil
}

// pageParser holds the txn-scoped inputs of one Row Index Pages region
// decode plus the running accumulators. region is the full body and pageStart
// the absolute offset (within region) where the pages region begins; crypto
// must be non-nil iff the pages are sealed (encrypted store), and pages seal
// under the chunk sequence continuing past seq (the chunk count).
type pageParser struct {
	region     []byte
	pageStart  int
	pageCount  uint32
	snapshotID uint64
	crypto     *ChunkCrypto
	sink       TxnSink
	seq        uint32
	plainCRC   uint32
	rowCount   uint64
}

// parse decodes the Row Index Pages + Fence Directory that
// follow the chunk directory in a txn body, handing entries to the
// sink in RowID-sorted order in bounded batches. It returns the plaintext-body
// CRC extended with the raw page bytes and the fence bytes, and the row count
// accumulated from the pages. Forged page counts, offsets or sizes are
// rejected before any attacker-sized allocation.
func (p *pageParser) parse() (crc uint32, rows uint64, err error) {
	if p.pageCount == 0 {
		return p.plainCRC, p.rowCount, nil
	}
	// Fence directory parsed + validated first; pageEnd is where the fence
	// directory begins (= where the pages region ends).
	fences, pageEnd, err := p.parseFences()
	if err != nil {
		return 0, 0, err
	}
	// Lazy mode: the sink captures the fence and stops without decoding any
	// page payload. The running row count is derived from the fence; the
	// plaintext-body CRC is left incomplete (the Lazy Open deliberately skips
	// the full-body CRC — integrity comes from the stored-byte CRC plus each
	// page's own PageCRC32C / AEAD).
	if fs, ok := p.sink.(FenceCaptureSink); ok {
		if err := fs.SetRowIndexFences(fences); err != nil {
			return 0, 0, err
		}
		for i := range fences {
			p.rowCount += uint64(fences[i].EntryCount)
		}
		return 0, 0, nil
	}
	// Decode each page and hand its entries to the sink.
	var batch pageRows
	batchSink, batchOK := p.sink.(rowBatchSink)
	for i := range fences {
		f := &fences[i]
		stored := p.region[int(f.StoredOffset) : int(f.StoredOffset)+int(f.StoredSize)]
		raw := stored
		if p.crypto != nil {
			pt, err := p.crypto.Open(p.seq+uint32(i), rowIndexPageChunkKind, uint32(i), int(f.RawSize), stored)
			if err != nil {
				return 0, 0, fmt.Errorf("rowpack: row index page %d open: %w", i, err)
			}
			raw = pt
		}
		pageRaw, err := block.Decompress(format.CompressionZstd, nil, raw, f.RawSize)
		if err != nil {
			return 0, 0, fmt.Errorf("rowpack: row index page %d decompress: %w", i, err)
		}
		if uint32(len(pageRaw)) != f.RawSize {
			return 0, 0, fmt.Errorf("rowpack: row index page %d raw %d bytes, want %d", i, len(pageRaw), f.RawSize)
		}
		if format.CRC32C(pageRaw[format.IndexPageHeaderSize:]) != f.PageCRC32C {
			return 0, 0, fmt.Errorf("rowpack: row index page %d CRC mismatch", i)
		}
		// The page encodes (TableID, RowID, BlockID, ItemOrdinal, ChangeType)
		// without SnapshotID (txn-wide); stamp it before handing to the sink.
		// A batch sink receives the whole page as columns; a RowEntrySink
		// receives entries one at a time via walkPage — no []RowIndexEntry
		// page materialization either way, and no []RowKeyLoc intermediate.
		if batchOK {
			if err := decodePageInto(&batch, pageRaw); err != nil {
				return 0, 0, fmt.Errorf("rowpack: row index page %d: %w", i, err)
			}
			if uint32(len(batch.rowIDs)) != f.EntryCount {
				return 0, 0, fmt.Errorf("rowpack: row index page %d %d entries, fence says %d", i, len(batch.rowIDs), f.EntryCount)
			}
			if err := batchSink.AddRowBatch(&batch, p.snapshotID); err != nil {
				return 0, 0, fmt.Errorf("rowpack: row index page %d: %w", i, err)
			}
			p.rowCount += uint64(len(batch.rowIDs))
			p.plainCRC = format.CRC32CConcat(p.plainCRC, pageRaw)
			continue
		}
		if es, ok := p.sink.(RowEntrySink); ok {
			emitted := 0
			if err := walkPage(pageRaw, func(e format.RowIndexEntry) error {
				e.SnapshotID = p.snapshotID
				emitted++
				return es.AddRowEntry(e)
			}); err != nil {
				return 0, 0, fmt.Errorf("rowpack: row index page %d: %w", i, err)
			}
			if emitted != int(f.EntryCount) {
				return 0, 0, fmt.Errorf("rowpack: row index page %d %d entries, fence says %d", i, emitted, f.EntryCount)
			}
			p.rowCount += uint64(emitted)
			p.plainCRC = format.CRC32CConcat(p.plainCRC, pageRaw)
			continue
		}
		entries, err := decodePage(pageRaw)
		if err != nil {
			return 0, 0, fmt.Errorf("rowpack: row index page %d: %w", i, err)
		}
		if uint32(len(entries)) != f.EntryCount {
			return 0, 0, fmt.Errorf("rowpack: row index page %d %d entries, fence says %d", i, len(entries), f.EntryCount)
		}
		for j := range entries {
			entries[j].SnapshotID = p.snapshotID
		}
		p.rowCount += uint64(len(entries))
		p.plainCRC = format.CRC32CConcat(p.plainCRC, pageRaw)
		for start := 0; start < len(entries); start += rowBatchSize {
			end := min(start+rowBatchSize, len(entries))
			if err := p.sink.AddRows(entries[start:end]); err != nil {
				return 0, 0, fmt.Errorf("rowpack: row index page %d add rows: %w", i, err)
			}
		}
	}
	p.plainCRC = format.CRC32CConcat(p.plainCRC, p.region[pageEnd:])
	return p.plainCRC, p.rowCount, nil
}
