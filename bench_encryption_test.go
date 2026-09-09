package rowpack

// v1.3 加密基准（S8）：加密 vs 未加密在关键读写路径上的同参对拍，作为
// 加密引入成本的可复现基线（ENCRYPTION_DEVELOPMENT_PLAN.md S8）。
//
//	go test -run '^$' -bench 'BenchmarkEncryption' -benchmem -count=1 [-benchtime=2s]
//
// 子测试命名即维度：get_hot|get_cold|scan|batch|write × plain|enc。
// get/scan/batch 场景为 100k 行 × 7 列 × 256KiB Block × mmap；
// batch 为 4096 行/批（顺序与随机两个 ids 集）。
// 验收门槛：冷路径（get_cold/scan/batch）加密 vs 未加密回退 ≤ 10%；
// 热路径（get_hot）不回退（缓存命中不触发解密块负载）。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// encBenchProvider returns a fixed key for benchmarks.
func encBenchProvider() *staticKeyProvider {
	return &staticKeyProvider{keyID: "bench", key: testKey("bench")}
}

func encBenchOpts(on bool, cold bool) Options {
	o := Options{}
	if on {
		o.Encryption = &EncryptionConfig{KeyProvider: encBenchProvider(), KeyID: "bench"}
	}
	if cold {
		o.CacheBytes = -1
	}
	return o
}

func benchEncGet(b *testing.B, enc, cold bool) {
	const rows = 100000
	db, fullID := buildBenchStoreOpts(b, filepath.Join(tmpdb(b), "eg"), rows, encBenchOpts(enc, cold))
	defer db.Close()
	var dst Row
	warm := 100
	if cold {
		warm = 10
	}
	for i := uint64(1); i <= uint64(warm); i++ {
		if _, err := db.Get(context.Background(), fullID, "bench", i, dst); err != nil {
			require.NoError(b, err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var rowID uint64
		if cold {
			rowID = uint64(i%rows) + 1
		} else {
			rowID = uint64(i%100) + 1
		}
		row, err := db.Get(context.Background(), fullID, "bench", rowID, dst)
		if err != nil {
			require.NoError(b, err)
		}
		dst = row
	}
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds()/1000, "kget/s")
}

func benchEncScan(b *testing.B, enc bool) {
	const rows = 100000
	db, fullID := buildBenchStoreOpts(b, filepath.Join(tmpdb(b), "es"), rows, encBenchOpts(enc, false))
	defer db.Close()
	for i := 0; i < 2; i++ { // burn-in
		it, err := db.Scan(context.Background(), fullID, "bench", ScanOptions{})
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		it, err := db.Scan(context.Background(), fullID, "bench", ScanOptions{})
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
		require.Equal(b, int(rows), n, "scan returned %d rows, want %d", n, rows)
	}
	b.StopTimer()
	b.ReportMetric(float64(rows)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
}

func benchEncBatch(b *testing.B, enc bool) {
	const rows = 100000
	const batch = 4096
	db, fullID := buildBenchStoreOpts(b, filepath.Join(tmpdb(b), "eb"), rows, encBenchOpts(enc, false))
	defer db.Close()
	// Sequential ids (3 blocks) and random ids (many blocks).
	seq := make([]RowID, batch)
	rnd := make([]RowID, batch)
	var rng uint64 = 88172645463325252
	for i := range seq {
		seq[i] = RowID(i + 1)
		rng = rng*6364136223846793005 + 1442695040888963407
		rnd[i] = RowID(rng%rows) + 1
	}
	ctx := context.Background()
	// Warm once.
	if _, err := db.ReadBatch(ctx, fullID, 1, seq); err != nil {
		require.NoError(b, err)
	}
	if _, err := db.ReadBatch(ctx, fullID, 1, rnd); err != nil {
		require.NoError(b, err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ids := seq
		if i%2 == 1 {
			ids = rnd
		}
		if _, err := db.ReadBatch(ctx, fullID, 1, ids); err != nil {
			require.NoError(b, err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(batch)*float64(b.N)/b.Elapsed().Seconds()/1000, "krows/s")
}

func benchEncWrite(b *testing.B, enc bool) {
	const rows = 100000
	rssBefore := peakRSSBytes()
	var lastStats Stats
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		base := filepath.Join(tmpdb(b), "ew")
		db, err := Create(base, encBenchOpts(enc, false))
		require.NoError(b, err)
		w, _ := db.BeginFull(context.Background())
		require.NoError(b, w.CreateTable("bench", benchSchema()))
		b.StartTimer()
		for j := uint64(0); j < rows; j++ {
			if err := w.Insert(context.Background(), "bench", j+1, benchRow(j)); err != nil {
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

// BenchmarkEncryption is the encryption cost baseline entry point.
func BenchmarkEncryption(b *testing.B) {
	for _, enc := range []struct {
		name string
		on   bool
	}{
		{"plain", false},
		{"enc", true},
	} {
		enc := enc
		for _, cold := range []struct {
			name string
			v    bool
		}{{"hot", false}, {"cold", true}} {
			name := "get_" + cold.name + "/" + enc.name
			b.Run(name, func(b *testing.B) {
				benchEncGet(b, enc.on, cold.v)
			})
		}
		b.Run("scan/"+enc.name, func(b *testing.B) {
			benchEncScan(b, enc.on)
		})
		b.Run("batch/"+enc.name, func(b *testing.B) {
			benchEncBatch(b, enc.on)
		})
		b.Run("write/"+enc.name, func(b *testing.B) {
			benchEncWrite(b, enc.on)
		})
	}
}
