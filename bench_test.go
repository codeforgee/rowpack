package rowpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// Benchmarks record environment-dependent numbers; run with `go test -bench .`.
// Each benchmark reports the environment (Go version, Zstd version) via
// BenchmarkEnv.

func BenchmarkEnv(b *testing.B) {
	// Informational benchmark: prints the environment (Go version, module
	// versions, platform) plus the reference dataset geometry measured from a
	// freshly built store, so every run records reproducible context.
	bi, ok := debug.ReadBuildInfo()
	zstdVer := "unknown"
	goVer := runtime.Version()
	if ok {
		for _, dep := range bi.Deps {
			if dep.Path == "github.com/klauspost/compress" {
				zstdVer = dep.Version
			}
		}
	}
	base := filepath.Join(tmpdb(b), "env")
	db, _ := buildBenchStoreOpts(b, base, 100000, Options{})
	st := db.Stats()
	db.Close()
	bytesPerRow := 0.0
	if st.DataFileBytes > 0 {
		bytesPerRow = float64(st.DataFileBytes) / 100000
	}
	ratio := 0.0
	if st.RawBytes > 0 {
		ratio = float64(st.StoredBytes) / float64(st.RawBytes)
	}
	b.Logf("go=%s zstd=%s os=%s/%s cacheBytes=%d blockSize=%d dataset=100k rows x 7 cols "+
		"dataMB=%.1f ratio=%.3f bytePerRow=%.1f indexMB=%.1f",
		goVer, zstdVer, runtime.GOOS, runtime.GOARCH, fileformat.DefaultCacheBytes,
		fileformat.DefaultBlockSize, float64(st.DataFileBytes)/(1<<20), ratio, bytesPerRow,
		float64(st.IndexMemoryBytes)/(1<<20))
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

func buildBenchStore(b *testing.B, base string, nRows uint64, blockSize int) (*Store, SnapshotID) {
	b.Helper()
	opts := Options{}
	if blockSize > 0 {
		opts.BlockSize = blockSize
	}
	return buildBenchStoreOpts(b, base, nRows, opts)
}

// buildBenchStoreOpts builds nRows into a FULL snapshot with the given
// options. Options must already be defaults-resolved-safe (negative CacheBytes
// disables the cache). It is the unified setup used by all matrix benchmarks.
func buildBenchStoreOpts(b *testing.B, base string, nRows uint64, opts Options) (*Store, SnapshotID) {
	b.Helper()
	db, err := Create(base, opts)
	require.NoError(b, err)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(b, err)
	require.NoError(b, w.DefineSchema(benchSchema()))
	for i := uint64(0); i < nRows; i++ {
		require.NoError(b, w.Insert(context.Background(), 1, i+1, 1, benchRow(i)))
	}
	full, err := w.Commit(context.Background())
	require.NoError(b, err)
	return db, full.ID
}

func BenchmarkFullSequentialWrite(b *testing.B) {
	const rows = 100000
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "w")
		db, err := Create(base, Options{})
		require.NoError(b, err)
		w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
		require.NoError(b, w.DefineSchema(benchSchema()))
		b.StartTimer()
		for j := uint64(0); j < rows; j++ {
			require.NoError(b, w.Insert(context.Background(), 1, j+1, 1, benchRow(j)))
		}
		if _, err := w.Commit(context.Background()); err != nil {
			require.NoError(b, err)
		}
		b.StopTimer()
		db.Close()
	}
	b.SetBytes(rows * 100)
	b.ReportMetric(float64(rows)/b.Elapsed().Seconds()/1000, "krows/s")
}

func BenchmarkGetColdRead(b *testing.B) {
	base := filepath.Join(tmpdb(b), "cold")
	db, fullID := buildBenchStore(b, base, 100000, 0)
	db.Close() // release the writer lock before reopening
	// Disable cache to force cold reads.
	db2, err := Open(base, Options{CacheBytes: -1})
	require.NoError(b, err)
	defer db2.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db2.Get(context.Background(), fullID, 1, uint64(i%100000)+1, nil); err != nil {
			require.NoError(b, err)
		}
	}
}

