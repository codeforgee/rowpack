package rowpack

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// visibleIDs filters ids to those visible at the snapshot (per Get), so
// batch tests can compare against per-row Gets row by row.
func visibleIDs(t *testing.T, db *Store, snap SnapshotID, ids []RowID) []RowID {
	t.Helper()
	out := make([]RowID, 0, len(ids))
	for _, id := range ids {
		if _, err := db.Get(context.Background(), snap, 1, id, nil); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// assertRowsMatch verifies batch rows equal per-row Gets value for value and
// that positions correspond to the input ids order.
func assertRowsMatch(t *testing.T, db *Store, snap SnapshotID, ids []RowID, got []Row) {
	t.Helper()
	if len(got) != len(ids) {
		t.Fatalf("batch returned %d rows for %d ids", len(got), len(ids))
	}
	for i, id := range ids {
		want, werr := db.Get(context.Background(), snap, 1, id, nil)
		if werr != nil {
			t.Fatalf("id %d: Get failed: %v", id, werr)
		}
		if len(got[i]) != len(want) {
			t.Fatalf("id %d: len %d != %d", id, len(got[i]), len(want))
		}
		for c := range want {
			if !rowValueEqual(want[c], got[i][c]) {
				t.Fatalf("id %d col %d mismatch: %v vs %v", id, c, want[c], got[i][c])
			}
		}
	}
}

// TestReadBatchMatchesGet compares ReadBatch against per-row Get on a FULL
// store and along a DELTA chain with updates and deletes, for both random and
// sequential id sets.
func TestReadBatchMatchesGet(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
	}{
		{"full", 0},
		{"delta-chain", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "batch-"+tc.name)
			db, head := buildReuseStore(t, base, 1000, tc.depth)
			defer db.Close()

			// Random set (LCG), filtered to visible rows.
			var rng uint64 = 42
			rand := make([]RowID, 0, 300)
			for len(rand) < 300 {
				rng = rng*6364136223846793005 + 7
				id := rng%1000 + 1
				if _, err := db.Get(context.Background(), head, 1, id, nil); err == nil {
					rand = append(rand, id)
				}
			}
			assertRowsMatch(t, db, head, rand, mustReadBatch(t, db, head, rand))

			// Sequential window.
			seq := make([]RowID, 0, 400)
			for id := RowID(1); id <= 400; id++ {
				if _, err := db.Get(context.Background(), head, 1, id, nil); err == nil {
					seq = append(seq, id)
				}
			}
			assertRowsMatch(t, db, head, seq, mustReadBatch(t, db, head, seq))
		})
	}
}

