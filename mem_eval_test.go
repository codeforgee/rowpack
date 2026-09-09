package rowpack

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// heapMB returns current HeapAlloc in MiB after forcing GC.
func heapMB(t testing.TB) float64 {
	t.Helper()
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapAlloc) / (1 << 20)
}

// peakHeapMB samples HeapAlloc every 2ms around fn and reports the peak.
func peakHeapMB(t testing.TB, fn func()) float64 {
	t.Helper()
	var peak uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak {
					peak = ms.HeapAlloc
				}
			}
		}
	}()
	fn()
	close(stop)
	<-done
	return float64(peak) / (1 << 20)
}

// TestMemEvalWritePath measures the write path: peak heap during a 1M-row
// FULL snapshot insert and commit, plus writer-side accounting.
func TestMemEvalWritePath(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	const rows = 1_000_000
	base := tmpdb(t) + "/memwrite"

	db, err := Create(base, Options{})
	require.NoError(t, err)
	w, err := db.BeginFull(context.Background())
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("bench", benchSchema()))

	midInsert := 0.0
	peakWrite := peakHeapMB(t, func() {
		for i := uint64(0); i < rows; i++ {
			require.NoError(t, w.Insert(context.Background(), "bench", i+1, benchRow(i)))
		}
		midInsert = heapMB(t)
	})
	peakCommit := peakHeapMB(t, func() {
		_, err := w.Commit(context.Background())
		require.NoError(t, err)
	})
	afterCommit := heapMB(t)

	viewMem := db.state.Load().view.MemoryBytes()
	st := db.Stats()
	t.Logf("WRITE PATH (1M rows, 7 cols):")
	t.Logf("  peak heap during insert = %.1f MB", peakWrite)
	t.Logf("  heap at end of insert   = %.1f MB", midInsert)
	t.Logf("  peak heap during commit = %.1f MB", peakCommit)
	t.Logf("  heap after commit       = %.1f MB", afterCommit)
	t.Logf("  view.MemoryBytes()      = %.1f MB", float64(viewMem)/(1<<20))
	t.Logf("  datafile=%.1f MB raw=%.1f MB stored=%.1f MB",
		float64(st.DataFileBytes)/(1<<20), float64(st.RawBytes)/(1<<20), float64(st.StoredBytes)/(1<<20))

	totalPendingPayload, totalPendingRows := 0, 0
	for _, p := range w.pending {
		totalPendingPayload += len(p.payload)
		totalPendingRows += len(p.rowsDir)
	}
	seenTotal := 0
	for _, s := range w.seenRows {
		seenTotal += s.Len()
	}
	t.Logf("  writer seenRows entries = %d (%d slots packed)", seenTotal, func() int {
		n := 0
		for _, s := range w.seenRows {
			n += len(s.slots)
		}
		return n
	}())
	t.Logf("  writer pending blocks   = %d, payload %.1f MB, row entries %d (~%.1f MB)",
		len(w.pending), float64(totalPendingPayload)/(1<<20), totalPendingRows,
		float64(totalPendingRows*40)/(1<<20))
	db.Close()
	t.Logf("  heap after close        = %.1f MB", heapMB(t))
}

// TestMemEvalOpenPath measures resident index memory after opening a store
// with 1 FULL (100k rows) + 200 DELTAs: view + schemaIndex behavior.
func TestMemEvalOpenPath(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	base := tmpdb(t) + "/memopen"
	db, err := Create(base, Options{})
	require.NoError(t, err)
	ctx := context.Background()

	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("bench", benchSchema()))
	for i := uint64(0); i < 100_000; i++ {
		require.NoError(t, w.Insert(ctx, "bench", i+1, benchRow(i)))
	}
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	parent := full
	for d := 0; d < 200; d++ {
		w, err := db.BeginDelta(ctx, parent)
		require.NoError(t, err)
		for i := uint64(0); i < 100; i++ {
			require.NoError(t, w.Insert(ctx, "bench", 1_000_000+uint64(d)*100+i, benchRow(i)))
		}
		si, err := w.Commit(ctx)
		require.NoError(t, err)
		parent = si
	}
	db.Close()

	before := heapMB(t)
	db2, err := Open(base, Options{})
	require.NoError(t, err)
	after := heapMB(t)
	st := db2.Stats()
	view := db2.state.Load().view
	si := db2.state.Load().schemas
	pairs := 0
	for _, ts := range si.bySnapshot {
		pairs += len(ts)
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("OPEN PATH (1 FULL 100k + 200 DELTA x100):")
	t.Logf("  heap before open        = %.1f MB", before)
	t.Logf("  heap after open         = %.1f MB", after)
	t.Logf("  view.MemoryBytes()      = %.1f MB", float64(view.MemoryBytes())/(1<<20))
	t.Logf("  stats.IndexMemoryBytes  = %.1f MB", float64(st.IndexMemoryBytes)/(1<<20))
	t.Logf("  schemaIndex snapshots=%d (table,schema) pairs=%d (alive Schema objects ~%d)",
		len(si.bySnapshot), pairs, pairs)
	t.Logf("  HeapObjects=%d HeapSys=%.1f MB", ms.HeapObjects, float64(ms.HeapSys)/(1<<20))
	db2.Close()
}

