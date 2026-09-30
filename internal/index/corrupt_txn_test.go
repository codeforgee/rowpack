package index

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
)

// This file pins the chunk-walk validation arms of bodyParser.parse and
// parseTxnChunked. Each case starts from a valid framed txn (snapshot +
// metadata + block chunks and three row-index pages), mutates exactly one
// header/directory/fence/footer field — re-stamping the affected CRC — and
// expects a specific rejection. The directory and fences carry no CRC of
// their own, so those mutations are plain byte edits; chunk headers and the
// txn header/footer are re-marshaled so their CRCs stay valid and the
// targeted check is what fires.

// failingSink fails exactly one entry kind to drive the sink-error arms.
type failingSink struct {
	failMeta  bool
	failBlock bool
}

func (s *failingSink) SetSnapshot(format.SnapshotIndexEntry) error { return nil }
func (s *failingSink) AddMetadata(format.MetadataIndexEntry) error {
	if s.failMeta {
		return errors.New("sinkboom")
	}
	return nil
}
func (s *failingSink) AddBlock(format.BlockIndexEntry) error {
	if s.failBlock {
		return errors.New("sinkboom")
	}
	return nil
}
func (s *failingSink) AddRows(batch []format.RowIndexEntry) error { return nil }

// mustBuildTxn frames a valid txn containing a snapshot chunk, one metadata
// chunk, one block chunk and nRows row-index entries (3 pages at nRows=9000).
func mustBuildTxn(t *testing.T, nRows int) []byte {
	t.Helper()
	b := NewBuilder(1)
	b.SetRowDedup(false)
	if err := b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull, BlockCount: 2}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(format.MetadataIndexEntry{SnapshotID: 1, ObjectID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(format.BlockIndexEntry{BlockID: 11, SnapshotID: 1, TableID: 1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range riSeq(nRows, 4) {
		r.SnapshotID = 1
		if err := b.AddRow(r); err != nil {
			t.Fatal(err)
		}
	}
	data, _, err := b.Build(BodyBounds{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: the pristine txn must parse.
	if _, err := parseStream(data, nil, nil); err != nil {
		t.Fatalf("pristine txn rejected: %v", err)
	}
	return data
}

type chunkInfo struct {
	off   int // header offset
	raw   format.IndexChunkHeader
	paylo int // payload offset
}

// scanTxn walks the chunk region and locates the directory and the fence
// directory.
func scanTxn(t *testing.T, data []byte) (chunks []chunkInfo, dirOff, fenceOff int, pageCount uint32) {
	t.Helper()
	var th format.IndexTxnHeader
	if err := th.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	pageCount = th.RowIndexPageCount
	pos := format.IndexTxnHeaderSize
	for bytes.HasPrefix(data[pos:], []byte(format.MagicIndexChunkHdr)) {
		var h format.IndexChunkHeader
		if err := h.Unmarshal(data[pos:]); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunkInfo{off: pos, raw: h, paylo: pos + format.IndexChunkHeaderSize})
		pos += format.IndexChunkHeaderSize + int(h.StoredBytes)
	}
	dirOff = pos
	fenceOff = len(data) - format.IndexTxnFooterSize - int(pageCount)*format.IndexFenceEntrySize
	return chunks, dirOff, fenceOff, pageCount
}

// restampChunk edits chunk header fields in place, re-stamping the header
// CRC. The mut func may also fix up PayloadCRC32C when it rewrites payload
// bytes.
func restampChunk(t *testing.T, data []byte, off int, mut func(*format.IndexChunkHeader)) {
	t.Helper()
	var h format.IndexChunkHeader
	if err := h.Unmarshal(data[off:]); err != nil {
		t.Fatal(err)
	}
	mut(&h)
	if err := h.MarshalTo(data[off:]); err != nil {
		t.Fatal(err)
	}
}

// dirEntryAt returns the mutable slice of directory entry i.
func dirEntryAt(data []byte, dirOff, i int) []byte {
	off := dirOff + i*format.IndexChunkDirEntrySize
	return data[off : off+format.IndexChunkDirEntrySize]
}

// syncDirEntry rewrites directory entry i from the given header (offsets are
// recomputed from the entry index).
func syncDirEntry(t *testing.T, data []byte, dirOff, i int, seq uint32, h format.IndexChunkHeader) {
	t.Helper()
	e := format.IndexChunkDirEntry{
		ChunkSequence:     seq,
		EntryKind:         h.EntryKind,
		EntryCount:        h.EntryCount,
		FirstEntryOrdinal: h.FirstEntryOrdinal,
		RawBytes:          h.RawBytes,
		StoredBytes:       h.StoredBytes,
		RegionOffset:      uint64(i * (format.IndexChunkHeaderSize + int(h.StoredBytes))),
	}
	if err := e.MarshalTo(dirEntryAt(data, dirOff, i)); err != nil {
		t.Fatal(err)
	}
}

func restampTxnHeader(t *testing.T, data []byte, mut func(*format.IndexTxnHeader)) {
	t.Helper()
	var h format.IndexTxnHeader
	if err := h.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	mut(&h)
	if err := h.MarshalTo(data[:format.IndexTxnHeaderSize]); err != nil {
		t.Fatal(err)
	}
}

func restampTxnFooter(t *testing.T, data []byte, mut func(*format.IndexTxnFooter)) {
	t.Helper()
	off := len(data) - format.IndexTxnFooterSize
	var f format.IndexTxnFooter
	if err := f.Unmarshal(data[off:]); err != nil {
		t.Fatal(err)
	}
	mut(&f)
	if err := f.MarshalTo(data[off:]); err != nil {
		t.Fatal(err)
	}
}

func fenceAt(t *testing.T, data []byte, fenceOff, i int, mut func(*format.RowIndexFenceEntry)) {
	t.Helper()
	off := fenceOff + i*format.IndexFenceEntrySize
	var f format.RowIndexFenceEntry
	if err := f.Unmarshal(data[off:]); err != nil {
		t.Fatal(err)
	}
	mut(&f)
	if err := f.MarshalTo(data[off:]); err != nil {
		t.Fatal(err)
	}
}

// reframeTxn rebuilds the whole txn region from a scan result. For every
// chunk it takes the plaintext (decompressed) payload, optionally replaces
// it (payloads[i] != nil) and optionally edits the header (muts[i] != nil),
// then re-compresses, re-stamps header + directory CRCs, recomputes the
// footer body CRC over the new plaintext stream, and re-frames header and
// footer. This is the price of touching chunk payloads at all: stored bytes
// are zstd streams, so in-place edits cannot preserve geometry.
func reframeTxn(t *testing.T, data []byte, chunks []chunkInfo, dirOff, _ int, payloads [][]byte, muts []func(*format.IndexChunkHeader)) []byte {
	t.Helper()
	var out []byte
	var dir []byte
	crc := format.CRC32C(nil)
	for i, c := range chunks {
		stored := data[c.paylo : c.paylo+int(c.raw.StoredBytes)]
		raw := stored
		if c.raw.Compression == format.IndexChunkCompressionZstd {
			var err error
			raw, err = block.Decompress(format.CompressionZstd, nil, stored, c.raw.RawBytes)
			require.Nil(t, err, "reframe: decompress chunk %d: %v", i, err)
		}
		if payloads != nil && payloads[i] != nil {
			raw = payloads[i]
		}
		h := c.raw
		if muts != nil && muts[i] != nil {
			muts[i](&h)
		}
		comp, err := block.Compress(format.CompressionZstd, 0, raw)
		require.Nil(t, err, "reframe: compress chunk %d: %v", i, err)
		h.Compression = format.IndexChunkCompressionZstd
		h.RawBytes = uint32(len(raw))
		h.StoredBytes = uint32(len(comp))
		h.PayloadCRC32C = format.CRC32C(comp)
		hb := make([]byte, format.IndexChunkHeaderSize)
		if err := h.MarshalTo(hb); err != nil {
			t.Fatal(err)
		}
		out = append(out, hb...)
		out = append(out, comp...)
		crc = format.CRC32CConcat(crc, raw)
		e := format.IndexChunkDirEntry{
			ChunkSequence:     uint32(i),
			EntryKind:         h.EntryKind,
			EntryCount:        h.EntryCount,
			FirstEntryOrdinal: h.FirstEntryOrdinal,
			RawBytes:          h.RawBytes,
			StoredBytes:       h.StoredBytes,
			RegionOffset:      uint64(len(out) - format.IndexChunkHeaderSize - len(comp)),
		}
		eb := make([]byte, format.IndexChunkDirEntrySize)
		if err := e.MarshalTo(eb); err != nil {
			t.Fatal(err)
		}
		dir = append(dir, eb...)
	}
	out = append(out, dir...)
	crc = format.CRC32CConcat(crc, dir)
	// Pages + fences: copied verbatim, but the plaintext page bytes feed the
	// body CRC in page order, then the fence directory. Fence StoredOffsets
	// are relative to the region (they skip the txn header).
	base := uint64(format.IndexTxnHeaderSize)
	var th format.IndexTxnHeader
	if err := th.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	end := len(data) - format.IndexTxnFooterSize
	tail := data[dirOff+int(th.RowIndexPageCount)*format.IndexChunkDirEntrySize : end]
	fenceBytes := tail[len(tail)-int(th.RowIndexPageCount)*format.IndexFenceEntrySize:]
	for i := 0; i < int(th.RowIndexPageCount); i++ {
		var f format.RowIndexFenceEntry
		if err := f.Unmarshal(fenceBytes[i*format.IndexFenceEntrySize:]); err != nil {
			t.Fatal(err)
		}
		stored := data[base+f.StoredOffset : base+f.StoredOffset+uint64(f.StoredSize)]
		raw, err := block.Decompress(format.CompressionZstd, nil, stored, f.RawSize)
		require.Nil(t, err, "reframe: decompress page %d: %v", i, err)
		crc = format.CRC32CConcat(crc, raw)
	}
	crc = format.CRC32CConcat(crc, fenceBytes)
	out = append(out, tail...)

	// Re-frame with matching header/footer.
	hb := make([]byte, format.IndexTxnHeaderSize)
	copy(hb, data[:format.IndexTxnHeaderSize])
	var th2 format.IndexTxnHeader
	if err := th2.Unmarshal(hb); err != nil {
		t.Fatal(err)
	}
	th2.BodyBytes = uint64(len(out))
	if err := th2.MarshalTo(hb); err != nil {
		t.Fatal(err)
	}
	fob := len(data) - format.IndexTxnFooterSize
	var f format.IndexTxnFooter
	if err := f.Unmarshal(data[fob:]); err != nil {
		t.Fatal(err)
	}
	f.BodyCRC32C = crc
	fb := make([]byte, format.IndexTxnFooterSize)
	if err := f.MarshalTo(fb); err != nil {
		t.Fatal(err)
	}
	return append(append(hb, out...), fb...)
}

func TestTxnParseRejectsCorruptions(t *testing.T) {
	data := mustBuildTxn(t, 9000)
	chunks, dirOff, fenceOff, pageCount := scanTxn(t, data)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks (snapshot, metadata, block), got %d", len(chunks))
	}
	require.Equal(t, uint32(3), pageCount, "want 3 row index pages, got %d", pageCount)

	// mut returns the (possibly re-framed) txn bytes.
	cases := []struct {
		name string
		mut  func(t *testing.T, d []byte) []byte
		want string
	}{
		// --- chunk header (chunk 0 = snapshot) ---
		{"chunk header CRC", func(t *testing.T, d []byte) []byte {
			d[format.IndexTxnHeaderSize+20] ^= 0xFF
			return d
		}, "chunk 0 header"},
		{"chunk entry count zero", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) { h.EntryCount = 0 })
			return d
		}, "entry count 0 outside"},
		{"chunk sequence mismatch", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) { h.ChunkSequence = 7 })
			return d
		}, "chunk sequence 7, want 0"},
		{"encryption flag inconsistent", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) {
				// Zstd keeps CheckLimits from equating stored and raw; raw is
				// shrunk below stored-16 so the tag guard passes too.
				h.Encryption = format.IndexChunkEncryptionAESGCM
				h.Compression = format.IndexChunkCompressionZstd
				h.RawBytes = h.StoredBytes - format.AESGCMTagLen
			})
			return d
		}, "encryption 1 inconsistent with store"},
		{"payload overruns body", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) {
				h.Compression = format.IndexChunkCompressionZstd // skip the none==raw check
				h.StoredBytes = format.IndexChunkMaxStoredBytes - 1
			})
			return d
		}, "overruns body"},
		{"payload CRC mismatch", func(t *testing.T, d []byte) []byte {
			d[chunks[0].paylo+5] ^= 0xFF
			return d
		}, "chunk 0 payload CRC mismatch"},
		{"first ordinal mismatch", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) { h.FirstEntryOrdinal = 9 })
			return d
		}, "first ordinal 9, want 0"},
		{"snapshot chunk entry count", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) { h.EntryCount = 2 })
			return d
		}, "snapshot chunk 0: 2 entries"},
		{"zstd garbage payload", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, format.IndexTxnHeaderSize, func(h *format.IndexChunkHeader) {
				h.Compression = format.IndexChunkCompressionZstd
			})
			return d
		}, "chunk 0 decompress"},
		{"zstd raw size mismatch", func(t *testing.T, d []byte) []byte {
			raw := append([]byte(nil), d[chunks[0].paylo:chunks[0].paylo+int(chunks[0].raw.StoredBytes)]...)
			comp, err := block.Compress(format.CompressionZstd, 0, raw)
			if err != nil {
				t.Fatal(err)
			}
			copy(d[chunks[0].paylo:], comp)
			restampChunk(t, d, chunks[0].off, func(h *format.IndexChunkHeader) {
				h.Compression = format.IndexChunkCompressionZstd
				h.RawBytes += 1
				h.StoredBytes = uint32(len(comp))
				h.PayloadCRC32C = format.CRC32C(comp)
			})
			return d
		}, "chunk 0 raw"},

		// --- chunk 1 (metadata) and chunk 2 (block) ---
		{"duplicate snapshot chunk", func(t *testing.T, d []byte) []byte {
			// Turn the metadata chunk into a second snapshot chunk (payload is
			// the snapshot entry plaintext, re-compressed by reframeTxn). The
			// snapshot chunk is stored uncompressed, so the plaintext is the
			// stored bytes themselves.
			snapPlain := append([]byte(nil), d[chunks[0].paylo:chunks[0].paylo+int(chunks[0].raw.StoredBytes)]...)
			payloads := [][]byte{nil, snapPlain, nil}
			muts := []func(*format.IndexChunkHeader){
				nil,
				func(h *format.IndexChunkHeader) {
					h.EntryKind = format.IndexChunkKindSnapshot
					h.EntryCount = 1
					h.FirstEntryOrdinal = 1 // nextOrd[snapshot] after chunk 0
				},
				nil,
			}
			return reframeTxn(t, d, chunks, dirOff, fenceOff, payloads, muts)
		}, "duplicate snapshot chunk 1"},
		{"metadata entry count mismatch", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, chunks[1].off, func(h *format.IndexChunkHeader) { h.EntryCount = 2 })
			syncDirEntry(t, d, dirOff, 1, 1, chunks[1].raw)
			return d
		}, "metadata chunk 1: 48 bytes for 2 entries"},
		{"metadata entry corrupt", func(t *testing.T, d []byte) []byte {
			// Flip a byte inside the decompressed metadata entry, then let
			// reframeTxn re-stamp every CRC: the entry-level CRC check fires.
			plain, err := block.Decompress(format.CompressionZstd, nil,
				d[chunks[1].paylo:chunks[1].paylo+int(chunks[1].raw.StoredBytes)], chunks[1].raw.RawBytes)
			if err != nil {
				t.Fatal(err)
			}
			plain[3] ^= 0xFF
			return reframeTxn(t, d, chunks, dirOff, fenceOff, [][]byte{nil, plain, nil}, nil)
		}, "metadata chunk 1 entry 0"},
		{"block chunk size mismatch", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, chunks[2].off, func(h *format.IndexChunkHeader) { h.EntryCount = 2 })
			syncDirEntry(t, d, dirOff, 2, 2, chunks[2].raw)
			return d
		}, "block chunk 2: 56 bytes for 2 entries"},
		{"block entry corrupt", func(t *testing.T, d []byte) []byte {
			plain, err := block.Decompress(format.CompressionZstd, nil,
				d[chunks[2].paylo:chunks[2].paylo+int(chunks[2].raw.StoredBytes)], chunks[2].raw.RawBytes)
			if err != nil {
				t.Fatal(err)
			}
			plain[3] ^= 0xFF
			return reframeTxn(t, d, chunks, dirOff, fenceOff, [][]byte{nil, nil, plain}, nil)
		}, "block chunk 2 entry 0"},
		{"obsolete row chunk kind", func(t *testing.T, d []byte) []byte {
			restampChunk(t, d, chunks[2].off, func(h *format.IndexChunkHeader) {
				h.EntryKind = format.IndexChunkKindRow
				h.EntryCount = 1
			})
			syncDirEntry(t, d, dirOff, 2, 2, chunks[2].raw)
			return d
		}, "obsolete row-chunk layout"},

		// --- directory (plaintext, no CRC) ---
		{"directory truncated", func(t *testing.T, d []byte) []byte {
			// Keep header + chunks, replace everything after them with 16
			// bytes of junk, keep the original footer: the dir-length check
			// fires long before the fence geometry or body CRC matter.
			out := append([]byte(nil), d[:dirOff]...)
			out = append(out, make([]byte, 16)...)
			out = append(out, d[len(d)-format.IndexTxnFooterSize:]...)
			restampTxnHeader(t, out, func(h *format.IndexTxnHeader) {
				h.BodyBytes = uint64(len(out) - format.IndexTxnHeaderSize - format.IndexTxnFooterSize)
			})
			return out
		}, "chunk directory 16 bytes malformed"},
		{"directory entry sequence", func(t *testing.T, d []byte) []byte {
			var e format.IndexChunkDirEntry
			if err := e.Unmarshal(dirEntryAt(d, dirOff, 0)); err != nil {
				t.Fatal(err)
			}
			e.ChunkSequence = 5
			if err := e.MarshalTo(dirEntryAt(d, dirOff, 0)); err != nil {
				t.Fatal(err)
			}
			return d
		}, "directory entry 0 sequence 5"},
		{"directory entry offset", func(t *testing.T, d []byte) []byte {
			var e format.IndexChunkDirEntry
			if err := e.Unmarshal(dirEntryAt(d, dirOff, 1)); err != nil {
				t.Fatal(err)
			}
			e.RegionOffset += 1
			if err := e.MarshalTo(dirEntryAt(d, dirOff, 1)); err != nil {
				t.Fatal(err)
			}
			return d
		}, "directory entry 1 offset"},
		{"directory entry disagrees", func(t *testing.T, d []byte) []byte {
			var e format.IndexChunkDirEntry
			if err := e.Unmarshal(dirEntryAt(d, dirOff, 0)); err != nil {
				t.Fatal(err)
			}
			e.EntryCount = 9
			if err := e.MarshalTo(dirEntryAt(d, dirOff, 0)); err != nil {
				t.Fatal(err)
			}
			return d
		}, "disagrees with chunk header"},

		// --- row index fences / pages (3 pages; fence 1 sits mid-run, so an
		// 8-byte shift stays inside the fence bounds checks and hits the
		// exact-offset check instead) ---
		{"fence directory overrun", func(t *testing.T, d []byte) []byte {
			restampTxnHeader(t, d, func(h *format.IndexTxnHeader) { h.RowIndexPageCount = 1 << 20 })
			return d
		}, "row index fence directory"},
		{"fence offset moved", func(t *testing.T, d []byte) []byte {
			fenceAt(t, d, fenceOff, 1, func(f *format.RowIndexFenceEntry) { f.StoredOffset += 8 })
			return d
		}, "row index fence 1 offset"},
		{"fence raw size inflated", func(t *testing.T, d []byte) []byte {
			fenceAt(t, d, fenceOff, 0, func(f *format.RowIndexFenceEntry) { f.RawSize += 1 })
			return d
		}, "row index page 0 raw"},
		{"fence entry count inflated", func(t *testing.T, d []byte) []byte {
			fenceAt(t, d, fenceOff, 0, func(f *format.RowIndexFenceEntry) { f.EntryCount += 1 })
			return d
		}, "row index page 0"},

		// --- txn header / footer ---
		{"footer data end mismatch", func(t *testing.T, d []byte) []byte {
			restampTxnFooter(t, d, func(f *format.IndexTxnFooter) { f.DataSnapshotEnd += 1 })
			return d
		}, "data end mismatch"},
		{"body CRC mismatch", func(t *testing.T, d []byte) []byte {
			restampTxnFooter(t, d, func(f *format.IndexTxnFooter) { f.BodyCRC32C ^= 1 })
			return d
		}, "body CRC mismatch"},
		{"header metadata count inflated", func(t *testing.T, d []byte) []byte {
			restampTxnHeader(t, d, func(h *format.IndexTxnHeader) { h.MetadataEntryCount += 9 })
			return d
		}, "metadata entries, header says"},
		{"header row count inflated", func(t *testing.T, d []byte) []byte {
			restampTxnHeader(t, d, func(h *format.IndexTxnHeader) { h.RowEntryCount += 100 })
			return d
		}, "row entries, header says"},
		{"row count hint beyond prealloc bound", func(t *testing.T, d []byte) []byte {
			// Also drives the boundedCap clamp in parseTxnChunked.
			restampTxnHeader(t, d, func(h *format.IndexTxnHeader) { h.RowEntryCount = 1 << 40 })
			return d
		}, "row entries, header says"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mut(t, append([]byte(nil), data...))
			_, err := parseStream(mutated, nil, nil)
			require.Error(t, err, "corruption accepted")
			require.Contains(t, err.Error(), tc.want, "error %q, want substring %q", err, tc.want)
		})
	}
}

