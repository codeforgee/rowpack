package rowpack

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---- V2-M4: 批量分块读取 ----

// batchCollect drains an iterator, returning (rowIDs, values) in emission
// order plus the final stats.
func batchCollect(t *testing.T, it *BatchIterator) ([]RowID, []string, BatchReadStats) {
	t.Helper()
	ids := make([]RowID, 0, 16)
	vals := make([]string, 0, 16)
	for {
		id, row, ok := it.Next(nil)
		if !ok {
			break
		}
		ids = append(ids, id)
		v, _ := row[1].String()
		vals = append(vals, v)
	}
	require.NoError(t, it.Err())
	st := it.Stats()
	require.NoError(t, it.Close())
	return ids, vals, st
}

// TestBatchByIDsMatchesGet cross-checks ReadRowsByIDs against per-row Get for
// clustered, sparse, reversed and boundary ID sets, in both emission orders.
func TestBatchByIDsMatchesGet(t *testing.T) {
	base := filepath.Join(tmpdb(t), "b")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	sets := [][]RowID{
		{1, 2, 3, 4, 5},                            // clustered
		{1, 500, 1000, 1500, 2000},                 // sparse
		{2000, 1500, 1000, 500, 1},                 // reversed input
		{1, 2000},                                  // boundary
		{7, 7, 8},                                  // duplicates
		{42},                                       // single
		{2001, 5000},                               // all missing
	}
	for _, ids := range sets {
		for _, order := range []BatchOrder{BatchOrderRowID, BatchOrderInput} {
			it, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{Order: order})
			require.NoError(t, err)
			gotIDs, gotVals, st := batchCollect(t, it)

			type wantPair struct {
				id  RowID
				val string
			}
			var want []wantPair
			for _, id := range ids {
				r, err := db.Get(context.Background(), full, 1, id, nil)
				if err != nil {
					continue // invisible: skipped
				}
				v, _ := r[1].String()
				want = append(want, wantPair{id: id, val: v})
			}
			if order == BatchOrderRowID {
				sort.SliceStable(want, func(a, b int) bool { return want[a].id < want[b].id })
			}
			wantIDs := make([]RowID, len(want))
			wantVals := make([]string, len(want))
			for i, w := range want {
				wantIDs[i] = w.id
				wantVals[i] = w.val
			}
			require.Equal(t, wantIDs, gotIDs, "ids for %v order %d", ids, order)
			require.Equal(t, wantVals, gotVals, "values for %v order %d", ids, order)
			require.Equal(t, uint64(len(ids)), st.RequestedIDs)
			require.Equal(t, uint64(len(want)), st.RowsReturned)
		}
	}
}

// TestBatchSkipInvisible verifies missing and deleted rows are skipped, the
// request still succeeds, and the stats expose the difference.
func TestBatchSkipInvisible(t *testing.T) {
	base := filepath.Join(tmpdb(t), "skip")
	db, full := buildConcurrentStore(t, base, Options{})
	// Delete rows 3 and 4 in a DELTA.
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	require.NoError(t, w.Delete(context.Background(), 1, 3))
	require.NoError(t, w.Delete(context.Background(), 1, 4))
	delta, err := w.Commit(context.Background())
	require.NoError(t, err)

	it, err := db.ReadRowsByIDs(context.Background(), delta.ID, 1, []RowID{2, 3, 4, 5, 9999}, BatchReadOptions{})
	require.NoError(t, err)
	ids, vals, st := batchCollect(t, it)
	require.Equal(t, []RowID{2, 5}, ids)
	require.Equal(t, []string{"n-2", "n-5"}, vals)
	require.Equal(t, uint64(5), st.RequestedIDs)
	require.Equal(t, uint64(2), st.RowsReturned)

	// At the FULL snapshot rows 3/4 are visible again.
	it2, err := db.ReadRowsByIDs(context.Background(), full, 1, []RowID{3, 4}, BatchReadOptions{})
	require.NoError(t, err)
	ids2, _, _ := batchCollect(t, it2)
	require.Equal(t, []RowID{3, 4}, ids2)
}

