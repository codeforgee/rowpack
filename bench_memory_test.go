package rowpack

import (
	"path/filepath"
	"runtime"
	"testing"
)

// Open 内存基准（S0 冻结，FILE_FORMAT_REFACTOR_PLAN.md §3.1 验收口径）：
//
//	idxMB     Eager Row Index 常驻内存（目标 ≤16 B/row，期望 ~12）
//	idxB/row  同上的每行口径，重构主要 KPI
//	rssdMB    Open 期间的进程峰值 RSS 增量（getrusage 高水位，近似）
//
// 10M 行档由 ROWPACK_BENCH_ROWS10M 门控（构建约 10–20 s，默认跳过）；
// 1M 行档随 `make bench` 常规运行。数据集为 geomMixed 顺序 RowID。

// BenchmarkOpenMemory measures index-resident memory and Open peak RSS on a
// 1M-row store (ROWPACK_BENCH_ROWS1M overrides).
func BenchmarkOpenMemory(b *testing.B) {
	openMemoryBench(b, benchRows1M)
}

// BenchmarkOpenMemory10M is the 10M-row tier of BenchmarkOpenMemory; it is
// skipped unless ROWPACK_BENCH_ROWS10M is set (e.g. 10000000).
func BenchmarkOpenMemory10M(b *testing.B) {
	n := envInt("ROWPACK_BENCH_ROWS10M", 0)
	if n == 0 {
		b.Skip("set ROWPACK_BENCH_ROWS10M to enable (e.g. ROWPACK_BENCH_ROWS10M=10000000)")
	}
	openMemoryBench(b, n)
}

func openMemoryBench(b *testing.B, n int) {
	base := filepath.Join(tmpdb(b), "openmem")
	db, _ := benchStoreAt(b, base, Options{}, n)
	requireNilErr(b, db.Close())
	runtime.GC()
	rssBefore := peakRSSBytes()

	b.ReportAllocs()
	b.ResetTimer()
	var st Stats
	for i := 0; i < b.N; i++ {
		d, err := Open(base, Options{})
		if err != nil {
			b.Fatal(err)
		}
		st = d.Stats()
		if err := d.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()

	if st.IndexMemoryBytes == 0 {
		b.Fatal("no index memory reported")
	}
	b.ReportMetric(float64(st.IndexMemoryBytes)/(1<<20), "idxMB")
	b.ReportMetric(float64(st.IndexMemoryBytes)/float64(n), "idxB/row")
	if rss := peakRSSBytes(); rssBefore >= 0 && rss > rssBefore {
		b.ReportMetric(float64(rss-rssBefore)/(1<<20), "rssdMB")
	}
}

// BenchmarkOpenMemoryLazy measures the S4 Lazy index Open: the resident row-index
// footprint is the Row Index Fence Directory only (fenceB/row ≈ 52 B/page ÷ 4096
// ≈ 0.013 B/row for the geomMixed single-table dataset); no row page is decoded
// at Open. The default ROWPACK_BENCH_ROWS1M override matches BenchmarkOpenMemory.
func BenchmarkOpenMemoryLazy(b *testing.B) {
	n := benchRows1M
	base := filepath.Join(tmpdb(b), "openmem-lazy")
	db, _ := benchStoreAt(b, base, Options{}, n)
	requireNilErr(b, db.Close())
	runtime.GC()

	// Report the Lazy Open RESIDENT index footprint (the fence directory).
	// Stats() is deliberately not used: it runs LogicalRowCount, which for a
	// Lazy view decodes every index page (measuring the read path, not Open).
	b.ReportAllocs()
	b.ResetTimer()
	var fenceBytes, idx uint64
	for i := 0; i < b.N; i++ {
		d, err := Open(base, Options{IndexMode: IndexLazy, IndexCacheBytes: 1 << 20})
		if err != nil {
			b.Fatal(err)
		}
		if st := d.state.Load(); st != nil && st.view != nil {
			fenceBytes = uint64(st.view.IndexFenceBytes())
			idx = uint64(st.view.MemoryBytes())
		}
		if err := d.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()

	if fenceBytes == 0 {
		b.Fatal("no fence bytes reported for lazy open")
	}
	b.ReportMetric(float64(fenceBytes)/(1<<20), "fenceMB")
	b.ReportMetric(float64(fenceBytes)/float64(n), "fenceB/row")
	b.ReportMetric(float64(idx)/(1<<20), "idxMB")
}
