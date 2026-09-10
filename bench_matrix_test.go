package rowpack

// v2 统一基准矩阵（恢复自 011056d 删除的 bench_matrix_test.go，适配 v2 API）。
//
// BenchmarkMainMatrix 是矩阵唯一入口：按场景 × BlockSize × 缓存 × 持久化 ×
// I/O 路径的剪枝矩阵；BenchmarkLatency 补点读延迟分位数；BenchmarkEnv 自描述
// 运行环境与标准数据集几何。单条命令复现全部基线（见 Makefile bench 目标）：
//
//	go test -run '^$' -bench 'Benchmark(Env|MainMatrix|Latency)' -benchmem -count=1
//
// 每个子测试报告 ns/op、B/op、allocs/op（-benchmem）以及自定义指标：
// krows/s=行吞吐；kget/s=点读吞吐；dataMB=.rpk 单文件大小（v2 含内嵌
// IndexTxn）；ratio=存储字节/原始字节（压缩率，越小越好）；bytePerRow=落盘
// 字节/行；idxMB=索引常驻内存；hitpct/scanhitpct=块缓存/扫描窗口命中率%；
// rssdMB=进程峰值 RSS 增量（本子测试归属，近似）。
//
// 场景 × 维度剪枝规则（控制总时长，覆盖全部有意义组合）：
//   - 写场景：BlockSize × 持久化（Sync/Async）；不读盘，无 I/O 路径维度。
//   - 100k 读场景：BlockSize × 缓存（hot/cold）× I/O（mmap/readat）。
//   - 1M 读场景：仅 BlockSize × mmap（构建 1M 行耗时高，readat 对比以 100k 为准）。
//   - OpenReplay / IndexRebuild：仅 BlockSize（不读块缓存）。
//   - DELTA 链：仅 BlockSize × mmap。
//   - 并发点读：仅缓存热档 × I/O（锁竞争与块大小无关）。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/iofile"
	"github.com/stretchr/testify/require"
)

// benchCtx carries one matrix cell's dimensions.
type benchCtx struct {
	bs     int  // block size; 0 = default
	cold   bool // -1 CacheBytes (cache disabled)
	async  bool // AsyncCommit durability
	readAt bool // force the ReadAt I/O path (no mmap)
}

func (c benchCtx) opts() Options {
	o := Options{Durability: SyncCommit}
	if c.async {
		o.Durability = AsyncCommit
	}
	if c.bs > 0 {
		o.BlockSize = c.bs
	}
	if c.cold {
		o.CacheBytes = -1
	}
	return o
}

// applyIO sets the ReadAt-force toggle for the duration of the subtest. Only
// new views pick it up, so every matrix cell must Create/Open fresh files
// while the toggle is active (all cell builders do).
func (c benchCtx) applyIO(b *testing.B) {
	iofile.ForceReadAt(c.readAt)
	b.Cleanup(func() { iofile.ForceReadAt(false) })
}

// peakRSSBytes returns the process peak RSS via getrusage (Darwin reports
// bytes, Linux reports KiB). Returns -1 if unavailable; callers skip the
// rssdMB metric then.
func peakRSSBytes() int64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return -1
	}
	max := ru.Maxrss
	if runtime.GOOS == "linux" {
		max *= 1024
	}
	return int64(max)
}

// reportStatsMetrics reports data size, compression ratio, per-row footprint,
// index memory and the peak RSS delta attributed to this subtest.
func reportStatsMetrics(b *testing.B, st Stats, rssBefore int64, rows uint64) {
	if st.DataFileBytes > 0 {
		b.ReportMetric(float64(st.DataFileBytes)/(1<<20), "dataMB")
	}
	if st.RawBytes > 0 {
		b.ReportMetric(float64(st.StoredBytes)/float64(st.RawBytes), "ratio")
	}
	if rows > 0 {
		b.ReportMetric(float64(st.StoredBytes)/float64(rows), "bytePerRow")
	}
	b.ReportMetric(float64(st.IndexMemoryBytes)/(1<<20), "idxMB")
	if rssBefore > 0 {
		b.ReportMetric(float64(peakRSSBytes()-rssBefore)/(1<<20), "rssdMB")
	}
}

