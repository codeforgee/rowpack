package rowpack

import (
	"testing"
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
		data, scan := splitCacheBudget(c.total, c.scan)
		if data != c.wantData || scan != c.wantScan {
			t.Errorf("splitCacheBudget(total=%d, scan=%d) = (%d, %d), want (%d, %d)",
				c.total, c.scan, data, scan, c.wantData, c.wantScan)
		}
		if c.total > 0 && data+scan != c.total {
			t.Errorf("total=%d: data+scan = %d, must equal total", c.total, data+scan)
		}
	}
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
