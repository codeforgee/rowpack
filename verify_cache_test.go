package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestVerifyLeavesCacheUntouched: Verify is a one-shot full-store walk — every
// block is visited exactly once and never revisited, so installing the walked
// containers in the random-read cache would only evict the Get hot set (a
// post-Verify Get would pay cold-read latency for a store it had already
// warmed). Both modes must leave the cache byte-for-byte unchanged.
func TestVerifyLeavesCacheUntouched(t *testing.T) {
	ctx := context.Background()
	base, snap := setupPlainMultiPageStore(t, 600)
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	// Warm the random-read cache with one Get per block's first row.
	bl, err := db.Blocks(snap, "t")
	require.NoError(t, err)
	require.NotEmpty(t, bl)
	for _, b := range bl {
		_, err := db.Get(ctx, snap, "t", b.MinRowID, nil)
		require.NoError(t, err)
	}
	used := db.Stats().Cache.UsedBytes
	require.Positive(t, used, "warmed Gets must populate the cache")

	for _, mode := range []VerifyMode{VerifyQuick, VerifyFull} {
		rep, err := db.Verify(ctx, mode, VerifyScope{})
		require.NoError(t, err)
		require.Positive(t, rep.BlocksChecked)
		require.Equal(t, used, db.Stats().Cache.UsedBytes,
			"Verify (%d) must not install anything in the random-read cache", mode)
	}
}