// reportCacheMetrics reports the block cache hit percentage when there is
// cache activity (cold/cache-disabled runs report nothing).
func reportCacheMetrics(b *testing.B, st Stats) {
	h, m := st.Cache.Hits, st.Cache.Misses
	if h+m > 0 {
		b.ReportMetric(100*float64(h)/float64(h+m), "hitpct")
	}
	h, m = st.ScanCache.Hits, st.ScanCache.Misses
	if h+m > 0 {
		b.ReportMetric(100*float64(h)/float64(h+m), "scanhitpct")
	}
}

func bsLabel(bs int) string {
	if bs == 0 {
		return "256K" // resolved default (README reference config)
	}
	if bs >= 1<<20 {
		return strconv.Itoa(bs>>20) + "M"
	}
	return strconv.Itoa(bs>>10) + "K"
}

var matrixBlockSizes = []int{64 << 10, 256 << 10, 1 << 20}

var matrixIOModes = []struct {
	name string
	on   bool
}{
	{"mmap", false},
	{"readat", true},
}

var matrixDurations = []struct {
	name  string
	async bool
}{
	{"sync", false},
	{"async", true},
}

// isoRow is one fixed row used by the isolated-write scenario: identical row
// content strips per-row encoding variance (fmt.Sprintf, varying bytes) so
// the number isolates the engine write path.
var isoRow = Row{
	Uint64(42),
	Int64(7),
	Int64(-7),
	Float64(0.25),
	String("iso-row"),
	DateTime(testTime),
	Bytes([]byte{1, 2, 3, 4}),
}

// deltaRow builds a row for DELTA-chain inserts; shape matches benchCols.
func deltaRow(id uint64, i int) Row {
	return Row{
		Uint64(id),
		Int64(int64(i)),
		Int64(-int64(i)),
		Float64(float64(i) * 0.5),
		String("delta-row"),
		DateTime(testTime),
		Bytes([]byte{byte(id), byte(id >> 8)}),
	}
}

// ---- 场景实现 ----

func benchWriteFull(b *testing.B, c benchCtx) {
	rows := benchRows
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	var lastStats Stats
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "w")
		db, err := Create(base, c.opts())
		require.NoError(b, err)
		w, err := db.Begin(ctx, NoParent)
		require.NoError(b, err)
		require.NoError(b, w.DefineTable("t", benchCols()))
		b.StartTimer()
		for j := uint64(0); j < uint64(rows); j++ {
			require.NoError(b, w.Insert("t", j+1, benchRow(j+1)))
		}
		_, err = w.Commit(ctx)
		require.NoError(b, err)
		b.StopTimer()
		lastStats = db.Stats()
		require.NoError(b, db.Close())
	}
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	reportStatsMetrics(b, lastStats, rssBefore, uint64(rows))
}

func benchWriteIsolated(b *testing.B, c benchCtx) {
	rows := benchRows
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	var lastStats Stats
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "wi")
		db, err := Create(base, c.opts())
		require.NoError(b, err)
		w, err := db.Begin(ctx, NoParent)
		require.NoError(b, err)
		require.NoError(b, w.DefineTable("t", benchCols()))
		b.StartTimer()
		for j := uint64(0); j < uint64(rows); j++ {
			require.NoError(b, w.Insert("t", j+1, isoRow))
		}
		_, err = w.Commit(ctx)
		require.NoError(b, err)
		b.StopTimer()
		lastStats = db.Stats()
		require.NoError(b, db.Close())
	}
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	reportStatsMetrics(b, lastStats, rssBefore, uint64(rows))
}

