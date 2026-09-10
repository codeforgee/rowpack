package rowpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/block"
)

// Benchmarks are deliberately single-configuration: one dataset shape and
// one default option set per metric, so `make bench` completes in seconds
// while still covering every hot path (write, hot/cold read, scan, batch,
// open/replay, deep chain, encryption). For matrix sweeps see
// bench_matrix_test.go.
//
// Dataset sizes are tunable for quick runs without touching code:
// ROWPACK_BENCH_ROWS / ROWPACK_BENCH_ROWS1M override the row counts (see the
// bench-quick Makefile target). Quick-run ns/op is NOT comparable to the
// documented 100k baseline; use it for structure/coverage smoke checks and
// order-of-magnitude sanity only.
var (
	benchRows   = envInt("ROWPACK_BENCH_ROWS", 100_000)
	benchRows1M = envInt("ROWPACK_BENCH_ROWS1M", 1_000_000)
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// benchCols is a 7-column row similar to the README reference baseline.
func benchCols() []Column {
	return []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "a", Type: TypeInt64},
		{Name: "b", Type: TypeInt64},
		{Name: "c", Type: TypeFloat64},
		{Name: "s", Type: TypeString},
		{Name: "t", Type: TypeDateTime},
		{Name: "b16", Type: TypeBytes},
	}
}

func benchRow(i uint64) Row {
	return Row{
		Uint64(i),
		Int64(int64(i * 7)),
		Int64(-int64(i * 3)),
		Float64(float64(i) * 0.25),
		String(fmt.Sprintf("row-%08d", i)),
		DateTime(testTime),
		Bytes([]byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}),
	}
}

var testTime = timeUnix(1757400000)

func timeUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// benchStore builds a FULL mixed-geometry store with n rows and returns it
// plus the snapshot ID. Marks the caller warm (stops the timer around the
// build). All dataset construction routes through benchStoreGeom so every
// benchmark shares the same frozen S0 geometries.
func benchStore(tb testing.TB, opts Options, n int) (*Store, SnapshotID) {
	tb.Helper()
	return benchStoreGeom(tb, opts, geomMixed, n)
}

func requireNilErr(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkWriteFull measures the sequential FULL write path: encode + block
// build + one commit. Reported as ns/row (see the krows/s derivation in
// docs/perf-report.md).
func BenchmarkWriteFull(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := filepath.Join(tmpdb(b), fmt.Sprintf("wf-%d", i))
		db, err := Create(base, Options{})
		requireNilErr(b, err)
		w, err := db.Begin(ctx, NoParent)
		requireNilErr(b, err)
		requireNilErr(b, w.DefineTable("t", benchCols()))
		b.StartTimer()
		for r := 1; r <= benchRows; r++ {
			requireNilErr(b, w.Insert("t", uint64(r), benchRow(uint64(r))))
		}
		if _, err := w.Commit(ctx); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		requireNilErr(b, db.Close())
	}
	b.SetBytes(int64(benchRows) * 64) // approximate row footprint for bytes/s reporting
}

// BenchmarkGetHot measures random reads served from the decoded-block cache
// with the documented dst-reuse pattern.
func BenchmarkGetHot(b *testing.B) {
	ctx := context.Background()
	db, snap := benchStore(b, Options{}, 20_000)
	b.Cleanup(func() { db.Close() })
	var dst Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := RowID(i%20_000) + 1
		row, err := db.Get(ctx, snap, "t", id, dst)
		if err != nil {
			b.Fatal(err)
		}
		dst = row[:0]
	}
	_ = dst
}

