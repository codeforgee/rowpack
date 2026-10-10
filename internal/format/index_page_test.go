package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
	require.Equal(t, h, got, "round trip = %+v, want %+v", got, h)
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
	err := (&RowIndexPageHeader{}).Unmarshal(b[:], total)
	require.NoError(t, err, "geometry mismatch rejected")
	// A mismatched totalLen must be rejected.
	require.Error(t, (&RowIndexPageHeader{}).Unmarshal(b[:], total+1), "wrong geometry accepted")
	// The change stream is indexed directly by entry ordinal, so its exact
	// geometry is a safety invariant, not merely a compression detail.
	b[0] = 'R'
	putU32(b[32:], 0)
	require.Error(t, (&RowIndexPageHeader{}).Unmarshal(b[:], 0), "undersized change-bit stream accepted")
	// Bad magic must be rejected.
	b[0] = 'X'
	require.Error(t, (&RowIndexPageHeader{}).Unmarshal(b[:], total), "bad magic accepted")
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
	// SnapshotID and StoredOffset are not encoded: the parser injects the
	// former from the IndexTxn header and recomputes the latter from the
	// preceding sizes, so a fence that has been to disk and back carries
	// neither.
	want := e
	want.SnapshotID = 0
	want.StoredOffset = 0
	require.Equal(t, want, got, "round trip = %+v, want %+v", got, want)
	// Zero EntryCount is rejected.
	e2 := e
	e2.EntryCount = 0
	var b2 [IndexFenceEntrySize]byte
	_ = e2.MarshalTo(b2[:])
	require.Error(t, (&RowIndexFenceEntry{}).Unmarshal(b2[:]), "zero-entry fence accepted")
}