func benchGet(b *testing.B, c benchCtx, fullRange bool) {
	rows := benchRows
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "g")
	db, snap := benchStoreAt(b, base, c.opts(), rows)
	defer db.Close()
	var dst Row
	warm := 100
	if c.cold {
		warm = 10 // cold reads are ~300µs each; keep burn-in short
	}
	// Warm + burn-in before the timer: the first iterations after a
	// benchmarking reset absorb one-time costs (GC, allocator, page cache),
	// which would otherwise dominate per-op ns at low benchtime.
	for i := uint64(1); i <= uint64(warm); i++ {
		row, err := db.Get(ctx, snap, "t", i, dst)
		require.NoError(b, err)
		dst = row[:0]
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rowID := uint64(i%100) + 1
		if fullRange {
			rowID = uint64(i%rows) + 1
		}
		row, err := db.Get(ctx, snap, "t", rowID, dst)
		require.NoError(b, err)
		dst = row[:0]
	}
	// Freeze the timer before collecting store stats: Stats() merges the full
	// logical row count (k-way heap) and must not count into ns/op.
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore, uint64(rows))
	reportCacheMetrics(b, st)
}

func benchConcurrentGet(b *testing.B, c benchCtx, g int) {
	rows := benchRows
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "cg")
	db, snap := benchStoreAt(b, base, c.opts(), rows)
	defer db.Close()
	// Warm the whole cache so the benchmark measures concurrent hot reads.
	for i := uint64(0); i < uint64(rows); i++ {
		_, err := db.Get(ctx, snap, "t", i+1, nil)
		require.NoError(b, err)
	}
	b.ResetTimer()
	// No require.* in the parallel loop: testing.T.Helper() takes the test
	// mutex (92% of the mutex profile's contention) and would make this measure
	// testify, not the store; errors surface after the loop. Each goroutine
	// reuses its own dst like BenchmarkGetHot — nil would add one []Value
	// allocation per get.
	errCh := make(chan error, 1)
	b.RunParallel(func(pb *testing.PB) {
		var dst Row
		i := uint64(0)
		for pb.Next() {
			i++
			row, err := db.Get(ctx, snap, "t", i%uint64(rows)+1, dst)
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			dst = row[:0]
		}
	})
	b.StopTimer()
	select {
	case err := <-errCh:
		b.Fatal(err)
	default:
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	_ = g // goroutine count is set by GOMAXPROCS via -cpu; kept for name clarity
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore, uint64(rows))
	reportCacheMetrics(b, st)
}

func benchScan(b *testing.B, c benchCtx) {
	rows := benchRows
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "sc")
	db, snap := benchStoreAt(b, base, c.opts(), rows)
	defer db.Close()
	// Burn-in: two scans absorb one-time costs before the timer.
	for i := 0; i < 2; i++ {
		it, err := db.Scan(ctx, snap, "t", ScanOptions{})
		require.NoError(b, err)
		for {
			if _, ok := it.Next(); !ok {
				break
			}
		}
		require.NoError(b, it.Err())
		it.Close()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(ctx, snap, "t", ScanOptions{})
		require.NoError(b, err)
		n := 0
		for {
			if _, ok := it.Next(); !ok {
				break
			}
			n++
		}
		require.NoError(b, it.Err())
		it.Close()
		require.Equal(b, rows, n, "scan returned %d rows, want %d", n, rows)
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore, uint64(rows))
	reportCacheMetrics(b, st)
}

func benchScan1M(b *testing.B, c benchCtx) {
	rows := benchRows1M
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "s1m")
	db, snap := benchStoreAt(b, base, c.opts(), rows)
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(ctx, snap, "t", ScanOptions{})
		require.NoError(b, err)
		n := 0
		for {
			if _, ok := it.Next(); !ok {
				break
			}
			n++
		}
		require.NoError(b, it.Err())
		it.Close()
		require.Equal(b, rows, n, "scan returned %d rows, want %d", n, rows)
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore, uint64(rows))
	reportCacheMetrics(b, st)
}

func benchGetRandom1M(b *testing.B, c benchCtx) {
	rows := benchRows1M
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "r1m")
	db, snap := benchStoreAt(b, base, c.opts(), rows)
	defer db.Close()
	var rng uint64 = 88172645463325252
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		_, err := db.Get(ctx, snap, "t", rng%uint64(rows)+1, nil)
		require.NoError(b, err)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore, uint64(rows))
	reportCacheMetrics(b, st)
}