func BenchmarkGetHotRead(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(tmpdb(b), "hot"), 100000, 0)
	defer db.Close()
	// Warm a few blocks.
	for i := uint64(0); i < 100; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
			require.NoError(b, err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, uint64(i%100)+1, nil); err != nil {
			require.NoError(b, err)
		}
	}
}

func BenchmarkConcurrentGet(b *testing.B) {
	for _, g := range []int{1, 8, 32, 64} {
		b.Run(fmt.Sprintf("g%d", g), func(b *testing.B) {
			db, fullID := buildBenchStore(b, filepath.Join(tmpdb(b), "conc"), 100000, 0)
			defer db.Close()
			// Warm the cache so the benchmark measures concurrent hot reads.
			for i := uint64(0); i < 100000; i++ {
				if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
					require.NoError(b, err)
				}
			}
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := uint64(0)
				for pb.Next() {
					i++
					if _, err := db.Get(context.Background(), fullID, 1, i%100000+1, nil); err != nil {
						require.NoError(b, err)
					}
				}
			})
		})
	}
}

func BenchmarkScan(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(tmpdb(b), "scan"), 100000, 0)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		if err != nil {
			require.NoError(b, err)
		}
		for {
			if _, ok := it.Next(); !ok {
				break
			}
		}
		if err := it.Err(); err != nil {
			require.NoError(b, err)
		}
		it.Close()
	}
}

// BenchmarkGetHotReadInto is BenchmarkGetHotRead with a reused dst Row
// across all Gets.
func BenchmarkGetHotReadInto(b *testing.B) {
	db, fullID := buildBenchStore(b, filepath.Join(tmpdb(b), "hotinto"), 100000, 0)
	defer db.Close()
	// Warm a few blocks.
	for i := uint64(0); i < 100; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
			require.NoError(b, err)
		}
	}
	var dst Row
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		row, err := db.Get(context.Background(), fullID, 1, uint64(i%100)+1, dst)
		if err != nil {
			require.NoError(b, err)
		}
		dst = row
	}
}

func BenchmarkOpenReplay(b *testing.B) {
	base := filepath.Join(tmpdb(b), "open")
	db, _ := buildBenchStore(b, base, 100000, 0)
	db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db2, err := Open(base, Options{})
		if err != nil {
			require.NoError(b, err)
		}
		db2.Close()
	}
}

func BenchmarkRebuildIndex(b *testing.B) {
	base := filepath.Join(tmpdb(b), "reb")
	db, _ := buildBenchStore(b, base, 100000, 0)
	db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Simulate a missing index each iteration.
		idx := db.Path() + ".rpi"
		_ = removeFile(idx)
		if err := RebuildIndex(context.Background(), db.Path(), RebuildOptions{Durability: AsyncCommit}); err != nil {
			require.NoError(b, err)
		}
	}
}

func removeFile(path string) error { return os.Remove(path) }

// ---- 大规模 / DELTA 链场景基准 ----

// buildBenchStoreN writes nRows into a FULL snapshot.
func buildBenchStoreN(b *testing.B, base string, nRows uint64) (*Store, SnapshotID) {
	b.Helper()
	db, err := Create(base, Options{})
	require.NoError(b, err)
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(b, w.DefineSchema(benchSchema()))
	for i := uint64(0); i < nRows; i++ {
		require.NoError(b, w.Insert(context.Background(), 1, i+1, 1, benchRow(i)))
	}
	full, err := w.Commit(context.Background())
	require.NoError(b, err)
	return db, full.ID
}

