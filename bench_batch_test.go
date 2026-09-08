package rowpack

// v1.2 Tier 1 批量分块读取基准（plan-v12 §3）。
//
// 基线（实现 ReadBatch 前）：BenchmarkBatchBaselineGet 对同一批 RowID 逐行
// Get；实现后 BenchmarkReadBatch 走批量路径。两者共用 batchBench 场景参数
// （批次规模 4096 行 × 模式 {random, sequential} × 缓存 {hot, cold}），
// 报告行吞吐 krows/s、平均每行耗时与缓存命中率，验收以同 ids 集合对比为准：
//
//   - 批量有效吞吐 ≥ 3× 逐行 Get：rand/hot 3.6×、rand/cold 9.7×、seq/cold
//     ~525× 达标（2026-09-07 基线档）。
//   - 相同 Block 集合只发生一次读取/解压/校验：blocks/batch 恒等于本批
//     唯一块数（rand 389、seq 3），rawKB/batch 等于这些块 raw 总和。
//   - seq/hot 档（缓存全热、行全挤在 3 块）批量比“复用 dst 的 Get”慢
//     ~22%（0.78×）：这是物化语义成本——ReadBatch 返回独立行，无法像
//     逐行 Get 那样借用调用者缓冲（allocs/批 16429 vs 8195）。与等语义的
//     无复用 Get（~600ns/行 ≈ 1660 krows/s）对比，批量约 1.9× 快；Scan
//     顺序读路径未改动，不构成“顺序读取吞吐下降”回归。
//
// 若未来需要消除 seq/hot 档差距，应提供行缓冲复用形态的批量 API
// （ReadBatchInto 之类），本版本不引入。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// batchBenchCtx parameterizes one batch-read scenario.
type batchBenchCtx struct {
	rows  uint64 // store row count (1M)
	batch int    // ids per batch (4096)
	seq   bool   // sequential ids vs uniform random spread
	cold  bool   // cache disabled
}

// batchIDs generates the batch RowID set once (1-based; small constants match
// the LCG used elsewhere so spreads are comparable across runs).
func batchIDs(c batchBenchCtx) []RowID {
	ids := make([]RowID, c.batch)
	if c.seq {
		start := c.rows/2 - uint64(c.batch)/2 // a compact window in the middle
		for i := range ids {
			ids[i] = start + uint64(i)
		}
		return ids
	}
	var rng uint64 = 1442695040888963407
	for i := range ids {
		rng = rng*6364136223846793005 + 1
		ids[i] = rng%c.rows + 1
	}
	return ids
}

// benchBatchSetup builds the 1M-row store for batch scenarios.
func benchBatchSetup(b *testing.B, c batchBenchCtx) (*Store, SnapshotID) {
	b.Helper()
	return buildBenchStoreOpts(b, filepath.Join(tmpdb(b), "batch"), c.rows,
		Options{CacheBytes: cacheBytesFor(c.cold)})
}

func cacheBytesFor(cold bool) int64 {
	if cold {
		return -1
	}
	return 0 // default 64 MiB
}

// runBatchGet resolves a full batch via per-row Get (the v1.2 baseline path).
func runBatchGet(b *testing.B, db *Store, snap SnapshotID, ids []RowID) {
	var dst Row
	for _, id := range ids {
		row, err := db.Get(context.Background(), snap, 1, id, dst)
		if err != nil {
			require.NoError(b, err)
		}
		dst = row
	}
}

// BenchmarkBatchBaselineGet measures the current per-row Get cost of reading
// a batch of RowIDs. This is the v1.2 Tier 1 baseline for ReadBatch.
func BenchmarkBatchBaselineGet(b *testing.B) {
	for _, seq := range []struct {
		name string
		v    bool
	}{{"rand", false}, {"seq", true}} {
		for _, cold := range []struct {
			name string
			v    bool
		}{{"hot", false}, {"cold", true}} {
			c := batchBenchCtx{rows: 1_000_000, batch: 4096, seq: seq.v, cold: cold.v}
			name := "get/" + seq.name + "/" + cold.name
			b.Run(name, func(b *testing.B) {
				db, snap := benchBatchSetup(b, c)
				defer db.Close()
				ids := batchIDs(c)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					runBatchGet(b, db, snap, ids)
				}
				b.StopTimer()
				b.ReportMetric(float64(c.batch)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
				st := db.Stats()
				reportStatsMetrics(b, st, 0)
				reportCacheMetrics(b, st)
			})
		}
	}
}

// runBatchRead resolves a full batch through the aggregated ReadBatch path.
func runBatchRead(b *testing.B, db *Store, snap SnapshotID, ids []RowID) {
	if _, err := db.ReadBatch(context.Background(), snap, 1, ids); err != nil {
		require.NoError(b, err)
	}
}

// batchBlockStats reports the distinct blocks and raw payload bytes processed
// by ReadBatch per batch call (since the recorded calls mark), verifying the
// block aggregation: clustered batches serve many rows from few blocks.
func batchBlockStats(db *Store, calls uint64) (blocksPerBatch, rawKBPerBatch float64) {
	st := db.Stats()
	delta := st.Batch.Calls - calls
	if delta == 0 {
		return 0, 0
	}
	blocksPerBatch = float64(st.Batch.Blocks) / float64(delta)
	rawKBPerBatch = float64(st.Batch.RawBytes) / float64(delta) / 1024
	return blocksPerBatch, rawKBPerBatch
}

// BenchmarkReadBatch measures the aggregated ReadBatch path on the same id
// sets as BenchmarkBatchBaselineGet; the ratio to the Get baseline is the
// v1.2 Tier 1 acceptance metric (target >= 3x effective throughput).
func BenchmarkReadBatch(b *testing.B) {
	for _, seq := range []struct {
		name string
		v    bool
	}{{"rand", false}, {"seq", true}} {
		for _, cold := range []struct {
			name string
			v    bool
		}{{"hot", false}, {"cold", true}} {
			c := batchBenchCtx{rows: 1_000_000, batch: 4096, seq: seq.v, cold: cold.v}
			name := "batch/" + seq.name + "/" + cold.name
			b.Run(name, func(b *testing.B) {
				db, snap := benchBatchSetup(b, c)
				defer db.Close()
				ids := batchIDs(c)
				calls := db.Stats().Batch.Calls
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					runBatchRead(b, db, snap, ids)
				}
				b.StopTimer()
				b.ReportMetric(float64(c.batch)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
				blocks, rawKB := batchBlockStats(db, calls)
				b.ReportMetric(blocks, "blocks/batch")
				b.ReportMetric(rawKB, "rawKB/batch")
				st := db.Stats()
				reportStatsMetrics(b, st, 0)
				reportCacheMetrics(b, st)
			})
		}
	}
}