// buildDeltaChainStore builds a FULL (rows) + depth DELTAs of deltaRows each,
// returning the open store and the chain head.
func buildDeltaChainStore(b *testing.B, base string, depth, deltaRows, rows, bs int) (*Store, SnapshotID) {
	b.Helper()
	db, full := benchStoreAt(b, base, Options{BlockSize: bs}, rows)
	ctx := context.Background()
	parent := full
	nextID := uint64(rows + 1)
	for d := 0; d < depth; d++ {
		w, err := db.Begin(ctx, parent)
		require.NoError(b, err)
		for i := 0; i < deltaRows; i++ {
			require.NoError(b, w.Insert("t", nextID, deltaRow(nextID, i)))
			nextID++
		}
		parent, err = w.Commit(ctx)
		require.NoError(b, err)
	}
	return db, parent
}

func benchDeepChain(b *testing.B, c benchCtx, scan bool) {
	rows := benchRows
	const depth = 32
	const deltaRows = 1000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	ctx := context.Background()
	base := filepath.Join(tmpdb(b), "dc")
	db, head := buildDeltaChainStore(b, base, depth, deltaRows, rows, c.bs)
	defer db.Close()
	if !scan {
		// Warm the whole cache: measures chain-walk + hot block read.
		for i := uint64(1); i <= uint64(rows); i++ {
			_, err := db.Get(ctx, head, "t", i, nil)
			require.NoError(b, err)
		}
	}
	b.ResetTimer()
	if scan {
		want := rows + depth*deltaRows
		for i := 0; i < b.N; i++ {
			it, err := db.Scan(ctx, head, "t", ScanOptions{})
			require.NoError(b, err)
			n := 0
			for {
				if _, ok := it.Next(); !ok {
					break
				}
				n++
			}
			require.NoError(b, it.Err())
			it.Close()
			require.Equal(b, want, n, "chain scan returned %d rows, want %d", n, want)
		}
		b.StopTimer()
		b.ReportMetric(float64(want)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	} else {
		var rng uint64 = 1442695040888963407
		for i := 0; i < b.N; i++ {
			rng = rng*6364136223846793005 + 1
			_, err := db.Get(ctx, head, "t", rng%uint64(rows)+1, nil)
			require.NoError(b, err)
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	}
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore, uint64(rows))
	reportCacheMetrics(b, st)
}

func benchOpenReplay(b *testing.B, c benchCtx) {
	c.applyIO(b)
	base := filepath.Join(tmpdb(b), "op")
	db, _ := benchStoreAt(b, base, c.opts(), benchRows)
	require.NoError(b, db.Close())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db2, err := Open(base, c.opts())
		require.NoError(b, err)
		require.NoError(b, db2.Close())
	}
}

// tamperFirstIndexTxn closes db and flips bytes in the middle of the FIRST
// snapshot's IndexTxn extent, so every subsequent Open rebuilds that
// snapshot's index in memory (v2 §10.2). The file is never rewritten.
func tamperFirstIndexTxn(b *testing.B, db *Store, base string) {
	b.Helper()
	committed, _, err := db.scanDataFile()
	require.NoError(b, err)
	require.NotEmpty(b, committed)
	s1 := committed[0]
	require.Greater(b, s1.txnEnd-s1.txnStart, int64(16))
	require.NoError(b, db.Close())
	mid := s1.txnStart + (s1.txnEnd-s1.txnStart)/2
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(b, err)
	_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
	require.NoError(b, err)
	require.NoError(b, f.Close())
}

