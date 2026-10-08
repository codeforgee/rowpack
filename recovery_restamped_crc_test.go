package rowpack

// Round-3 recovery probes: arms the existing crafted tests cannot reach —
// in particular a txn whose footer-bound CRC was restamped to match corrupted
// body bytes. The CRC gate in readIndexTxn passes, so the only defense left
// is the streaming apply's own validation: recovery must fall back to the
// rebuild path and serve a healthy store, never the garbage.

import (
	"path/filepath"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// TestRestampedTxnCRCWithGarbageBody: a committed footer whose IndexTxnCRC32C
// matches corrupted body bytes (a writer bug or adversarial edit, not bit
// rot — plain rot fails the CRC gate). The txn must not be applied: recovery
// falls back to the block rebuild and the reopened store reads exactly the
// committed rows.
func TestRestampedTxnCRCWithGarbageBody(t *testing.T) {
	base := filepath.Join(t.TempDir(), "restamp")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	snap2 := buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	last := committed[len(committed)-1]
	require.Equal(t, uint64(snap2), last.snapshotID)
	require.NoError(t, db.Close())

	// Corrupt one body byte well past the txn header, then restamp the
	// footer's IndexTxnCRC32C so the footer-bound check passes.
	span := readSpan(t, base, last.txnStart, last.txnEnd)
	require.Greater(t, len(span), format.IndexTxnHeaderSize+16)
	span[format.IndexTxnHeaderSize+16] ^= 0xFF
	restampSpanCRC(t, base, span, last.footerOff)
	writeFileSpan(t, base, last.txnStart, span)

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err, "a restamped-CRC txn must recover via rebuild, not fail the open")
	defer func() { require.NoError(t, db2.Close()) }()

	rec := db2.Stats().Recovery
	require.True(t, rec.Performed, "the corrupted txn must trigger a rebuild")
	require.Equal(t, uint64(1), rec.SnapshotsRebuilt)

	// Both snapshots' rows survive, byte for byte.
	ctx := t.Context()
	for _, tc := range []struct {
		snap SnapshotID
		id   uint64
	}{
		{1, 1}, {snap2, 2},
	} {
		row, err := db2.Get(ctx, tc.snap, "users", tc.id, nil)
		require.NoError(t, err, "snapshot %d row %d", tc.snap, tc.id)
		v, ok := row[0].Uint64()
		require.True(t, ok)
		require.Equal(t, tc.id, v)
	}

	// Verify must accept the rebuilt store.
	_, err = db2.Verify(ctx, VerifyFull, VerifyScope{})
	require.NoError(t, err)
}
