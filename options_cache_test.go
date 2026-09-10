package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSplitCacheBudget verifies the S1 hard-budget invariant: data + scan
// capacities always sum to exactly the total, for every explicit-override
// combination (FILE_FORMAT_REFACTOR_PLAN.md §8.1).
func TestSplitCacheBudget(t *testing.T) {
	const MiB = 1 << 20
	cases := []struct {
		total, scan int64
		wantData    int64
		wantScan    int64
	}{
		{0, 0, 0, 0},                             // total disabled
		{-1, 0, 0, 0},                            // total disabled (bench cold)
		{64 * MiB, 0, 32 * MiB, 32 * MiB},        // default split unchanged
		{256 * MiB, 0, 192 * MiB, 64 * MiB},      // 64 MiB scan cap kicks in
		{64 * MiB, 16 * MiB, 48 * MiB, 16 * MiB}, // explicit scan
		{64 * MiB, -1, 64 * MiB, 0},              // explicit scan disable
		{2 * MiB, 0, 1 * MiB, 1 * MiB},           // small total: no 1 MiB floor, sum preserved
	}
	for _, c := range cases {
		data, scan := cacheBudget(c.total, c.scan)
		if data != c.wantData || scan != c.wantScan {
			t.Errorf("cacheBudget(total=%d, scan=%d) = (%d, %d), want (%d, %d)",
				c.total, c.scan, data, scan, c.wantData, c.wantScan)
		}
		if c.total > 0 && data+scan != c.total {
			t.Errorf("total=%d: data+scan = %d, must equal total", c.total, data+scan)
		}
	}
}

func TestDisabledScanCacheAndGrowingPageCacheStayBounded(t *testing.T) {
	const budget = 128 << 10
	db := testDB(t, Options{
		BlockSize:      256 << 10,
		PageSize:       32 << 10,
		CacheBytes:     budget,
		ScanCacheBytes: -1,
	})
	w, err := db.Begin(context.Background(), NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 5000)
	snap, err := w.Commit(context.Background())
	require.NoError(t, err)

	// Exercise both the nil scan-cache path and enough distinct decoded pages
	// to force dynamic container growth and LRU eviction.
	it, err := db.Scan(context.Background(), snap, "users", ScanOptions{})
	require.NoError(t, err)
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
	}
	require.NoError(t, it.Err())
	require.NoError(t, it.Close())
	for id := RowID(1); id <= 5000; id += 250 {
		_, err := db.Get(context.Background(), snap, "users", id, nil)
		require.NoError(t, err)
	}
	st := db.Stats()
	require.LessOrEqual(t, st.Cache.UsedBytes, st.Cache.CapacityBytes)
	require.Equal(t, uint64(0), st.ScanCache.CapacityBytes)
}

// TestOptionsScanCacheValidation covers the explicit scan-budget validation.
func TestOptionsScanCacheValidation(t *testing.T) {
	ok := Options{CacheBytes: 64 << 20, ScanCacheBytes: 32 << 20}
	if _, err := ok.resolved(); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}
	bad := Options{CacheBytes: 64 << 20, ScanCacheBytes: 64 << 20}
	if _, err := bad.resolved(); err == nil {
		t.Fatal("scan budget consuming the whole cache must be rejected")
	}
	worse := Options{CacheBytes: 64 << 20, ScanCacheBytes: 128 << 20}
	if _, err := worse.resolved(); err == nil {
		t.Fatal("scan budget exceeding the cache must be rejected")
	}
	neg := Options{CacheBytes: 64 << 20, ScanCacheBytes: -1}
	if _, err := neg.resolved(); err != nil {
		t.Fatalf("scan-window disable must be accepted: %v", err)
	}
}
