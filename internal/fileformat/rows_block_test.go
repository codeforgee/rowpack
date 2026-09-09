package fileformat

import (
	"testing"
)

func TestRowsBlockHeaderRoundTrip(t *testing.T) {
	h := RowsBlockHeader{PageCount: 4, DirectoryBytes: 4 * RowsPageDirEntrySize, TotalRecords: 1000}
	var b [RowsBlockHeaderSize]byte
	if err := h.MarshalTo(b[:]); err != nil {
		t.Fatal(err)
	}
	var got RowsBlockHeader
	if err := got.Unmarshal(b[:]); err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("round trip = %+v, want %+v", got, h)
	}
}

func TestRowsBlockHeaderRejectsBadGeometry(t *testing.T) {
	// DirectoryBytes must equal PageCount * RowsPageDirEntrySize.
	h := RowsBlockHeader{PageCount: 4, DirectoryBytes: 100, TotalRecords: 1}
	var b [RowsBlockHeaderSize]byte
	if err := h.MarshalTo(b[:]); err == nil {
		t.Fatal("inconsistent directory bytes accepted by MarshalTo")
	}
	// A header with a mismatched magic must be rejected.
	h2 := RowsBlockHeader{PageCount: 4, DirectoryBytes: 4 * RowsPageDirEntrySize, TotalRecords: 1}
	var b2 [RowsBlockHeaderSize]byte
	_ = h2.MarshalTo(b2[:])
	b2[0] = 'X'
	var got RowsBlockHeader
	if err := got.Unmarshal(b2[:]); err == nil {
		t.Fatal("bad magic accepted")
	}
	// Multiplication must be checked in uint64: this count wraps to zero when
	// multiplied by the 56-byte entry size in uint32 arithmetic.
	putU32(b2[12:], 1<<29)
	putU32(b2[16:], 0)
	b2[0] = 'R'
	if err := got.Unmarshal(b2[:]); err == nil {
		t.Fatal("overflowing page count accepted")
	}
}

func TestRowsPageDirEntryRoundTrip(t *testing.T) {
	e := RowsPageDirEntry{
		PageOrdinal: 3, FirstRecordOrdinal: 10, RecordCount: 200,
		StoredOffset: 4096, StoredSize: 8192, RawSize: 32768,
		MinRowID: 5, MaxRowID: 205, PageCRC32C: 0xdeadbeef, Flags: 1,
	}
	var b [RowsPageDirEntrySize]byte
	if err := e.MarshalTo(b[:]); err != nil {
		t.Fatal(err)
	}
	var got RowsPageDirEntry
	if err := got.Unmarshal(b[:]); err != nil {
		t.Fatal(err)
	}
	if got != e {
		t.Fatalf("round trip = %+v, want %+v", got, e)
	}
}
