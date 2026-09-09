package fileformat

import "testing"

// TestIndexTxnHeaderRowIndexPageCount round-trips the RowIndexPageCount word
// (offset 12..16) and locks that it is independent of the KeyEpoch word
// (offset 76..80). A plain store records the page count there; the same word
// must never collide with the encrypted-store KeyEpoch patch.
func TestIndexTxnHeaderRowIndexPageCount(t *testing.T) {
	h := IndexTxnHeader{
		TxnSequence: 12, SnapshotID: 7, DataSnapshotStart: 128, DataSnapshotEnd: 20000,
		MetadataEntryCount: 3, BlockEntryCount: 10, RowEntryCount: 5000, BodyBytes: 640,
		RowIndexPageCount: 7,
	}
	var b [IndexTxnHeaderSize]byte
	if err := h.MarshalTo(b[:]); err != nil {
		t.Fatal(err)
	}
	if v := binaryUint32(b[12:]); v != 7 {
		t.Fatalf("index page count word = %d, want 7", v)
	}
	// The KeyEpoch word (76..80) must still be zero for a plain header.
	if v := binaryUint32(b[IndexTxnKeyEpochOffset:]); v != 0 {
		t.Fatalf("key epoch word = %d, want 0 for plain header", v)
	}
	var got IndexTxnHeader
	if err := got.Unmarshal(b[:]); err != nil {
		t.Fatal(err)
	}
	if got.RowIndexPageCount != h.RowIndexPageCount {
		t.Fatalf("round trip count = %d, want %d", got.RowIndexPageCount, h.RowIndexPageCount)
	}

	// PatchIndexTxnHeaderForStorage (encrypted store) must preserve the page
	// count while stamping KeyEpoch.
	if err := PatchIndexTxnHeaderForStorage(b[:], 1000, 5); err != nil {
		t.Fatal(err)
	}
	if v := binaryUint32(b[12:]); v != 7 {
		t.Fatalf("index page count word after patch = %d, want 7", v)
	}
	if got := IndexTxnHeaderKeyEpoch(b[:]); got != 5 {
		t.Fatalf("key epoch after patch = %d, want 5", got)
	}
	if err := got.Unmarshal(b[:]); err != nil {
		t.Fatalf("header with patched key epoch/body does not unmarshal: %v", err)
	}
	if got.RowIndexPageCount != 7 {
		t.Fatalf("round trip count after patch = %d, want 7", got.RowIndexPageCount)
	}
	if got.BodyBytes != 1000 {
		t.Fatalf("body bytes after patch = %d, want 1000", got.BodyBytes)
	}
}