// BenchmarkGetCold measures random reads with the decoded-block cache
// disabled entirely (CacheBytes < 0; 0 resolves to the 64 MiB default): every
// Get pays block load + CRC + decompress into the pooled scratch. Custom
// metrics quantify the block-level read amplification (S0 frozen baselines):
//
//	readB/op  file bytes pulled per read (header + stored payload)
//	rawB/op   decompressed raw bytes produced per read
//
// Steady-state pooled allocation is B/op; the unpooled temp-allocation view
// is BenchmarkGetColdUnpooled.
func BenchmarkGetCold(b *testing.B) {
	ctx := context.Background()
	db, snap := benchStore(b, Options{CacheBytes: -1}, 20_000)
	b.Cleanup(func() { db.Close() })
	before := db.Stats().Read
	var dst Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := RowID(i%20_000) + 1
		row, err := db.Get(ctx, snap, "t", id, dst)
		if err != nil {
			b.Fatal(err)
		}
		dst = row[:0]
	}
	b.StopTimer()
	after := db.Stats().Read
	n := float64(b.N)
	b.ReportMetric(float64(after.ReadBytes-before.ReadBytes)/n, "readB/op")
	b.ReportMetric(float64(after.DecompressedBytes-before.DecompressedBytes)/n, "rawB/op")
}

// BenchmarkGetColdUnpooled is the temp-allocation twin of BenchmarkGetCold:
// the scratch pool is bypassed for the duration, so every Get visibly
// allocates (and drops) its full decompression buffer. B/op is the per-read
// temporary allocation the page-format refactor must cut from ~256 KiB to
// <= 64 KiB (FILE_FORMAT_REFACTOR_PLAN.md §3.1). The pool state is restored
// on exit; benchmarks run sequentially so the flip is race-free.
func BenchmarkGetColdUnpooled(b *testing.B) {
	prev := block.SetPoolDisabled(true)
	defer block.SetPoolDisabled(prev)
	ctx := context.Background()
	db, snap := benchStore(b, Options{CacheBytes: -1}, 20_000)
	b.Cleanup(func() { db.Close() })
	before := db.Stats().Read
	var dst Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		id := RowID(i%20_000) + 1
		row, err := db.Get(ctx, snap, "t", id, dst)
		if err != nil {
			b.Fatal(err)
		}
		dst = row[:0]
	}
	b.StopTimer()
	after := db.Stats().Read
	n := float64(b.N)
	b.ReportMetric(float64(after.ReadBytes-before.ReadBytes)/n, "readB/op")
	b.ReportMetric(float64(after.DecompressedBytes-before.DecompressedBytes)/n, "rawB/op")
}

// BenchmarkScan measures a full-table scan of 100k rows (single-pass,
// iterator buffered, no per-row allocations).
func BenchmarkScan(b *testing.B) {
	ctx := context.Background()
	db, snap := benchStore(b, Options{}, benchRows)
	b.Cleanup(func() { db.Close() })
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(ctx, snap, "t", ScanOptions{})
		if err != nil {
			b.Fatal(err)
		}
		var n int
		for {
			row, ok := it.Next()
			if !ok {
				break
			}
			if len(row) != 7 {
				b.Fatalf("row %d has %d columns", n, len(row))
			}
			n++
		}
		if err := it.Err(); err != nil {
			b.Fatal(err)
		}
		it.Close()
		if n != benchRows {
			b.Fatalf("scanned %d rows, want %d", n, benchRows)
		}
	}
}

// BenchmarkReadBatch1000 reads 1000 consecutive RowIDs in one batch call: the
// aggregation effect (blocks decompressed once) vs BenchmarkGetLoop1000 is
// the headline number; see README.
func BenchmarkReadBatch1000(b *testing.B) {
	ctx := context.Background()
	db, snap := benchStore(b, Options{}, benchRows)
	b.Cleanup(func() { db.Close() })
	ids := make([]RowID, 1000)
	for i := range ids {
		ids[i] = RowID(i) + 1
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.ReadBatch(ctx, snap, "t", ids)
		if err != nil {
			b.Fatal(err)
		}
		if len(rows) != 1000 {
			b.Fatalf("batch returned %d rows", len(rows))
		}
	}
}

// BenchmarkGetLoop1000 is the per-row baseline of the same 1000 RowIDs.
func BenchmarkGetLoop1000(b *testing.B) {
	ctx := context.Background()
	db, snap := benchStore(b, Options{}, benchRows)
	b.Cleanup(func() { db.Close() })
	ids := make([]RowID, 1000)
	for i := range ids {
		ids[i] = RowID(i) + 1
	}
	var dst Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, id := range ids {
			row, err := db.Get(ctx, snap, "t", id, dst)
			if err != nil {
				b.Fatal(err)
			}
			dst = row[:0]
		}
	}
}

