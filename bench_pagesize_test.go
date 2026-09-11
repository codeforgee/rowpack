package rowpack

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkPageSizeSweep measures the page-geometry trade-off end to end.
// PageSize is the raw payload target of one Rows Page, so it directly sets how
// much a cold single-row read decompresses: with the cache disabled, cold Get
// costs one page decompression per row. Smaller pages cut that read
// amplification (and, down to ~8K, even the stored size) at the cost of more
// pages: slower hot reads and writes and a slightly slower full scan.
//
// This is an on-demand benchmark (not part of BENCH_PATTERN); run it with
// `go test -run '^$' -bench BenchmarkPageSizeSweep -benchmem .`.
func BenchmarkPageSizeSweep(b *testing.B) {
	for _, ps := range []int{4 << 10, 8 << 10, 16 << 10, 32 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("page=%dK", ps>>10), func(b *testing.B) {
			benchPageSize(b, ps)
		})
	}
}

func benchPageSize(b *testing.B, pageSize int) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "hot")
	db, snap := benchStoreAt(b, base, Options{PageSize: pageSize}, benchRows)
	st := db.Stats()
	b.Cleanup(func() { db.Close() })

	var dst Row
	b.Run("get_hot", func(b *testing.B) {
		b.ReportMetric(float64(st.StoredBytes)/float64(benchRows), "storedB/row")
		b.ReportMetric(float64(st.DataFileBytes)/(1<<20), "fileMB")
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			row, err := db.Get(ctx, snap, "t", RowID(i%benchRows)+1, dst)
			if err != nil {
				b.Fatal(err)
			}
			dst = row[:0]
		}
	})

	b.Run("scan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			it, err := db.Scan(ctx, snap, "t", ScanOptions{})
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for {
				if _, ok := it.Next(); !ok {
					break
				}
				n++
			}
			if err := it.Err(); err != nil {
				b.Fatal(err)
			}
			if err := it.Close(); err != nil {
				b.Fatal(err)
			}
			if n != benchRows {
				b.Fatalf("scan returned %d rows, want %d", n, benchRows)
			}
		}
	})

	// Cold reads: cache disabled, so every Get decompresses its page.
	cbase := filepath.Join(tmpdb(b), "cold")
	cdb, csnap := benchStoreAt(b, cbase, Options{PageSize: pageSize, CacheBytes: -1}, benchRows)
	b.Cleanup(func() { cdb.Close() })
	b.Run("get_cold", func(b *testing.B) {
		before := cdb.Stats().Read
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			row, err := cdb.Get(ctx, csnap, "t", RowID((i*7919)%benchRows)+1, dst)
			if err != nil {
				b.Fatal(err)
			}
			dst = row[:0]
		}
		b.StopTimer()
		after := cdb.Stats().Read
		b.ReportMetric(float64(after.DecompressedBytes-before.DecompressedBytes)/float64(b.N), "rawB/op")
		b.ReportMetric(float64(after.ReadBytes-before.ReadBytes)/float64(b.N), "readB/op")
	})

	b.Run("write", func(b *testing.B) {
		const n = 25_000
		for i := 0; i < b.N; i++ {
			d, _ := benchStoreAt(b, filepath.Join(tmpdb(b), fmt.Sprintf("w%d", i)), Options{PageSize: pageSize}, n)
			d.Close()
		}
		b.StopTimer()
		b.ReportMetric(float64(n)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	})
}