func benchIndexRebuildOnOpen(b *testing.B, c benchCtx) {
	c.applyIO(b)
	base := filepath.Join(tmpdb(b), "rb")
	db, _ := benchStoreAt(b, base, c.opts(), benchRows)
	tamperFirstIndexTxn(b, db, base)
	// One guarded open outside the timer: proves the rebuild path engages.
	db2, err := Open(base, c.opts())
	require.NoError(b, err)
	require.True(b, db2.Stats().Recovery.Performed, "tampered IndexTxn must trigger rebuild")
	require.NoError(b, db2.Close())
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d, err := Open(base, c.opts())
		require.NoError(b, err)
		require.NoError(b, d.Close())
	}
}

// ---- 矩阵入口 ----

// BenchmarkMainMatrix runs the pruned v2 benchmark matrix (see file doc).
func BenchmarkMainMatrix(b *testing.B) {
	for _, io := range matrixIOModes {
		for _, bs := range matrixBlockSizes {
			io, bs := io, bs
			name := fmt.Sprintf("get_hot/bs=%s/io=%s", bsLabel(bs), io.name)
			b.Run(name, func(b *testing.B) {
				benchGet(b, benchCtx{bs: bs, readAt: io.on}, false)
			})
			name = fmt.Sprintf("get_hot_into/bs=%s/io=%s", bsLabel(bs), io.name)
			b.Run(name, func(b *testing.B) {
				benchGet(b, benchCtx{bs: bs, readAt: io.on}, true)
			})
			name = fmt.Sprintf("get_cold/bs=%s/io=%s", bsLabel(bs), io.name)
			b.Run(name, func(b *testing.B) {
				benchGet(b, benchCtx{bs: bs, cold: true, readAt: io.on}, false)
			})
			for _, ca := range []struct {
				name string
				v    bool
			}{{"hot", false}, {"cold", true}} {
				name = fmt.Sprintf("scan/bs=%s/cache=%s/io=%s", bsLabel(bs), ca.name, io.name)
				b.Run(name, func(b *testing.B) {
					benchScan(b, benchCtx{bs: bs, cold: ca.v, readAt: io.on})
				})
			}
		}
		// Concurrent hot gets: cache-hot only, 8 and 64 goroutines.
		for _, g := range []int{8, 64} {
			name := fmt.Sprintf("conc_get_g%d/io=%s", g, io.name)
			b.Run(name, func(b *testing.B) {
				benchConcurrentGet(b, benchCtx{readAt: io.on}, g)
			})
		}
	}
	for _, bs := range matrixBlockSizes {
		bs := bs
		for _, d := range matrixDurations {
			name := fmt.Sprintf("write_full/bs=%s/dur=%s", bsLabel(bs), d.name)
			b.Run(name, func(b *testing.B) {
				benchWriteFull(b, benchCtx{bs: bs, async: d.async})
			})
			name = fmt.Sprintf("write_iso/bs=%s/dur=%s", bsLabel(bs), d.name)
			b.Run(name, func(b *testing.B) {
				benchWriteIsolated(b, benchCtx{bs: bs, async: d.async})
			})
		}
		// Heavy 1M scenarios: block size only (mmap path).
		name := fmt.Sprintf("scan1m/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchScan1M(b, benchCtx{bs: bs})
		})
		name = fmt.Sprintf("getrand1m/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchGetRandom1M(b, benchCtx{bs: bs})
		})
		name = fmt.Sprintf("get_deepchain/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchDeepChain(b, benchCtx{bs: bs}, false)
		})
		name = fmt.Sprintf("scan_deepchain/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchDeepChain(b, benchCtx{bs: bs}, true)
		})
		name = fmt.Sprintf("open_replay/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchOpenReplay(b, benchCtx{bs: bs})
		})
		name = fmt.Sprintf("index_rebuild/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchIndexRebuildOnOpen(b, benchCtx{bs: bs})
		})
	}
}

// ---- 延迟分位数 ----

// measureLatency runs fn n times, collecting per-op durations, and returns
// the p50/p95/p99 in nanoseconds.
func measureLatency(b *testing.B, n int, fn func() error) (p50, p95, p99 time.Duration) {
	b.Helper()
	durs := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		require.NoError(b, fn())
		durs[i] = time.Since(start)
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	px := func(q int) time.Duration { return durs[n*q/100] }
	return px(50), px(95), px(99)
}

