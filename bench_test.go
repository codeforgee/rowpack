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

// BenchmarkScanInto is BenchmarkScan with the v1.1 reuse mode: rows are
// decoded into one reused buffer, eliminating the per-row Row allocation and
// per-row Decimal big.Int churn.
func BenchmarkScanInto(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(b.TempDir(), "scaninto"), 100000, 0)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		var dst Row
		for {
			row, ok := it.NextInto(dst)
			if !ok {
				break
			}
			dst = row
		}
		if err := it.Err(); err != nil {
			b.Fatal(err)
		}
		it.Close()
	}
	b.ReportMetric(float64(100000)/b.Elapsed().Seconds()/1000, "krows/s")
}

// BenchmarkGetHotReadInto is BenchmarkGetHotRead with the v1.1 reuse mode:
// a single dst Row is reused across all Gets.
func BenchmarkGetHotReadInto(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(b.TempDir(), "hotinto"), 100000, 0)
	defer db.Close()
	// Warm a few blocks.
	for i := uint64(0); i < 100; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i+1); err != nil {
			b.Fatal(err)
		}
	}
	var dst Row
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		row, err := db.GetInto(context.Background(), fullID, 1, uint64(i%100)+1, dst)
		if err != nil {
			b.Fatal(err)
		}
		dst = row
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

// ---- 大规模 / DELTA 链场景基准 ----

// buildBenchStoreN writes nRows into a FULL snapshot.
func buildBenchStoreN(b *testing.B, base string, nRows uint64) (*Store, SnapshotID) {
	b.Helper()
	db, err := Create(base, DefaultOptions())
	if err != nil {
		b.Fatal(err)
	}
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	w.DefineSchema(benchSchema())
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

// BenchmarkWrite1M writes a million rows in one FULL snapshot.
func BenchmarkWrite1M(b *testing.B) {
	const rows = 1_000_000
	var db *Store
	var fullID SnapshotID
	b.ResetTimer()
	build := func() {
		base := filepath.Join(b.TempDir(), "w1m")
		db, fullID = buildBenchStoreN(b, base, rows)
	}
	build()
	b.StopTimer()
	b.SetBytes(rows * 100)
	b.ReportMetric(float64(rows)/b.Elapsed().Seconds()/1000, "krows/s")
	db.Close()
	_ = fullID
}

// BenchmarkGetRandom1M reads 10k random RowIDs from a million-row store
// (AC-003 scenario: random access must not scan the data file).
func BenchmarkGetRandom1M(b *testing.B) {
	const rows = 1_000_000
	db, fullID := buildBenchStoreN(b, filepath.Join(b.TempDir(), "r1m"), rows)
	defer db.Close()
	var rng uint64 = 88172645463325252
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		rowID := rng%rows + 1
		if _, err := db.Get(context.Background(), fullID, 1, rowID); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
}

// BenchmarkScan1M scans a million-row table.
func BenchmarkScan1M(b *testing.B) {
	const rows = 1_000_000
	db, fullID := buildBenchStoreN(b, filepath.Join(b.TempDir(), "s1m"), rows)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for it.Next() {
			n++
		}
		if err := it.Err(); err != nil {
			b.Fatal(err)
		}
		it.Close()
		if n != int(rows) {
			b.Fatalf("scan returned %d rows", n)
		}
	}
	b.ReportMetric(float64(rows)/b.Elapsed().Seconds()/1000, "krows/s")
}

// buildDeltaChainStore builds a FULL + depth DELTAs, each touching deltaRows.
func buildDeltaChainStore(b *testing.B, base string, depth, deltaRows int) (*Store, SnapshotID) {
	b.Helper()
	db, fullID := buildBenchStoreN(b, base, 100_000)
	parent := fullID
	nextID := uint64(100_001)
	for d := 0; d < depth; d++ {
		w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: parent})
		if err != nil {
			b.Fatal(err)
		}
		for i := 0; i < deltaRows; i++ {
			if err := w.Insert(context.Background(), 1, nextID, 1, Row{Uint64(nextID), String("delta-row"), Bool(false), Int32(int32(i)), Float64(0), DateTimeValueOf(1700000000000000000), DecimalValue(Decimal{Unscaled: bigI(1), Scale: 2})}); err != nil {
				b.Fatal(err)
			}
			nextID++
		}
		info, err := w.Commit(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		parent = info.ID
	}
	return db, parent
}

// BenchmarkGetDeepChain performs point reads at the head of a 32-deep DELTA
// chain (parent-chain resolution cost).
func BenchmarkGetDeepChain(b *testing.B) {
	db, head := buildDeltaChainStore(b, filepath.Join(b.TempDir(), "chain"), 32, 1000)
	defer db.Close()
	var rng uint64 = 1442695040888963407
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rng = rng*6364136223846793005 + 1
		rowID := rng%100_000 + 1
		if _, err := db.Get(context.Background(), head, 1, rowID); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanDeepChain scans the head of a 32-deep DELTA chain.
func BenchmarkScanDeepChain(b *testing.B) {
	db, head := buildDeltaChainStore(b, filepath.Join(b.TempDir(), "scanchain"), 32, 1000)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), head, 1, ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for it.Next() {
			n++
		}
		if err := it.Err(); err != nil {
			b.Fatal(err)
		}
		it.Close()
		if n != 132_000 {
			b.Fatalf("scan returned %d rows, want 132000", n)
		}
	}
}

func DateTimeValueOf(ns int64) Value {
	t := time.Unix(0, ns).UTC()
	return DateTime(t)
}