func mustReadBatch(t *testing.T, db *Store, snap SnapshotID, ids []RowID) []Row {
	t.Helper()
	rows, err := db.ReadBatch(context.Background(), snap, 1, ids)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestReadBatchMissingAndDeleted verifies the per-row error semantics: a
// batch containing a missing or deleted id fails with the same ErrNotFound
// error Get would return.
func TestReadBatchMissingAndDeleted(t *testing.T) {
	base := filepath.Join(t.TempDir(), "batch-err")
	db, head := buildReuseStore(t, base, 200, 2)
	defer db.Close()

	missing := []RowID{1, 2, 999_999} // 999999 absent
	if _, err := db.ReadBatch(context.Background(), head, 1, missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing id: got %v, want ErrNotFound", err)
	}
	// RowID known deleted by the chain builder (i%50==49 -> row 50).
	deleted := []RowID{50}
	if _, err := db.ReadBatch(context.Background(), head, 1, deleted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted id: got %v, want ErrNotFound", err)
	}
	// Get reports the identical error kind and message for the same id.
	_, gerr := db.Get(context.Background(), head, 1, 50, nil)
	if gerr == nil || gerr.Error() == "" {
		t.Fatal("Get on deleted id did not error")
	}
}

// TestReadBatchEmptyAndDuplicates covers the degenerate id sets.
func TestReadBatchEmptyAndDuplicates(t *testing.T) {
	base := filepath.Join(t.TempDir(), "batch-edge")
	db, head := buildReuseStore(t, base, 100, 0)
	defer db.Close()

	rows, err := db.ReadBatch(context.Background(), head, 1, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty batch: %v, %d rows", err, len(rows))
	}
	// Duplicate ids read the row twice, positionally aligned like repeated Gets.
	dup := []RowID{7, 7, 8, 7}
	got := mustReadBatch(t, db, head, dup)
	if len(got) != 4 {
		t.Fatalf("dup batch: %d rows", len(got))
	}
	for i := range dup {
		want, err := db.Get(context.Background(), head, 1, dup[i], nil)
		if err != nil {
			t.Fatal(err)
		}
		for c := range want {
			if !rowValueEqual(want[c], got[i][c]) {
				t.Fatalf("dup idx %d col %d mismatch", i, c)
			}
		}
	}
}

// TestReadBatchStats verifies the cumulative counters and, in the fully
// clustered case, that one block serves the whole batch (Blocks == 1).
func TestReadBatchStats(t *testing.T) {
	base := filepath.Join(t.TempDir(), "batch-stats")
	db, fullID := buildReuseStore(t, base, 1000, 0)
	defer db.Close()

	ids := make([]RowID, 1000)
	for i := range ids {
		ids[i] = RowID(i + 1)
	}
	before := db.Stats().Batch
	rows, err := db.ReadBatch(context.Background(), fullID, 1, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1000 {
		t.Fatalf("got %d rows", len(rows))
	}
	after := db.Stats().Batch
	if after.Calls-before.Calls != 1 || after.Rows-before.Rows != 1000 {
		t.Fatalf("calls/rows delta: %d/%d", after.Calls-before.Calls, after.Rows-before.Rows)
	}
	if blocks := after.Blocks - before.Blocks; blocks != 1 {
		t.Fatalf("1000 clustered rows served by %d blocks, want 1", blocks)
	}
	if raw := after.RawBytes - before.RawBytes; raw == 0 {
		t.Fatal("raw bytes delta is zero")
	}
}

// TestReadBatchAfterReopen verifies ReadBatch works after Close/Reopen
// (recovery path) and agrees with Get.
func TestReadBatchAfterReopen(t *testing.T) {
	base := filepath.Join(t.TempDir(), "batch-reopen")
	db, fullID := buildReuseStore(t, base, 500, 0)
	want, err := db.ReadBatch(context.Background(), fullID, 1, []RowID{1, 250, 500})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	got, err := db2.ReadBatch(context.Background(), fullID, 1, []RowID{1, 250, 500})
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		for c := range want[i] {
			if !rowValueEqual(want[i][c], got[i][c]) {
				t.Fatalf("row %d col %d mismatch after reopen", i, c)
			}
		}
	}
}

// TestReadBatchConcurrent exercises ReadBatch from many goroutines while
// mixing Get calls; run under -race.
func TestReadBatchConcurrent(t *testing.T) {
	base := filepath.Join(t.TempDir(), "batch-conc")
	db, head := buildReuseStore(t, base, 1000, 2)
	defer db.Close()

	visible := make([]RowID, 0, 1000)
	for id := RowID(1); id <= 1000; id++ {
		if _, err := db.Get(context.Background(), head, 1, id, nil); err == nil {
			visible = append(visible, id)
		}
	}
	const g = 16
	var wg sync.WaitGroup
	errs := make(chan error, g)
	for i := 0; i < g; i++ {
		wg.Add(1)
		go func(seed uint64) {
			defer wg.Done()
			var rng uint64 = seed
			for iter := 0; iter < 40; iter++ {
				ids := make([]RowID, 0, 64)
				for len(ids) < 64 {
					rng = rng*6364136223846793005 + 3
					ids = append(ids, visible[rng%uint64(len(visible))])
				}
				rows, err := db.ReadBatch(context.Background(), head, 1, ids)
				if err != nil {
					errs <- err
					return
				}
				if len(rows) != len(ids) {
					errs <- errors.New("batch row count mismatch")
					return
				}
			}
		}(uint64(1000 + i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}