// TestTxnParseRejectsSinkErrors drives the streaming sink-error arms with a
// sink that fails metadata and block ingestion.
func TestTxnParseRejectsSinkErrors(t *testing.T) {
	data := mustBuildTxn(t, 0)
	sink := &failingSink{failMeta: true}
	if _, err := parseStream(data, nil, sink); err == nil || !strings.Contains(err.Error(), "sinkboom") {
		t.Fatalf("metadata sink error not surfaced: %v", err)
	}
	sink = &failingSink{failBlock: true}
	if _, err := parseStream(data, nil, sink); err == nil || !strings.Contains(err.Error(), "sinkboom") {
		t.Fatalf("block sink error not surfaced: %v", err)
	}
}

// TestTxnParseRejectsSnapshotIDMismatch uses a pages-free txn: with no row
// index pages the fence snapshot check cannot mask the txn-level snapshot id
// cross-check between the snapshot chunk and the header.
func TestTxnParseRejectsSnapshotIDMismatch(t *testing.T) {
	data := mustBuildTxn(t, 0)
	restampTxnHeader(t, data, func(h *format.IndexTxnHeader) { h.SnapshotID = 2 })
	restampTxnFooter(t, data, func(f *format.IndexTxnFooter) { f.SnapshotID = 2 })
	if _, err := parseStream(data, nil, nil); err == nil || !strings.Contains(err.Error(), "snapshot id mismatch") {
		t.Fatalf("want snapshot id mismatch, got %v", err)
	}
}