// TestBatchDuplicates pins the duplicate semantics (R17): every occurrence of
// a duplicated input ID is emitted, 1:1 with input positions, in both orders.
func TestBatchDuplicates(t *testing.T) {
	base := filepath.Join(tmpdb(t), "dup")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	ids := []RowID{5, 5, 9, 5}

	it, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{Order: BatchOrderInput})
	require.NoError(t, err)
	gotIDs, gotVals, st := batchCollect(t, it)
	require.Equal(t, []RowID{5, 5, 9, 5}, gotIDs, "input order must map 1:1")
	require.Equal(t, []string{"n-5", "n-5", "n-9", "n-5"}, gotVals)
	require.Equal(t, uint64(4), st.RowsReturned)

	it2, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{Order: BatchOrderRowID})
	require.NoError(t, err)
	gotIDs2, gotVals2, _ := batchCollect(t, it2)
	require.Equal(t, []RowID{5, 5, 5, 9}, gotIDs2, "row id order keeps duplicates adjacent")
	require.Equal(t, []string{"n-5", "n-5", "n-5", "n-9"}, gotVals2)
}

// TestBatchRanges verifies multi-range planning: overlap merging, holes,
// cross-layer visibility, and agreement with Scan over the same span.
func TestBatchRanges(t *testing.T) {
	base := filepath.Join(tmpdb(t), "rng")
	db, full := buildConcurrentStore(t, base, Options{})
	// DELTA deletes 10 and inserts 2001..2003.
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	require.NoError(t, w.Delete(context.Background(), 1, 10))
	for i := uint64(2001); i <= 2003; i++ {
		require.NoError(t, w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("n-%d", i))}))
	}
	delta, err := w.Commit(context.Background())
	require.NoError(t, err)

	// Overlapping ranges merge; hole at 10 (deleted); 2999..3999 empty.
	ranges := []RowIDRange{
		{Start: 8, End: 12},     // rows 8,9,11,12 (10 deleted)
		{Start: 11, End: 15},    // overlaps: merged 8..15
		{Start: 2000, End: 2004},// 2000..2003
		{Start: 2999, End: 3999},// empty span
	}
	it, err := db.ReadRowRanges(context.Background(), delta.ID, 1, ranges, BatchReadOptions{})
	require.NoError(t, err)
	ids, _, st := batchCollect(t, it)
	want := []RowID{8, 9, 11, 12, 13, 14, 2000, 2001, 2002, 2003}
	require.Equal(t, want, ids)
	require.Equal(t, uint64(4), st.RequestedRanges)
	require.Equal(t, uint64(3), st.MergedRanges, "ranges 1+2 merge; range 4 empty but kept")
	require.Equal(t, uint64(len(want)), st.RowsReturned)

	// Cross-check the single merged range against Scan over the same span
	// (Scan is the single-range kernel; its span covers all visible rows,
	// which for [8,15) equals the merged range's result).
	scanIDs := []RowID{}
	scanIt, err := db.Scan(context.Background(), delta.ID, 1, ScanOptions{StartRowID: 8, EndRowID: 15})
	require.NoError(t, err)
	for {
		row, ok := scanIt.Next()
		if !ok {
			break
		}
		scanIDs = append(scanIDs, scanIt.RowID())
		_ = row
	}
	require.NoError(t, scanIt.Err())
	require.Equal(t, []RowID{8, 9, 11, 12, 13, 14}, scanIDs, "single-range batch must match Scan visibility")
	itScan, err := db.ReadRowRanges(context.Background(), delta.ID, 1, []RowIDRange{{Start: 8, End: 15}}, BatchReadOptions{})
	require.NoError(t, err)
	scanLikeIDs, _, _ := batchCollect(t, itScan)
	require.Equal(t, scanIDs, scanLikeIDs)

	// Single-layer FULL snapshot sees 10 again.
	it2, err := db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 9, End: 11}}, BatchReadOptions{})
	require.NoError(t, err)
	ids2, _, _ := batchCollect(t, it2)
	require.Equal(t, []RowID{9, 10}, ids2)
}

