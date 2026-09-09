package rowpack

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkDeepChainGetLazy measures the S4 decision #4 risk: a Lazy index
// Get at the tip of an N-layer DELTA chain must resolve along the parent chain,
// doing one fence binary-search + on-demand index-page decode per layer before
// the final data block read. Eager resolves the whole chain in memory. Two
// dimensions are reported for each depth:
//
//	warm — the IndexPageCache is enabled and pre-warmed, showing the cached
//	     hot path (page read once per layer, then served from cache);
//	cold — IndexPageCache is disabled (IndexCacheBytes=-1), showing the worst
//	     case: every chain layer re-reads + re-decodes the candidate page,
//	     which is the amplification a negative-query-cache or page bloom filter
//	     would target.
//
// If the cold amplification is unacceptable the ADR decides between the two
// prototypes.
func BenchmarkDeepChainGetLazy(b *testing.B) {
	for _, cold := range []bool{false, true} {
		for _, depth := range []int{1, 8, 32, 128} {
			mode := "warm"
			if cold {
				mode = "cold"
			}
			b.Run(fmt.Sprintf("%s/depth=%d", mode, depth), func(b *testing.B) {
				ctx := context.Background()
				const rows = 1000
				const deltaRows = 1
				base := filepath.Join(tmpdb(b), fmt.Sprintf("dcl-%s-%d", mode, depth))
				db, head := buildDeltaChainStore(b, base, depth, deltaRows, rows, 0)
				if err := db.Close(); err != nil {
					b.Fatal(err)
				}
				indexBytes := int64(8 << 20)
				if cold {
					indexBytes = -1
				}
				db, err := Open(base, Options{IndexMode: IndexLazy, IndexCacheBytes: indexBytes})
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				// Warm the data-block cache so the reported amplification is the
				// index-chain walk, not the block read. In cold index mode the
				// index pages stay uncached; the data blocks are cached.
				for i := uint64(1); i <= uint64(rows); i++ {
					_, err := db.Get(ctx, head, "t", i, nil)
					if err != nil {
						b.Fatal(err)
					}
				}
				var rng uint64 = 1442695040888963407
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rng = rng*6364136223846793005 + 1
					_, err := db.Get(ctx, head, "t", rng%uint64(rows)+1, nil)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				st := db.Stats()
				b.ReportMetric(float64(st.Read.ReadBytes)/float64(b.N), "readB/op")
				b.ReportMetric(float64(st.Read.DecompressedBytes)/float64(b.N), "rawB/op")
				h, m := st.IndexPageCache.Hits, st.IndexPageCache.Misses
				if h+m > 0 {
					b.ReportMetric(100*float64(h)/float64(h+m), "idxpage-hitpct")
				}
				b.ReportMetric(float64(st.IndexPageCache.Loads)/float64(b.N), "idxpage-loads/op")
			})
		}
	}
}
