package rowpack

// v1.2 Tier 0 统一基准矩阵（plan-v12 §2）。
//
// BenchmarkMainMatrix 是唯一入口：按场景 × BlockSize × 缓存 × 持久化 × I/O
// 路径的剪枝矩阵，单条命令复现全部当前基线：
//
//	go test -run '^$' -bench 'Benchmark(Env|MainMatrix|Latency)' -benchmem -count=1
//
// 每个子测试报告 ns/op、B/op、allocs/op（-benchmem）以及自定义指标：
// krows/s=行吞吐；dataMB=.rpk 数据文件大小；ratio=存储字节/原始字节（压缩率；
// 越小越好）；idxMB=索引常驻内存；hitpct=块缓存命中率%（读场景）；
// rssdMB=进程峰值 RSS 增量（本子测试归属）。
//
// 场景 × 维度剪枝规则（控制总时长，覆盖全部有意义组合）：
//   - 写场景：BlockSize × 持久化（Sync/Async）；不读盘，无 I/O 路径维度。
//   - 100k 读场景：BlockSize × 缓存（hot/cold）× I/O（mmap/readat）。
//   - 1M 读场景：仅 BlockSize × mmap（构建 1M 行耗时高，readat 对比以 100k 为准）。
//   - OpenReplay / RebuildIndex：仅 BlockSize（不读块缓存）。
//   - DELTA 链：仅 BlockSize × mmap。

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
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

// applyIO sets the ReadAt-force toggle for the duration of the subtest.
func (c benchCtx) applyIO(b *testing.B) {
	iofile.ForceReadAt(c.readAt)
	b.Cleanup(func() { iofile.ForceReadAt(false) })
}

// reportStatsMetrics reports data size, compression ratio, index memory and
// the peak RSS delta attributed to this subtest.
func reportStatsMetrics(b *testing.B, st Stats, rssBefore int64) {
	if st.DataFileBytes > 0 {
		b.ReportMetric(float64(st.DataFileBytes)/(1<<20), "dataMB")
	}
	if st.RawBytes > 0 {
		b.ReportMetric(float64(st.StoredBytes)/float64(st.RawBytes), "ratio")
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

// ---- 场景实现 ----

func benchWriteFull(b *testing.B, c benchCtx) {
	const rows = 100000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	var lastStats Stats
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "w")
		db, err := Create(base, c.opts())
		require.NoError(b, err)
		w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
		require.NoError(b, w.DefineSchema(benchSchema()))
		b.StartTimer()
		for j := uint64(0); j < rows; j++ {
			if err := w.Insert(context.Background(), 1, j+1, 1, benchRow(j)); err != nil {
				require.NoError(b, err)
			}
		}
		if _, err := w.Commit(context.Background()); err != nil {
			require.NoError(b, err)
		}
		b.StopTimer()
		lastStats = db.Stats()
		db.Close()
	}
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	reportStatsMetrics(b, lastStats, rssBefore)
}

func benchWriteIsolated(b *testing.B, c benchCtx) {
	const rows = 100000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	var lastStats Stats
	r := isoRow()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "wi")
		db, err := Create(base, c.opts())
		require.NoError(b, err)
		w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
		require.NoError(b, w.DefineSchema(benchSchema()))
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
		lastStats = db.Stats()
		db.Close()
	}
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	reportStatsMetrics(b, lastStats, rssBefore)
}

func benchGet(b *testing.B, c benchCtx, into bool) {
	const rows = 100000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	base := filepath.Join(tmpdb(b), "g")
	db, fullID := buildBenchStoreOpts(b, base, rows, c.opts())
	defer db.Close()
	var dst Row
	warm := 100
	if c.cold {
		warm = 10 // cold reads are ~270µs each; keep burn-in short
	}
	// Warm + burn-in before the timer: the first iterations after a
	// benchmarking reset absorb one-time costs (GC, allocator, page cache),
	// which would otherwise dominate per-op ns at low benchtime.
	for i := uint64(1); i <= uint64(warm); i++ {
		row, err := db.Get(context.Background(), fullID, 1, i, dst)
		require.NoError(b, err)
		dst = row
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rowID := uint64(i%100) + 1
		if into {
			rowID = uint64(i%rows) + 1
		}
		row, err := db.Get(context.Background(), fullID, 1, rowID, dst)
		require.NoError(b, err)
		dst = row
	}
	// Freeze the timer before collecting store stats: Stats() merges the full
	// logical row count (k-way heap) and must not count into ns/op.
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore)
	reportCacheMetrics(b, st)
}

