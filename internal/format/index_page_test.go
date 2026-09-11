package format

import "testing"

func TestRowIndexPageHeaderRoundTrip(t *testing.T) {
	h := RowIndexPageHeader{
		EntryCount: 4096, TableRunBytes: 10, RowIDBytes: 20, BlockRunBytes: 15,
		OrdinalBytes: 12, ChangeBitsBytes: 1024, FirstRowID: 7, MinRowID: 3,
		MaxRowID: 4000, CRC32C: 0xC0FFEE,
	}
	var b [IndexPageHeaderSize]byte
	if err := h.MarshalTo(b[:]); err != nil {
		t.Fatal(err)
	}
	var got RowIndexPageHeader
	// totalLen 0 skips the streams-sum cross-check (this round-trips only the
	// header; the streams region is validated by the geometry test below).
	if err := got.Unmarshal(b[:], 0); err != nil {
		t.Fatal(err)
	}
	if got != h {
		t.Fatalf("round trip = %+v, want %+v", got, h)
	}
}

func TestRowIndexPageHeaderGeometry(t *testing.T) {
	// Streams sum must equal totalLen when provided.
	h := RowIndexPageHeader{
		EntryCount: 2, TableRunBytes: 2, RowIDBytes: 2, BlockRunBytes: 2,
		OrdinalBytes: 2, ChangeBitsBytes: 1,
	}
	var b [IndexPageHeaderSize]byte
	if err := h.MarshalTo(b[:]); err != nil {
		t.Fatal(err)
	}
	total := IndexPageHeaderSize + int(h.StreamsBytes())
	if err := (&RowIndexPageHeader{}).Unmarshal(b[:], total); err != nil {
		t.Fatalf("geometry mismatch rejected: %v", err)
	}
	// A mismatched totalLen must be rejected.
	if err := (&RowIndexPageHeader{}).Unmarshal(b[:], total+1); err == nil {
		t.Fatal("wrong geometry accepted")
	}
	// The change stream is indexed directly by entry ordinal, so its exact
	// geometry is a safety invariant, not merely a compression detail.
	b[0] = 'R'
	putU32(b[32:], 0)
	if err := (&RowIndexPageHeader{}).Unmarshal(b[:], 0); err == nil {
		t.Fatal("undersized change-bit stream accepted")
	}
	// Bad magic must be rejected.
	b[0] = 'X'
	if err := (&RowIndexPageHeader{}).Unmarshal(b[:], total); err == nil {
		t.Fatal("bad magic accepted")
	}
}

func TestRowIndexFenceRoundTrip(t *testing.T) {
	e := RowIndexFenceEntry{
		SnapshotID: 9, TableID: 3, MinRowID: 100, MaxRowID: 200,
		StoredOffset: 12345, StoredSize: 555, RawSize: 16000, EntryCount: 4096,
		PageCRC32C: 0xDEADBEEF,
	}
	var b [IndexFenceEntrySize]byte
	if err := e.MarshalTo(b[:]); err != nil {
		t.Fatal(err)
	}
	var got RowIndexFenceEntry
	if err := got.Unmarshal(b[:]); err != nil {
		t.Fatal(err)
	}
	if got != e {
		t.Fatalf("round trip = %+v, want %+v", got, e)
	}
	// Zero EntryCount is rejected.
	e2 := e
	e2.EntryCount = 0
	var b2 [IndexFenceEntrySize]byte
	_ = e2.MarshalTo(b2[:])
	if err := (&RowIndexFenceEntry{}).Unmarshal(b2[:]); err == nil {
		t.Fatal("zero-entry fence accepted")
	}
}