// BenchmarkOpenReplay measures Open of a 100k-row store: header + scan +
// IndexTxn parse + schema derivation (the per-open index replay cost).
func BenchmarkOpenReplay(b *testing.B) {
	base := filepath.Join(tmpdb(b), "replay")
	db, _ := benchStoreAt(b, base, Options{}, benchRows)
	requireNilErr(b, db.Close())
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db2, err := Open(base, Options{})
		if err != nil {
			b.Fatal(err)
		}
		if err := db2.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

// benchStoreAt is benchStore writing to an explicit base path.
func benchStoreAt(tb testing.TB, base string, opts Options, n int) (*Store, SnapshotID) {
	tb.Helper()
	if opts.BlockSize == 0 {
		opts.BlockSize = 256 << 10
	}
	db, err := Create(base, opts)
	requireNilErr(tb, err)
	w, err := db.Begin(context.Background(), NoParent)
	requireNilErr(tb, err)
	requireNilErr(tb, w.DefineTable("t", benchCols()))
	for i := 1; i <= n; i++ {
		requireNilErr(tb, w.Insert("t", uint64(i), benchRow(uint64(i))))
	}
	snap, err := w.Commit(context.Background())
	requireNilErr(tb, err)
	return db, snap
}

// BenchmarkDeepChainGet resolves a row at the tip of a 32-snapshot DELTA
// chain: index resolution walks the parent chain, the row itself lives in
// snapshot 1's block.
func BenchmarkDeepChainGet(b *testing.B) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(b), "chain"), Options{})
	requireNilErr(b, err)
	b.Cleanup(func() { db.Close() })
	w, _ := db.Begin(ctx, NoParent)
	requireNilErr(b, w.DefineTable("t", benchCols()))
	for r := 1; r <= 100; r++ {
		requireNilErr(b, w.Insert("t", uint64(r), benchRow(uint64(r))))
	}
	snap, err := w.Commit(ctx)
	requireNilErr(b, err)
	const depth = 32
	for i := 0; i < depth; i++ {
		d, err := db.Begin(ctx, snap)
		requireNilErr(b, err)
		// One change per layer: rewrite row 1 (a delete of a parent-invisible
		// row would be rejected by the strict parent check).
		requireNilErr(b, d.Update("t", 1, benchRow(uint64(i+1))))
		snap, err = d.Commit(ctx)
		requireNilErr(b, err)
	}
	var dst Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		row, err := db.Get(ctx, snap, "t", RowID(i%100)+1, dst)
		if err != nil {
			b.Fatal(err)
		}
		dst = row[:0]
	}
}

// BenchmarkEncryptedWrite measures the FULL write path with AES-256-GCM
// block and IndexTxn chunk sealing enabled.
func BenchmarkEncryptedWrite(b *testing.B) {
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		base := filepath.Join(tmpdb(b), fmt.Sprintf("ew-%d", i))
		db, err := Create(base, encOptions("bk"))
		requireNilErr(b, err)
		w, err := db.Begin(ctx, NoParent)
		requireNilErr(b, err)
		requireNilErr(b, w.DefineTable("t", benchCols()))
		b.StartTimer()
		for r := 1; r <= benchRows; r++ {
			requireNilErr(b, w.Insert("t", uint64(r), benchRow(uint64(r))))
		}
		if _, err := w.Commit(ctx); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		requireNilErr(b, db.Close())
	}
	b.SetBytes(int64(benchRows) * 64)
}

// BenchmarkEncryptedGetHot measures cached random reads on an encrypted
// store (decrypt + decompress per block miss, then AESGCM on hot? blocks are
// cached decoded, so this measures the authenticated decode path).
func BenchmarkEncryptedGetHot(b *testing.B) {
	ctx := context.Background()
	db, snap := benchStore(b, encOptions("bk"), 20_000)
	b.Cleanup(func() { db.Close() })
	var dst Row
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		row, err := db.Get(ctx, snap, "t", RowID(i%20_000)+1, dst)
		if err != nil {
			b.Fatal(err)
		}
		dst = row[:0]
	}
}
