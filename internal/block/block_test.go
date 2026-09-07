package block

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/metadata"
)

type capturedBlock struct {
	header  fileformat.BlockHeader
	payload []byte
}

type captureSink struct {
	blocks []capturedBlock
}

func (s *captureSink) flush(fb *FlushedBlock) error {
	s.blocks = append(s.blocks, capturedBlock{fb.Header, append([]byte(nil), fb.Stored...)})
	return nil
}

func mkRow(n int) []byte { return bytes.Repeat([]byte{byte(n % 251)}, n) }

func TestRowsBlockBuilderFlushAndDeterminism(t *testing.T) {
	for _, compress := range []fileformat.Compression{fileformat.CompressionNone, fileformat.CompressionZstd} {
		var s1, s2 captureSink
		build := func(sink *captureSink) error {
			b := NewRowsBlockBuilder(1, 7, 256<<10, compress, 3, DefaultLimits(), sink.flush)
			for i := 0; i < 1000; i++ {
				if err := b.Add(uint64(i), 1, fileformat.ChangeInsert, mkRow(200)); err != nil {
					return err
				}
			}
			return b.Flush()
		}
		if err := build(&s1); err != nil {
			t.Fatalf("%v: %v", compress, err)
		}
		if err := build(&s2); err != nil {
			t.Fatalf("%v: %v", compress, err)
		}
		if len(s1.blocks) == 0 {
			t.Fatal("no blocks flushed")
		}
		// Deterministic: two identical builds produce identical bytes.
		if len(s1.blocks) != len(s2.blocks) {
			t.Fatalf("%v: block count differs %d vs %d", compress, len(s1.blocks), len(s2.blocks))
		}
		for i := range s1.blocks {
			if !bytes.Equal(s1.blocks[i].payload, s2.blocks[i].payload) {
				t.Fatalf("%v: block %d payload not deterministic", compress, i)
			}
			if s1.blocks[i].header.RawSize != s1.blocks[i].header.StoredSize && compress == fileformat.CompressionNone {
				t.Fatal("None block stored != raw")
			}
		}
	}
}