func benchConcurrentGet(b *testing.B, c benchCtx, g int) {
	const rows = 100000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	base := filepath.Join(tmpdb(b), "cg")
	db, fullID := buildBenchStoreOpts(b, base, rows, c.opts())
	defer db.Close()
	// Warm the whole cache so the benchmark measures concurrent hot reads.
	for i := uint64(0); i < rows; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
			require.NoError(b, err)
		}
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := uint64(0)
		for pb.Next() {
			i++
			if _, err := db.Get(context.Background(), fullID, 1, i%rows+1, nil); err != nil {
				require.NoError(b, err)
			}
		}
	})
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore)
	reportCacheMetrics(b, st)
}

func benchScan(b *testing.B, c benchCtx) {
	const rows = 100000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	base := filepath.Join(tmpdb(b), "sc")
	db, fullID := buildBenchStoreOpts(b, base, rows, c.opts())
	defer db.Close()
	// Burn-in: two scans absorb one-time costs before the timer.
	for i := 0; i < 2; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		require.NoError(b, err)
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		require.NoError(b, err)
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
		require.Equal(b, int(rows), n, "scan returned %d rows, want %d", n, rows)
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore)
	reportCacheMetrics(b, st)
}

func benchScan1M(b *testing.B, c benchCtx) {
	const rows = 1000000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	base := filepath.Join(tmpdb(b), "s1m")
	db, fullID := buildBenchStoreOpts(b, base, rows, c.opts())
	defer db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
		require.NoError(b, err)
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
		require.Equal(b, int(rows), n, "scan returned %d rows, want %d", n, rows)
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore)
	reportCacheMetrics(b, st)
}

func benchGetRandom1M(b *testing.B, c benchCtx) {
	const rows = 1000000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	base := filepath.Join(tmpdb(b), "r1m")
	db, fullID := buildBenchStoreOpts(b, base, rows, c.opts())
	defer db.Close()
	var rng uint64 = 88172645463325252
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		if _, err := db.Get(context.Background(), fullID, 1, rng%rows+1, nil); err != nil {
			require.NoError(b, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore)
	reportCacheMetrics(b, st)
}

func benchDeepChain(b *testing.B, c benchCtx, scan bool) {
	const rows = 100000
	const depth = 32
	const deltaRows = 1000
	c.applyIO(b)
	rssBefore := peakRSSBytes()
	base := filepath.Join(tmpdb(b), "dc")
	db, head := buildDeltaChainStoreBS(b, base, depth, deltaRows, rows, c.bs)
	defer db.Close()
	if !scan {
		for i := uint64(1); i <= rows; i++ {
			if _, err := db.Get(context.Background(), head, 1, i, nil); err != nil {
				require.NoError(b, err)
			}
		}
	}
	b.ResetTimer()
	if scan {
		for i := 0; i < b.N; i++ {
			it, err := db.Scan(context.Background(), head, 1, ScanOptions{})
			require.NoError(b, err)
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
			require.Equal(b, int(rows+depth*deltaRows), n, "chain scan returned %d rows, want %d", n, rows+depth*deltaRows)
		}
		b.StopTimer()
		b.ReportMetric(float64(rows+depth*deltaRows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
	} else {
		var rng uint64 = 1442695040888963407
		for i := 0; i < b.N; i++ {
			rng = rng*6364136223846793005 + 1
			if _, err := db.Get(context.Background(), head, 1, rng%rows+1, nil); err != nil {
				require.NoError(b, err)
			}
		}
		b.StopTimer()
		b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
	}
	st := db.Stats()
	reportStatsMetrics(b, st, rssBefore)
	reportCacheMetrics(b, st)
}

// buildDeltaChainStoreBS builds a FULL (rows) + depth DELTAs of deltaRows with
// the given block size, returning the open store and chain head.
func buildDeltaChainStoreBS(b *testing.B, base string, depth, deltaRows, rows int, bs int) (*Store, SnapshotID) {
	b.Helper()
	db, fullID := buildBenchStoreOpts(b, base, uint64(rows), Options{BlockSize: bs})
	parent := fullID
	nextID := uint64(rows + 1)
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

func benchOpenReplay(b *testing.B, c benchCtx) {
	c.applyIO(b)
	base := filepath.Join(tmpdb(b), "op")
	db, _ := buildBenchStoreOpts(b, base, 100000, c.opts())
	db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db2, err := Open(base, Options{})
		require.NoError(b, err)
		db2.Close()
	}
}

func benchRebuildIndex(b *testing.B, c benchCtx) {
	c.applyIO(b)
	base := filepath.Join(tmpdb(b), "rb")
	db, _ := buildBenchStoreOpts(b, base, 100000, c.opts())
	db.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := db.Path() + ".rpi"
		_ = removeFile(idx)
		if err := RebuildIndex(context.Background(), db.Path(), RebuildOptions{Durability: AsyncCommit}); err != nil {
			require.NoError(b, err)
		}
	}
}

// ---- 矩阵入口 ----

// BenchmarkMainMatrix runs the pruned v1.2 benchmark matrix (see file doc).
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
		name = fmt.Sprintf("rebuild/bs=%s", bsLabel(bs))
		b.Run(name, func(b *testing.B) {
			benchRebuildIndex(b, benchCtx{bs: bs})
		})
	}
}