// BenchmarkWrite1M writes a million rows in one FULL snapshot.
func BenchmarkWrite1M(b *testing.B) {
	const rows = 1_000_000
	var db *Store
	var fullID SnapshotID
	b.ResetTimer()
	build := func() {
		base := filepath.Join(tmpdb(b), "w1m")
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
	db, fullID := buildBenchStoreN(b, filepath.Join(tmpdb(b), "r1m"), rows)
	defer db.Close()
	var rng uint64 = 88172645463325252
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		rowID := rng%rows + 1
		if _, err := db.Get(context.Background(), fullID, 1, rowID, nil); err != nil {
			require.NoError(b, err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
}

// BenchmarkScan1M scans a million-row table.
func BenchmarkScan1M(b *testing.B) {
	const rows = 1_000_000
	db, fullID := buildBenchStoreN(b, filepath.Join(tmpdb(b), "s1m"), rows)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		if err != nil {
			require.NoError(b, err)
		}
		n := 0
		for {
			if _, ok := it.Next(); !ok {
				break
			}
			n++
		}
		if err := it.Err(); err != nil {
			require.NoError(b, err)
		}
		it.Close()
		require.Equal(b, int(rows), n, "scan returned %d rows", n)
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
		require.NoError(b, err)
		for i := 0; i < deltaRows; i++ {
			require.NoError(b, w.Insert(context.Background(), 1, nextID, 1, Row{Uint64(nextID), String("delta-row"), Bool(false), Int32(int32(i)), Float64(0), DateTimeValueOf(1700000000000000000), DecimalValue(Decimal{Unscaled: bigI(1), Scale: 2})}))
			nextID++
		}
		info, err := w.Commit(context.Background())
		require.NoError(b, err)
		parent = info.ID
	}
	return db, parent
}

// BenchmarkGetDeepChain performs point reads at the head of a 32-deep DELTA
// chain (parent-chain resolution cost). The block cache is warmed first so
// the benchmark measures resolution and decode, not cold decompression.
func BenchmarkGetDeepChain(b *testing.B) {
	db, head := buildDeltaChainStore(b, filepath.Join(tmpdb(b), "chain"), 32, 1000)
	defer db.Close()
	for i := uint64(1); i <= 100_000; i++ {
		if _, err := db.Get(context.Background(), head, 1, i, nil); err != nil {
			require.NoError(b, err)
		}
	}
	var rng uint64 = 1442695040888963407
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rng = rng*6364136223846793005 + 1
		rowID := rng%100_000 + 1
		if _, err := db.Get(context.Background(), head, 1, rowID, nil); err != nil {
			require.NoError(b, err)
		}
	}
}

// BenchmarkScanDeepChain scans the head of a 32-deep DELTA chain.
func BenchmarkScanDeepChain(b *testing.B) {
	db, head := buildDeltaChainStore(b, filepath.Join(tmpdb(b), "scanchain"), 32, 1000)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), head, 1, ScanOptions{})
		if err != nil {
			require.NoError(b, err)
		}
		n := 0
		for {
			if _, ok := it.Next(); !ok {
				break
			}
			n++
		}
		if err := it.Err(); err != nil {
			require.NoError(b, err)
		}
		it.Close()
		require.Equal(b, 132_000, n, "scan returned %d rows, want 132000", n)
	}
}

func DateTimeValueOf(ns int64) Value {
	t := time.Unix(0, ns).UTC()
	return DateTime(t)
}

// isoRow is a prebuilt row: isolates the library write path from benchmark
// row-construction noise (fmt.Sprintf / big.NewInt in benchRow dominate the
// reported allocs of the FullSequentialWrite benchmarks).
func isoRow() Row {
	return Row{
		Uint64(1), String("user-1-abcdefghijklmnop"), Bool(true), Int32(1),
		Float64(0.5), DateTimeValueOf(1700000000000000000),
		DecimalValue(Decimal{Unscaled: bigI(100), Scale: 2}),
	}
}

// BenchmarkIsolatedWrite writes rows with a prebuilt Row so the reported
// allocs/bytes measure the library write path alone (Insert + Commit).
func BenchmarkIsolatedWrite(b *testing.B) {
	const rows = 100000
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "wisolated")
		db, err := Create(base, Options{})
		require.NoError(b, err)
		w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
		require.NoError(b, w.DefineSchema(benchSchema()))
		r := isoRow()
		b.StartTimer()
		for j := uint64(0); j < rows; j++ {
			if err := w.Insert(context.Background(), 1, j+1, 1, r); err != nil {
				require.NoError(b, err)
			}
		}
		if _, err := w.Commit(context.Background()); err != nil {
			require.NoError(b, err)
		}
		b.StopTimer()
		db.Close()
	}
	b.SetBytes(rows * 100)
	b.ReportMetric(float64(rows)/b.Elapsed().Seconds()/1000, "krows/s")
}