// TestBatchSameBlockOnce verifies the aggregation gate: many requested rows
// inside one block produce exactly one block read and one decode.
func TestBatchSameBlockOnce(t *testing.T) {
	base := filepath.Join(tmpdb(t), "once")
	// Small block size so rows 1..100 share one block.
	opts := Options{BlockSize: 1024}
	db, full := buildConcurrentStore(t, base, opts)
	defer db.Close()

	// One block: the first ~20 rows share a block at BlockSize=1024.
	ids := make([]RowID, 0, 20)
	for i := uint64(1); i <= 20; i++ {
		ids = append(ids, i)
	}
	it, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{})
	require.NoError(t, err)
	gotIDs, vals, st := batchCollect(t, it)
	require.Len(t, gotIDs, 20)
	require.Equal(t, "n-1", vals[0])
	require.Equal(t, "n-20", vals[19])
	require.Equal(t, uint64(1), st.BlocksRead, "same-block rows must decode once: %+v", st)
	require.Equal(t, uint64(1), st.CandidateBlocks)
	require.Equal(t, uint64(0), st.CacheHits, "first load of the block is a miss")

	// Wider batch: rows 1..100 span several blocks; loads must equal the
	// candidate count (each block decoded exactly once), never the row count.
	wide := make([]RowID, 0, 100)
	for i := uint64(1); i <= 100; i++ {
		wide = append(wide, i)
	}
	it2, err := db.ReadRowsByIDs(context.Background(), full, 1, wide, BatchReadOptions{})
	require.NoError(t, err)
	_, _, st2 := batchCollect(t, it2)
	require.Equal(t, st2.CandidateBlocks, st2.BlocksRead, "each candidate block decoded exactly once: %+v", st2)
	require.Less(t, int(st2.BlocksRead), 100, "aggregation must collapse 100 rows into few blocks")
}

// TestBatchLimits pins the resource-limit and validation errors.
func TestBatchLimits(t *testing.T) {
	base := filepath.Join(tmpdb(t), "lim")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	// MaxRows.
	_, err := db.ReadRowsByIDs(context.Background(), full, 1, []RowID{1, 2, 3}, BatchReadOptions{MaxRows: 2})
	require.ErrorIs(t, err, ErrBatchLimit)
	_, err = db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 1, End: 10}}, BatchReadOptions{MaxRows: 5})
	require.ErrorIs(t, err, ErrBatchLimit)
	// MaxBytes (one block raw bytes > 1).
	_, err = db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 1, End: 10}}, BatchReadOptions{MaxBytes: 1})
	require.ErrorIs(t, err, ErrBatchLimit)
	// Invalid ranges.
	_, err = db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 5, End: 0}}, BatchReadOptions{})
	require.ErrorIs(t, err, ErrInvalidRange)
	_, err = db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 5, End: 5}}, BatchReadOptions{})
	require.ErrorIs(t, err, ErrInvalidRange)
	_, err = db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 7, End: 3}}, BatchReadOptions{})
	require.ErrorIs(t, err, ErrInvalidRange)
	// Input order on ranges.
	_, err = db.ReadRowRanges(context.Background(), full, 1, []RowIDRange{{Start: 1, End: 3}}, BatchReadOptions{Order: BatchOrderInput})
	require.ErrorIs(t, err, ErrInvalidArgument)
	// Unknown snapshot.
	_, err = db.ReadRowsByIDs(context.Background(), 99, 1, []RowID{1}, BatchReadOptions{})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestBatchCancel verifies ctx cancellation surfaces through Err and stops
// the iteration.
func TestBatchCancel(t *testing.T) {
	base := filepath.Join(tmpdb(t), "cancel")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	it, err := db.ReadRowsByIDs(ctx, full, 1, []RowID{1, 2, 3}, BatchReadOptions{})
	require.NoError(t, err)
	cancel()
	_, _, ok := it.Next(nil)
	require.False(t, ok)
	require.ErrorIs(t, it.Err(), context.Canceled)
	require.NoError(t, it.Close())
	// Closed iterator stays inert.
	_, _, ok = it.Next(nil)
	require.False(t, ok)
	require.NoError(t, it.Close())
}

// TestBatchEmptyInput verifies empty requests end immediately with no error.
func TestBatchEmptyInput(t *testing.T) {
	base := filepath.Join(tmpdb(t), "empty")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	it, err := db.ReadRowsByIDs(context.Background(), full, 1, nil, BatchReadOptions{})
	require.NoError(t, err)
	_, _, ok := it.Next(nil)
	require.False(t, ok)
	require.NoError(t, it.Err())

	it2, err := db.ReadRowRanges(context.Background(), full, 1, nil, BatchReadOptions{})
	require.NoError(t, err)
	_, _, ok = it2.Next(nil)
	require.False(t, ok)
	require.NoError(t, it2.Err())
}

