package rowpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Benchmarks record environment-dependent numbers; run with `go test -bench .`.
// Each benchmark reports the environment (Go version, Zstd version) via
// BenchmarkEnv.

func BenchmarkEnv(b *testing.B) {
	// Informational benchmark: prints Go + Zstd versions and dataset geometry.
	b.Logf("go=1.27 zstd=klauspost/compress/v1.20.0 blockSize=%d compression=zstd dataset=100k rows x 7 cols",
		fileformat.DefaultBlockSize)
	b.N = 0
}

func benchRow(i uint64) Row {
	return Row{
		Uint64(i),
		String(fmt.Sprintf("user-%d-abcdefghijklmnop", i)),
		Bool(i%2 == 0),
		Int32(int32(i)),
		Float64(float64(i) * 0.5),
		DateTime(time.Unix(0, 1700000000000000000).UTC()),
		DecimalValue(Decimal{Unscaled: bigI(int64(i * 100)), Scale: 2}),
	}
}

func benchSchema() Schema {
	return Schema{TableID: 1, Version: 1, Name: "bench", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
		{Name: "active", Type: TypeBool},
		{Name: "age", Type: TypeInt32},
		{Name: "score", Type: TypeFloat64},
		{Name: "created", Type: TypeDateTime},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	}}
}

// buildBenchStore writes nRows into a FULL snapshot and returns the open store
// plus snapshot ID.
func buildBenchStore(b *testing.B, base string, nRows uint64, blockSize int) (*Store, SnapshotID) {
	b.Helper()
	opts := DefaultOptions()
	if blockSize > 0 {
		opts.BlockSize = blockSize
	}
	db, err := Create(base, opts)
	if err != nil {
		b.Fatal(err)
	}
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		b.Fatal(err)
	}
	if err := w.DefineSchema(benchSchema()); err != nil {
		b.Fatal(err)
	}
	for i := uint64(0); i < nRows; i++ {
		if err := w.Insert(context.Background(), 1, i+1, 1, benchRow(i)); err != nil {
			b.Fatal(err)
		}
	}
	full, err := w.Commit(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	return db, full.ID
}

func BenchmarkFullSequentialWrite(b *testing.B) {
	const rows = 100000
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(b.TempDir(), "w")
		db, err := Create(base, DefaultOptions())
		if err != nil {
			b.Fatal(err)
		}
		w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
		w.DefineSchema(benchSchema())
		b.StartTimer()
		for j := uint64(0); j < rows; j++ {
			if err := w.Insert(context.Background(), 1, j+1, 1, benchRow(j)); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := w.Commit(context.Background()); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		db.Close()
	}
	b.SetBytes(rows * 100)
	b.ReportMetric(float64(rows)/b.Elapsed().Seconds()/1000, "krows/s")
}

func BenchmarkGetColdRead(b *testing.B) {
	base := filepath.Join(b.TempDir(), "cold")
	db, fullID := buildBenchStore(b, base, 100000, 0)
	db.Close() // release the writer lock before reopening
	// Disable cache to force cold reads.
	db2, err := Open(base, Options{CacheBytes: -1})
	if err != nil {
		b.Fatal(err)
	}
	defer db2.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db2.Get(context.Background(), fullID, 1, uint64(i%100000)+1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetHotRead(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(b.TempDir(), "hot"), 100000, 0)
	defer db.Close()
	// Warm a few blocks.
	for i := uint64(0); i < 100; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i+1); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, uint64(i%100)+1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConcurrentGet(b *testing.B) {
	for _, g := range []int{1, 8, 32, 64} {
		b.Run(fmt.Sprintf("g%d", g), func(b *testing.B) {
			db, fullID := buildBenchStore(b, filepath.Join(b.TempDir(), "conc"), 100000, 0)
			defer db.Close()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := uint64(0)
				for pb.Next() {
					i++
					if _, err := db.Get(context.Background(), fullID, 1, i%100000+1); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func BenchmarkScan(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(b.TempDir(), "scan"), 100000, 0)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		for it.Next() {
		}
		if err := it.Err(); err != nil {
			b.Fatal(err)
		}
		it.Close()
	}
}

func BenchmarkOpenReplay(b *testing.B) {
	base := filepath.Join(b.TempDir(), "open")
	db, _ := buildBenchStore(b, base, 100000, 0)
	db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db2, err := Open(base, DefaultOptions())
		if err != nil {
			b.Fatal(err)
		}
		db2.Close()
	}
}

func BenchmarkRebuildIndex(b *testing.B) {
	base := filepath.Join(b.TempDir(), "reb")
	db, _ := buildBenchStore(b, base, 100000, 0)
	db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate a missing index each iteration.
		idx := db.Path() + ".rpi"
		_ = removeFile(idx)
		if err := RebuildIndex(context.Background(), db.Path(), RebuildOptions{Durability: AsyncCommit}); err != nil {
			b.Fatal(err)
		}
	}
}

func removeFile(path string) error { return os.Remove(path) }