// TestMemEvalScanAllocs measures per-row allocation behavior of a full scan.
func TestMemEvalScanAllocs(t *testing.T) {
	base := tmpdb(t) + "/memscan"
	db, fullID := buildMemStore(t, base, 200_000)
	defer db.Close()

	// Warm-up pass (pool priming, cache).
	it, err := db.Scan(context.Background(), fullID, "bench", ScanOptions{})
	require.NoError(t, err)
	for {
		if _, ok := it.Next(); !ok {
			break
		}
	}
	it.Close()

	runtime.GC()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	it, err = db.Scan(context.Background(), fullID, "bench", ScanOptions{})
	require.NoError(t, err)
	n := 0
	for {
		if _, ok := it.Next(); !ok {
			break
		}
		n++
	}
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	it.Close()
	t.Logf("SCAN 200k rows: allocs=%d (%.2f allocs/row), bytes=%.1f MB (%.0f B/row)",
		ms1.Mallocs-ms0.Mallocs, float64(ms1.Mallocs-ms0.Mallocs)/float64(n),
		float64(ms1.TotalAlloc-ms0.TotalAlloc)/(1<<20),
		float64(ms1.TotalAlloc-ms0.TotalAlloc)/float64(n))
}

// TestMemEvalBatchAllocs measures ReadRowsByIDs allocation behavior.
func TestMemEvalBatchAllocs(t *testing.T) {
	base := tmpdb(t) + "/membatch"
	db, fullID := buildMemStore(t, base, 200_000)
	defer db.Close()

	ids := make([]RowID, 0, 50_000)
	for i := uint64(1); i <= 50_000; i++ {
		ids = append(ids, i*3+1)
	}
	runOnce := func() {
		it, err := db.ReadRowsByIDs(context.Background(), fullID, 1, ids, BatchReadOptions{})
		require.NoError(t, err)
		for {
			if _, _, ok := it.Next(nil); !ok {
				break
			}
		}
		it.Close()
	}
	runOnce()
	runtime.GC()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	runOnce()
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	t.Logf("BATCH 50k ids: allocs=%d (%.2f allocs/row), bytes=%.1f MB (%.0f B/row)",
		ms1.Mallocs-ms0.Mallocs, float64(ms1.Mallocs-ms0.Mallocs)/float64(len(ids)),
		float64(ms1.TotalAlloc-ms0.TotalAlloc)/(1<<20),
		float64(ms1.TotalAlloc-ms0.TotalAlloc)/float64(len(ids)))
}

// TestMemEvalManySnapsOpen quantifies open-time cost growth with snapshot
// count (view shallowCopy + schemaIndex re-derivation).
func TestMemEvalManySnapsOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	for _, n := range []int{50, 200, 800} {
		base := tmpdb(t) + fmt.Sprintf("/many%d", n)
		db, err := Create(base, Options{})
		require.NoError(t, err)
		ctx := context.Background()
		w, _ := db.BeginFull(ctx)
		require.NoError(t, w.CreateTable("bench", benchSchema()))
		for i := uint64(0); i < 20_000; i++ {
			require.NoError(t, w.Insert(ctx, "bench", i+1, benchRow(i)))
		}
		full, err := w.Commit(ctx)
		require.NoError(t, err)
		parent := full
		for d := 0; d < n; d++ {
			w, err := db.BeginDelta(ctx, parent)
			require.NoError(t, err)
			for i := uint64(0); i < 20; i++ {
				require.NoError(t, w.Insert(ctx, "bench", 100_000+uint64(d)*20+i, benchRow(i)))
			}
			si, err := w.Commit(ctx)
			require.NoError(t, err)
			parent = si
		}
		db.Close()

		db2, err := Open(base, Options{})
		require.NoError(t, err)
		h := heapMB(t)
		st := db2.Stats()
		t.Logf("MANY-SNAP OPEN n=%d: heap after open=%.1f MB, IndexMemoryBytes=%.1f MB",
			n, h, float64(st.IndexMemoryBytes)/(1<<20))
		db2.Close()
	}
}

// buildMemStore writes nRows into one FULL snapshot (testing.TB helper).
func buildMemStore(t testing.TB, base string, nRows uint64) (*Store, SnapshotID) {
	t.Helper()
	db, err := Create(base, Options{})
	require.NoError(t, err)
	w, err := db.BeginFull(context.Background())
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("bench", benchSchema()))
	for i := uint64(0); i < nRows; i++ {
		require.NoError(t, w.Insert(context.Background(), "bench", i+1, benchRow(i)))
	}
	full, err := w.Commit(context.Background())
	require.NoError(t, err)
	return db, full
}