// ---- 延迟分位数 ----

// measureLatency runs fn n times, collecting per-op durations, and returns
// the p50/p95/p99 in nanoseconds.
func measureLatency(b *testing.B, n int, fn func() error) (p50, p95, p99 time.Duration) {
	b.Helper()
	durs := make([]time.Duration, n)
	di := 0
	for i := 0; i < n; i++ {
		start := time.Now()
		if err := fn(); err != nil {
			require.NoError(b, err)
		}
		durs[di] = time.Since(start)
		di++
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
		base := filepath.Join(tmpdb(b), "lh")
		db, fullID := buildBenchStoreOpts(b, base, 100000, Options{})
		defer db.Close()
		for i := uint64(0); i < 100; i++ {
			if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
				require.NoError(b, err)
			}
		}
		var dst Row
		var rng uint64 = 1
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			row, err := db.Get(context.Background(), fullID, 1, rng%100+1, dst)
			dst = row
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
	b.Run("get_hot_into", func(b *testing.B) {
		base := filepath.Join(tmpdb(b), "lhi")
		db, fullID := buildBenchStoreOpts(b, base, 100000, Options{})
		defer db.Close()
		for i := uint64(0); i < 100; i++ {
			if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
				require.NoError(b, err)
			}
		}
		var dst Row
		var rng uint64 = 2
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			row, err := db.Get(context.Background(), fullID, 1, rng%100+1, dst)
			dst = row
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
	b.Run("get_cold", func(b *testing.B) {
		base := filepath.Join(tmpdb(b), "lc")
		db, fullID := buildBenchStoreOpts(b, base, 100000, Options{CacheBytes: -1})
		defer db.Close()
		var rng uint64 = 3
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			_, err := db.Get(context.Background(), fullID, 1, rng%100000+1, nil)
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
	b.Run("get_deepchain", func(b *testing.B) {
		base := filepath.Join(tmpdb(b), "ldc")
		db, head := buildDeltaChainStoreBS(b, base, 32, 1000, 100000, 0)
		defer db.Close()
		for i := uint64(1); i <= 100000; i++ {
			if _, err := db.Get(context.Background(), head, 1, i, nil); err != nil {
				require.NoError(b, err)
			}
		}
		var rng uint64 = 4
		p50, p95, p99 := measureLatency(b, samples, func() error {
			rng = rng*6364136223846793005 + 1
			_, err := db.Get(context.Background(), head, 1, rng%100000+1, nil)
			return err
		})
		reportLatency(b, p50, p95, p99)
	})
}