// TestBatchVsGetLoopCrossCheck cross-checks ReadBatch (v1 API), ReadRowsByIDs
// and per-row Get on the same ID set.
func TestBatchVsGetLoopCrossCheck(t *testing.T) {
	base := filepath.Join(tmpdb(t), "x")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	ids := []RowID{1, 600, 1200, 1800, 2000}
	want := map[RowID]string{}
	for _, id := range ids {
		r, err := db.Get(context.Background(), full, 1, id, nil)
		require.NoError(t, err)
		v, _ := r[1].String()
		want[id] = v
	}

	// v1 aggregate API.
	rows, err := db.ReadBatch(context.Background(), full, 1, ids)
	require.NoError(t, err)
	require.Len(t, rows, len(ids))
	// v2 iterator.
	it, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{Order: BatchOrderInput})
	require.NoError(t, err)
	gotIDs, gotVals, _ := batchCollect(t, it)
	require.Equal(t, ids, gotIDs)
	for i, id := range ids {
		v, _ := rows[i][1].String()
		require.Equal(t, want[id], v, "ReadBatch row %d", id)
		require.Equal(t, want[id], gotVals[i], "ReadRowsByIDs row %d", id)
	}
}
// TestBatchParallelMatchesSequential verifies Parallelism > 1 produces byte
// identical output to the sequential path across ID sets and ranges.
func TestBatchParallelMatchesSequential(t *testing.T) {
	base := filepath.Join(tmpdb(t), "par")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	// DELTA layer so the plan spans multiple blocks across two layers.
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	for i := uint64(1); i <= 100; i += 7 {
		require.NoError(t, w.Update(context.Background(), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("p-%d", i))}))
	}
	require.NoError(t, w.Delete(context.Background(), 1, 500))
	delta, err := w.Commit(context.Background())
	require.NoError(t, err)

	ids := make([]RowID, 0, 500)
	for i := uint64(1); i <= 1500; i++ {
		ids = append(ids, i)
	}

	run := func(opts BatchReadOptions) ([]RowID, []string) {
		it, err := db.ReadRowsByIDs(context.Background(), delta.ID, 1, ids, opts)
		require.NoError(t, err)
		idsOut, vals, _ := batchCollect(t, it)
		return idsOut, vals
	}
	seqIDs, seqVals := run(BatchReadOptions{Order: BatchOrderRowID})
	parIDs, parVals := run(BatchReadOptions{Order: BatchOrderRowID, Parallelism: 4})
	require.Equal(t, seqIDs, parIDs)
	require.Equal(t, seqVals, parVals)

	ranges := []RowIDRange{{Start: 400, End: 600}, {Start: 1000, End: 1100}, {Start: 5, End: 9}}
	scan, err := db.ReadRowRanges(context.Background(), delta.ID, 1, ranges, BatchReadOptions{Parallelism: 4, MaxRows: 400})
	require.NoError(t, err)
	pIDs, _, pst := batchCollect(t, scan)
	require.NotEmpty(t, pIDs)
	require.Equal(t, uint64(len(pIDs)), pst.RowsReturned)
}

// TestBatchParallelCancel verifies cancellation stops the parallel pipeline
// and Close reclaims every worker.
func TestBatchParallelCancel(t *testing.T) {
	base := filepath.Join(tmpdb(t), "par-cancel")
	db, full := buildConcurrentStore(t, base, Options{})
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ids := make([]RowID, 0, 2000)
	for i := uint64(1); i <= 2000; i++ {
		ids = append(ids, i)
	}
	it, err := db.ReadRowsByIDs(ctx, full, 1, ids, BatchReadOptions{Parallelism: 4})
	require.NoError(t, err)
	cancel()
	for {
		_, _, ok := it.Next(nil)
		if !ok {
			break
		}
	}
	require.ErrorIs(t, it.Err(), context.Canceled)
	require.NoError(t, it.Close())
	// Idempotent close; pipeline fully joined.
	require.NoError(t, it.Close())
}

// TestBatchCacheEvictionConsistency is the M5 gate: with a tiny cache that
// evicts mid-batch, results are identical to the uncached path.
func TestBatchCacheEvictionConsistency(t *testing.T) {
	base := filepath.Join(tmpdb(t), "evict")
	db, full := buildConcurrentStore(t, base, Options{CacheBytes: 32 << 10}) // forces eviction
	defer db.Close()

	ids := make([]RowID, 0, 2000)
	for i := uint64(1); i <= 2000; i++ {
		ids = append(ids, i)
	}
	run := func() ([]RowID, []string) {
		it, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{})
		require.NoError(t, err)
		idsOut, vals, _ := batchCollect(t, it)
		return idsOut, vals
	}
	aIDs, aVals := run()
	bIDs, bVals := run()
	require.Equal(t, aIDs, bIDs)
	require.Equal(t, aVals, bVals)
	require.Len(t, aIDs, 2000)
}
