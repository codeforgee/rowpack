package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

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
	require.EqualValues(t, 7, binaryUint32(b[12:]), "index page count word = %d, want 7", binaryUint32(b[12:]))
	// The KeyEpoch word (76..80) must still be zero for a plain header.
	require.EqualValues(t, 0, binaryUint32(b[IndexTxnKeyEpochOffset:]), "key epoch word = %d, want 0 for plain header", binaryUint32(b[IndexTxnKeyEpochOffset:]))
	var got IndexTxnHeader
	if err := got.Unmarshal(b[:]); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, h.RowIndexPageCount, got.RowIndexPageCount, "round trip count = %d, want %d", got.RowIndexPageCount, h.RowIndexPageCount)

	// PatchIndexTxnHeaderForStorage (encrypted store) must preserve the page
	// count while stamping KeyEpoch.
	if err := PatchIndexTxnHeaderForStorage(b[:], 1000, 5); err != nil {
		t.Fatal(err)
	}
	require.EqualValues(t, 7, binaryUint32(b[12:]), "index page count word after patch = %d, want 7", binaryUint32(b[12:]))
	require.Equal(t, uint32(5), IndexTxnHeaderKeyEpoch(b[:]), "key epoch after patch = %d, want 5", IndexTxnHeaderKeyEpoch(b[:]))
	err := got.Unmarshal(b[:])
	require.NoError(t, err, "header with patched key epoch/body does not unmarshal")
	require.EqualValues(t, 7, got.RowIndexPageCount, "round trip count after patch = %d, want 7", got.RowIndexPageCount)
	require.EqualValues(t, 1000, got.BodyBytes, "body bytes after patch = %d, want 1000", got.BodyBytes)
}
