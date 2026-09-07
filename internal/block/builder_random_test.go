package block

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// captureBuilder builds rows into a capture sink and returns the last flushed
// raw payload.
func captureBuilder(t *testing.T, compress fileformat.Compression, add func(b *RowsBlockBuilder) error) ([]capturedBlock, *RowsBlockBuilder) {
	t.Helper()
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, compress, 0, DefaultLimits(), s.flush)
	if err := add(b); err != nil {
		t.Fatal(err)
	}
	return s.blocks, b
}

func TestRowsBuilderDeletePendingRawPayload(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	if b.Pending() != 0 {
		t.Fatalf("initial Pending = %d", b.Pending())
	}
	if err := b.Add(10, 1, fileformat.ChangeInsert, []byte("row-ten")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(11, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(12, 1, fileformat.ChangeInsert, []byte("row-twelve")); err != nil {
		t.Fatal(err)
	}
	if b.Pending() != 3 {
		t.Fatalf("Pending = %d, want 3", b.Pending())
	}
	// RawPayload builds without flushing; Pending must be unchanged and the
	// next Flush must produce identical bytes.
	raw1 := append([]byte(nil), b.RawPayload()...)
	if b.Pending() != 3 {
		t.Fatal("RawPayload flushed pending records")
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	if b.Pending() != 0 {
		t.Fatalf("Pending after Flush = %d", b.Pending())
	}
	if len(s.blocks) != 1 {
		t.Fatalf("flushed %d blocks", len(s.blocks))
	}
	if !bytes.Equal(raw1, s.blocks[0].payload) {
		t.Fatal("RawPayload differs from flushed payload")
	}
	// Flush with nothing pending is a no-op.
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) != 1 {
		t.Fatal("empty Flush emitted a block")
	}

	// Payload semantics: tombstones carry no row bytes and zero CRC.
	p, err := ParseRowsPayload(s.blocks[0].payload, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.RowBytes(1); got != nil {
		t.Fatalf("DELETE RowBytes = %q", got)
	}
	if got := p.RowBytes(0); string(got) != "row-ten" {
		t.Fatalf("RowBytes(0) = %q", got)
	}
	if crc := p.RowCRC(0); crc != fileformat.CRC32C([]byte("row-ten")) {
		t.Fatalf("RowCRC(0) = %d", crc)
	}
	if crc := p.RowCRC(1); crc != 0 {
		t.Fatalf("DELETE RowCRC = %d", crc)
	}
	// Entry fields agree with the directory.
	if e := p.Entries[1]; e.RowID != 11 || e.ChangeType != fileformat.ChangeDelete {
		t.Fatalf("DELETE entry = %+v", e)
	}
}

func TestOversizedRowOwnBlock(t *testing.T) {
	// A row whose record does not fit the block size must land alone in its
	// own block even when pending records exist.
	var s captureSink
	// records buffer holds only record bytes (24-byte header + body), so a
	// blockSize of 80 fits one 8-byte row but forces a 64-byte row into the
	// oversized path.
	b := NewRowsBlockBuilder(1, 7, 80, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	if err := b.Add(1, 1, fileformat.ChangeInsert, make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := b.Add(2, 1, fileformat.ChangeInsert, make([]byte, 64)); err != nil { // oversized
		t.Fatal(err)
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(s.blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (one small, one oversized)", len(s.blocks))
	}
	if s.blocks[1].header.ItemCount != 1 {
		t.Fatalf("oversized block ItemCount = %d", s.blocks[1].header.ItemCount)
	}
}

func TestRowsBuilderLimitErrors(t *testing.T) {
	limits := DefaultLimits()
	// Row exceeds MaxRawBytes.
	var s1 captureSink
	b1 := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, limits, s1.flush)
	if err := b1.Add(1, 1, fileformat.ChangeInsert, make([]byte, limits.MaxRawBytes+1)); err == nil {
		t.Fatal("row over MaxRawBytes accepted")
	}
	// Block payload exceeds MaxRawBytes (offset+length check in append): the
	// row itself fits the limit but the accumulated records do not.
	var s2 captureSink
	small := Limits{MaxRawBytes: 200, MaxStoredBytes: 1 << 20}
	b2 := NewRowsBlockBuilder(1, 7, 1<<20, fileformat.CompressionNone, 0, small, s2.flush)
	if err := b2.Add(1, 1, fileformat.ChangeInsert, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if err := b2.Add(2, 1, fileformat.ChangeInsert, make([]byte, 100)); err == nil {
		t.Fatal("payload over MaxRawBytes accepted")
	}
}

func TestParseRowsDirectoryAndIndexRowBytes(t *testing.T) {
	raw := func() []byte {
		var s captureSink
		b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
		_ = b.Add(5, 1, fileformat.ChangeInsert, []byte("five"))
		_ = b.Delete(6, 1)
		_ = b.Add(7, 1, fileformat.ChangeInsert, []byte("seven"))
		if err := b.Flush(); err != nil {
			t.Fatal(err)
		}
		return s.blocks[0].payload
	}()

	idx, err := ParseRowsDirectory(raw, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 3 || idx.Header.ItemCount != 3 {
		t.Fatalf("index = %+v", idx.Header)
	}
	if got := idx.RowBytes(0); string(got) != "five" {
		t.Fatalf("RowBytes(0) = %q", got)
	}
	if got := idx.RowBytes(1); got != nil {
		t.Fatalf("DELETE RowBytes = %q", got)
	}
	if got := idx.RowBytes(2); string(got) != "seven" {
		t.Fatalf("RowBytes(2) = %q", got)
	}

	// Rejections.
	if _, err := ParseRowsDirectory(raw, 4, nil); err == nil {
		t.Fatal("item count mismatch accepted")
	}
	if _, err := ParseRowsDirectory(raw[:fileformat.RowsPayloadHeaderSize+2*fileformat.RowDirectoryEntrySize], 3, nil); err == nil {
		t.Fatal("truncated directory accepted")
	}
	// Records region mismatch: header claims more record bytes than present
	// (RecordsBytes is a uint64 at payload offset 24).
	bad := append([]byte(nil), raw...)
	bad[31] = 0xFF
	if _, err := ParseRowsDirectory(bad, 3, nil); err == nil {
		t.Fatal("records region mismatch accepted")
	}
	// Out-of-bounds record offset in the directory (entry[1].RecordOffset is
	// at payload header 32 + 1*24 + 8).
	bad = append([]byte(nil), raw...)
	bad[32+24+8+3] = 0x7F
	if _, err := ParseRowsDirectory(bad, 3, nil); err == nil {
		t.Fatal("out-of-bounds record accepted")
	}
}

func TestParseRowAt(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	_ = b.Add(5, 1, fileformat.ChangeInsert, []byte("five"))
	_ = b.Delete(6, 1)
	_ = b.Add(7, 1, fileformat.ChangeInsert, []byte("seven"))
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	raw := s.blocks[0].payload

	ref, err := ParseRowAt(raw, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Entry.RowID != 7 || string(ref.Row) != "seven" {
		t.Fatalf("ParseRowAt(2) = %+v %q", ref.Entry, ref.Row)
	}
	if ref.Header.RowCRC32C != fileformat.CRC32C([]byte("seven")) {
		t.Fatalf("record CRC = %d", ref.Header.RowCRC32C)
	}
	// DELETE: row bytes nil, tombstone invariants hold.
	ref, err = ParseRowAt(raw, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Row != nil || ref.Header.RowEncoding != fileformat.RowEncodingNone {
		t.Fatalf("DELETE ref = %+v", ref)
	}

	// Rejections.
	if _, err := ParseRowAt(raw, 4, 0); err == nil {
		t.Fatal("item count mismatch accepted")
	}
	if _, err := ParseRowAt(raw, 3, 3); err == nil {
		t.Fatal("ordinal out of range accepted")
	}
	// Corrupted directory entry: record header no longer agrees (entry[0]
	// starts at payload offset 32; RowID low byte is entry offset 0).
	bad := append([]byte(nil), raw...)
	bad[32] ^= 0xFF
	if _, err := ParseRowAt(bad, 3, 0); err == nil {
		t.Fatal("header/directory mismatch accepted")
	}
	// Records region mismatch (RecordsBytes at payload offset 24).
	bad = append([]byte(nil), raw...)
	bad[31] = 0xFF
	if _, err := ParseRowAt(bad, 3, 0); err == nil {
		t.Fatal("records region mismatch accepted")
	}
}

func TestParseRowsPayloadRejections(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	_ = b.Add(5, 1, fileformat.ChangeInsert, []byte("five"))
	_ = b.Delete(6, 1)
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	raw := s.blocks[0].payload

	if _, err := ParseRowsPayload(raw, 5); err == nil {
		t.Fatal("item count mismatch accepted")
	}
	// Directory exceeds payload.
	if _, err := ParseRowsPayload(raw[:fileformat.RowsPayloadHeaderSize], 2); err == nil {
		t.Fatal("truncated payload accepted")
	}
	// Records bytes overhang (RecordsBytes is a uint64 at payload offset 24).
	bad := append([]byte(nil), raw...)
	bad[31] = 0xFF
	if _, err := ParseRowsPayload(bad, 2); err == nil {
		t.Fatal("records overhang accepted")
	}
	// Directory entry claims a record beyond the records region
	// (entry[0].RecordOffset at payload offset 32+8).
	bad = append([]byte(nil), raw...)
	bad[32+8+3] = 0x7F
	if _, err := ParseRowsPayload(bad, 2); err == nil {
		t.Fatal("out-of-bounds record accepted")
	}
	// Record claiming DELETE while carrying row bytes: both the directory
	// entry (ChangeType at entry offset 16) and the record header (record
	// byte 12) must agree on DELETE while RowLength stays nonzero.
	bad = append([]byte(nil), raw...)
	recOff := fileformat.RowsPayloadHeaderSize + 2*fileformat.RowDirectoryEntrySize
	bad[32+16] = byte(fileformat.ChangeDelete)
	bad[recOff+12] = byte(fileformat.ChangeDelete)
	if _, err := ParseRowsPayload(bad, 2); err == nil {
		t.Fatal("DELETE record with row bytes accepted")
	}
}

// TestParseRowsDirectoryReuse verifies the directory entry slice argument is
// reused across blocks: parsing a second payload with the same backing slice
// overwrites entries correctly and validates each payload independently.
func TestParseRowsDirectoryReuse(t *testing.T) {
	mkRaw := func(ids []uint64, _ []string) []byte {
		var s captureSink
		b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
		for i, id := range ids {
			_ = b.Add(id, 1, fileformat.ChangeInsert, []byte(fmt.Sprintf("row-%d", id)))
			_ = i
		}
		if err := b.Flush(); err != nil {
			t.Fatal(err)
		}
		return s.blocks[0].payload
	}
	rawA := mkRaw([]uint64{1, 2}, nil)
	rawB := mkRaw([]uint64{100, 200, 300}, nil)

	var reuse []fileformat.RowDirectoryEntry
	idxA, err := ParseRowsDirectory(rawA, 2, reuse)
	if err != nil {
		t.Fatal(err)
	}
	reuse = idxA.Entries
	if len(idxA.Entries) != 2 {
		t.Fatalf("A: entries = %d", len(idxA.Entries))
	}
	// Reuse the same backing slice for a payload with MORE entries.
	idxB, err := ParseRowsDirectory(rawB, 3, reuse)
	if err != nil {
		t.Fatal(err)
	}
	if len(idxB.Entries) != 3 {
		t.Fatalf("B: entries = %d", len(idxB.Entries))
	}
	if got := idxB.RowBytes(2); string(got) != "row-300" {
		t.Fatalf("B RowBytes(2) = %q", got)
	}
	// Backing slice grew past the original capacity: A's alias is stale but
	// the next parse with the grown slice must still work.
	idxA2, err := ParseRowsDirectory(rawA, 2, idxB.Entries)
	if err != nil {
		t.Fatal(err)
	}
	if got := idxA2.RowBytes(1); string(got) != "row-2" {
		t.Fatalf("A2 RowBytes(1) = %q", got)
	}
}