// TestTxnParseRejectsSnapshotlessBody frames a body whose only chunk is a
// metadata chunk: the parser must refuse a txn without a snapshot chunk.
func TestTxnParseRejectsSnapshotlessBody(t *testing.T) {
	// Assemble: header + [metadata chunk] + dir + footer.
	meta := format.MetadataIndexEntry{SnapshotID: 1, ObjectID: 7}
	metaRaw := make([]byte, format.MetadataIndexEntrySize)
	if err := meta.MarshalTo(metaRaw); err != nil {
		t.Fatal(err)
	}
	h := format.IndexChunkHeader{
		EntryKind:   format.IndexChunkKindMetadata,
		EntryCount:  1,
		RawBytes:    uint32(len(metaRaw)),
		StoredBytes: uint32(len(metaRaw)),
	}
	h.PayloadCRC32C = format.CRC32C(metaRaw)
	hdr := make([]byte, format.IndexChunkHeaderSize)
	if err := h.MarshalTo(hdr); err != nil {
		t.Fatal(err)
	}
	e := format.IndexChunkDirEntry{
		ChunkSequence: 0, EntryKind: h.EntryKind, EntryCount: h.EntryCount,
		RawBytes: h.RawBytes, StoredBytes: h.StoredBytes,
	}
	dir := make([]byte, format.IndexChunkDirEntrySize)
	if err := e.MarshalTo(dir); err != nil {
		t.Fatal(err)
	}
	region := append(append(append([]byte(nil), hdr...), metaRaw...), dir...)

	txnFtr := format.IndexTxnFooter{TxnSequence: 1, SnapshotID: 1}

	full := format.IndexTxnHeaderSize + len(region) + format.IndexTxnFooterSize
	txnHdr := format.IndexTxnHeader{
		TxnSequence: 1, SnapshotID: 1, MetadataEntryCount: 1,
		BodyBytes:         uint64(len(region)),
		DataSnapshotEnd:   uint64(full),
		RowIndexPageCount: 0,
	}
	txnFtr.DataSnapshotEnd = txnHdr.DataSnapshotEnd
	txnFtr.TxnStartOffset = 0
	txnFtr.TxnEndOffset = uint64(full)
	c := format.CRC32C(nil)
	c = format.CRC32CConcat(c, metaRaw)
	c = format.CRC32CConcat(c, dir)
	txnFtr.BodyCRC32C = c

	out := make([]byte, 0, full)
	hb := make([]byte, format.IndexTxnHeaderSize)
	if err := txnHdr.MarshalTo(hb); err != nil {
		t.Fatal(err)
	}
	fb := make([]byte, format.IndexTxnFooterSize)
	if err := txnFtr.MarshalTo(fb); err != nil {
		t.Fatal(err)
	}
	out = append(out, hb...)
	out = append(out, region...)
	out = append(out, fb...)

	if _, err := parseStream(out, nil, nil); err == nil || !strings.Contains(err.Error(), "no snapshot chunk") {
		t.Fatalf("snapshotless body accepted: %v", err)
	}
}