func TestNoneVsZstdSameRows(t *testing.T) {
	var sNone, sZstd captureSink
	build := func(sink *captureSink, compress fileformat.Compression) {
		b := NewRowsBlockBuilder(1, 7, 4096, compress, 3, DefaultLimits(), sink.flush)
		for i := 0; i < 100; i++ {
			if err := b.Add(uint64(i), 1, fileformat.ChangeInsert, mkRow(64)); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	build(&sNone, fileformat.CompressionNone)
	build(&sZstd, fileformat.CompressionZstd)

	// Decode both and compare the logical rows.
	decode := func(s *captureSink) []string {
		var out []string
		for _, b := range s.blocks {
			raw, err := Decompress(b.header.Compression, nil, b.payload, DefaultLimits().MaxRawBytes)
			if err != nil {
				t.Fatal(err)
			}
			if fileformat.CRC32C(raw) != b.header.RawCRC32C {
				t.Fatal("raw CRC mismatch")
			}
			p, err := ParseRowsPayload(raw, b.header.ItemCount)
			if err != nil {
				t.Fatal(err)
			}
			for i := range p.Entries {
				out = append(out, fmt.Sprintf("%d/%d/%d/%x", p.Entries[i].RowID, p.Entries[i].ChangeType, p.Entries[i].SchemaVersion, p.RowBytes(i)))
			}
		}
		return out
	}
	a, b := decode(&sNone), decode(&sZstd)
	if len(a) != len(b) {
		t.Fatalf("row count differs: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("row %d differs:\n none: %s\n zstd: %s", i, a[i], b[i])
		}
	}
}

func TestOversizeSingleRowOwnBlock(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 4096, fileformat.CompressionZstd, 3, DefaultLimits(), s.flush)
	// A single row larger than the target block size.
	big := mkRow(10_000)
	if err := b.Add(1, 1, fileformat.ChangeInsert, big); err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) != 1 {
		t.Fatalf("oversize row produced %d blocks, want 1", len(s.blocks))
	}
	if s.blocks[0].header.ItemCount != 1 {
		t.Fatalf("oversize block has %d items", s.blocks[0].header.ItemCount)
	}
	if s.blocks[0].header.RawSize < 10_000 {
		t.Fatal("oversize block raw size too small")
	}
	// Subsequent small rows continue into new blocks.
	for i := 0; i < 100; i++ {
		if err := b.Add(uint64(100+i), 1, fileformat.ChangeInsert, mkRow(100)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	// Re-read the oversize row via the reader.
	rd := NewReader(bytes.NewReader(appendBlock(nil, &s.blocks[0])), DefaultLimits())
	blk, err := rd.ReadAtBlock(0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseRowsPayload(blk.Raw, blk.Header.ItemCount)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p.RowBytes(0), big) {
		t.Fatal("oversize row bytes differ after round trip")
	}
}

// appendBlock serializes a stored block (header + payload) for reader tests.
func appendBlock(dst []byte, cb *capturedBlock) []byte {
	var hdr [fileformat.BlockHeaderSize]byte
	_ = cb.header.MarshalTo(hdr[:])
	dst = append(dst, hdr[:]...)
	return append(dst, cb.payload...)
}

func TestReaderRejectsCorruption(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 4096, fileformat.CompressionZstd, 3, DefaultLimits(), s.flush)
	for i := 0; i < 50; i++ {
		if err := b.Add(uint64(i), 1, fileformat.ChangeInsert, mkRow(64)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	full := appendBlock(nil, &s.blocks[0])

	rd := NewReader(bytes.NewReader(full), DefaultLimits())
	if _, err := rd.ReadAtBlock(0); err != nil {
		t.Fatalf("valid block rejected: %v", err)
	}

	// Corrupt a payload byte -> raw CRC mismatch.
	bad := append([]byte(nil), full...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := NewReader(bytes.NewReader(bad), DefaultLimits()).ReadAtBlock(0); err == nil {
		t.Fatal("corrupt payload accepted")
	}

	// Corrupt the header CRC.
	bad = append([]byte(nil), full...)
	bad[53] ^= 0xFF
	if _, err := NewReader(bytes.NewReader(bad), DefaultLimits()).ReadAtBlock(0); err == nil {
		t.Fatal("corrupt header accepted")
	}

	// Truncated stored size.
	bad = full[:len(full)-10]
	if _, err := NewReader(bytes.NewReader(bad), DefaultLimits()).ReadAtBlock(0); err == nil {
		t.Fatal("truncated block accepted")
	}

	// None-compressed block with mismatched sizes.
	var s2 captureSink
	b2 := NewRowsBlockBuilder(1, 7, 4096, fileformat.CompressionNone, 0, DefaultLimits(), s2.flush)
	_ = b2.Add(1, 1, fileformat.ChangeInsert, mkRow(10))
	_ = b2.Flush()
	full2 := appendBlock(nil, &s2.blocks[0])
	// Patch StoredSize field to differ from RawSize.
	full2[44] = full2[44] + 1
	rd2 := NewReader(bytes.NewReader(full2), DefaultLimits())
	if _, err := rd2.ReadAtBlock(0); err == nil {
		t.Fatal("none block with stored != raw accepted")
	}
}

func TestCompressionBombRejected(t *testing.T) {
	// A payload that decodes far beyond the read limit must be rejected
	// without allocating its full size.
	huge := mkRow(200 << 20)
	comp, err := Compress(fileformat.CompressionZstd, 3, huge)
	if err != nil {
		t.Fatal(err)
	}
	lim := DefaultLimits()
	lim.MaxRawBytes = 64 << 20 // tighten to 64 MiB
	// The compressed frame itself is small; decoding into a 64 MiB cap must
	// fail without allocating 200 MiB.
	if _, err := Decompress(fileformat.CompressionZstd, nil, comp, lim.MaxRawBytes); err == nil {
		t.Fatal("compression bomb accepted")
	}
	// A normal small frame under the same limit must pass.
	small, _ := Compress(fileformat.CompressionZstd, 3, mkRow(1024))
	out, err := Decompress(fileformat.CompressionZstd, nil, small, lim.MaxRawBytes)
	if err != nil || len(out) != 1024 {
		t.Fatalf("small frame failed: %v len=%d", err, len(out))
	}
}

func TestMetadataBlockBuilder(t *testing.T) {
	var s captureSink
	b := NewMetadataBlockBuilder(1, 0, 4096, fileformat.CompressionZstd, 3, DefaultLimits(), s.flush)
	for i := 0; i < 10; i++ {
		rec := mkRow(300)
		if err := b.Add(metadataEntry(uint64(i)), rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) == 0 {
		t.Fatal("no metadata blocks")
	}
	for _, cb := range s.blocks {
		if cb.header.BlockKind != fileformat.BlockKindMetadata {
			t.Fatal("wrong block kind")
		}
		raw, err := Decompress(cb.header.Compression, nil, cb.payload, DefaultLimits().MaxRawBytes)
		if err != nil {
			t.Fatal(err)
		}
		if fileformat.CRC32C(raw) != cb.header.RawCRC32C {
			t.Fatal("metadata raw CRC mismatch")
		}
		p, err := parseMetaPayload(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Entries) != int(cb.header.ItemCount) {
			t.Fatalf("metadata entries %d != block items %d", len(p.Entries), cb.header.ItemCount)
		}
	}
}

func TestParseRowsPayloadRejects(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 4096, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	_ = b.Add(1, 1, fileformat.ChangeInsert, mkRow(20))
	_ = b.Add(2, 1, fileformat.ChangeDelete, nil)
	_ = b.Flush()
	raw := s.blocks[0].payload
	ic := s.blocks[0].header.ItemCount

	for n := 0; n < len(raw); n++ {
		if _, err := ParseRowsPayload(raw[:n], ic); err == nil {
			t.Fatalf("accepted truncated rows payload %d/%d", n, len(raw))
		}
	}
	// Trailing bytes.
	if _, err := ParseRowsPayload(append(raw, 1), ic); err == nil {
		t.Fatal("accepted trailing bytes")
	}
	// Item count mismatch.
	if _, err := ParseRowsPayload(raw, ic+1); err == nil {
		t.Fatal("accepted item count mismatch")
	}
	// Corrupt record header (RowID mismatch with directory).
	bad := append([]byte(nil), raw...)
	// First record starts at 32 + 2*24 = 80; flip its RowID byte.
	bad[80] ^= 0xFF
	if _, err := ParseRowsPayload(bad, ic); err == nil {
		t.Fatal("accepted record/directory RowID mismatch")
	}
	// DELETE carrying row bytes.
	bad = append([]byte(nil), raw...)
	// second record starts at 32+2*24+record1len(24+20=44) = 124; set its
	// RowLength to nonzero.
	rec2 := 32 + 2*24 + 44
	bad[rec2+16] = 5
	if _, err := ParseRowsPayload(bad, ic); err == nil {
		t.Fatal("accepted DELETE with row bytes")
	}
}

func TestRowLimitRejected(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxRawBytes = 1024
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 4096, fileformat.CompressionNone, 0, lim, s.flush)
	err := b.Add(1, 1, fileformat.ChangeInsert, mkRow(2000))
	if err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("oversize row accepted or wrong error: %v", err)
	}
}

func metadataEntry(oid uint64) metadata.DirectoryEntry {
	return metadata.DirectoryEntry{
		ObjectID:   oid,
		Revision:   1,
		RecordType: uint32(fileformat.RecordTable),
		Operation:  fileformat.OperationUpsert,
	}
}

func parseMetaPayload(raw []byte) (*metadata.Payload, error) {
	return metadata.Parse(raw)
}