func reportLatency(b *testing.B, p50, p95, p99 time.Duration) {
	b.ReportMetric(float64(p50.Nanoseconds()), "p50ns")
	b.ReportMetric(float64(p95.Nanoseconds()), "p95ns")
	b.ReportMetric(float64(p99.Nanoseconds()), "p99ns")
}

// BenchmarkLatency measures fixed-sample latency percentiles for the point
// read paths (hot / cold / deep chain), complementing the throughput matrix.
func BenchmarkLatency(b *testing.B) {
	const samples = 4096
	b.Run("get_hot", func(b *testing.B) {
		ctx := context.Background()
		base := filepath.Join(tmpdb(b), "lh")
		db, snap := benchStoreAt(b, base, Options{}, benchRows)
		defer db.Close()
		for i := uint64(0); i < 100; i++ {
			_, err := db.Get(ctx, snap, "t", i+1, nil)
			require.NoError(b, err)
		}
		var dst Row
		var rng uint64 = 1
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			row, err := db.Get(ctx, snap, "t", rng%100+1, dst)
			dst = row[:0]
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
	b.Run("get_hot_into", func(b *testing.B) {
		ctx := context.Background()
		base := filepath.Join(tmpdb(b), "lhi")
		db, snap := benchStoreAt(b, base, Options{}, benchRows)
		defer db.Close()
		for i := uint64(0); i < 100; i++ {
			_, err := db.Get(ctx, snap, "t", i+1, nil)
			require.NoError(b, err)
		}
		var dst Row
		var rng uint64 = 2
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			row, err := db.Get(ctx, snap, "t", rng%100+1, dst)
			dst = row[:0]
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
	b.Run("get_cold", func(b *testing.B) {
		ctx := context.Background()
		base := filepath.Join(tmpdb(b), "lc")
		db, snap := benchStoreAt(b, base, Options{CacheBytes: -1}, benchRows)
		defer db.Close()
		var rng uint64 = 3
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			_, err := db.Get(ctx, snap, "t", rng%uint64(benchRows)+1, nil)
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
	b.Run("get_deepchain", func(b *testing.B) {
		ctx := context.Background()
		base := filepath.Join(tmpdb(b), "ldc")
		db, head := buildDeltaChainStore(b, base, 32, 1000, benchRows, 0)
		defer db.Close()
		for i := uint64(1); i <= uint64(benchRows); i++ {
			_, err := db.Get(ctx, head, "t", i, nil)
			require.NoError(b, err)
		}
		var rng uint64 = 4
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			_, err := db.Get(ctx, head, "t", rng%uint64(benchRows)+1, nil)
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
}

// ---- 环境与数据集几何 ----

// BenchmarkEnv self-describes the runtime and reports the geometry of the
// standard 100k × 7-column dataset at the README reference config: single
// file size (dataMB, v2 including embedded IndexTxn), compression ratio,
// per-row footprint and index memory.
func BenchmarkEnv(b *testing.B) {
	b.Logf("env: %s/%s go=%s", runtime.GOOS, runtime.GOARCH, runtime.Version())
	base := filepath.Join(tmpdb(b), "env")
	db, _ := benchStoreAt(b, base, Options{}, benchRows)
	defer db.Close()
	st := db.Stats()
	require.Equal(b, uint64(benchRows), st.LogicalRows)
	b.ReportMetric(float64(st.DataFileBytes)/(1<<20), "dataMB")
	b.ReportMetric(float64(st.StoredBytes)/float64(st.RawBytes), "ratio")
	b.ReportMetric(float64(st.RawBytes)/float64(st.LogicalRows), "rawB/row")
	b.ReportMetric(float64(st.StoredBytes)/float64(st.LogicalRows), "bytePerRow")
	b.ReportMetric(float64(st.IndexMemoryBytes)/(1<<20), "idxMB")
	b.ReportMetric(float64(st.Blocks), "blocks")
}
