package rowpack

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// buildConcurrentStore creates a store with a FULL snapshot of 2000 rows.
func buildConcurrentStore(t *testing.T, base string, opts Options) (*Store, SnapshotID) {
	t.Helper()
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString},
	}}); err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 2000; i++ {
		if err := w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("n-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	full, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return db, full.ID
}

// TestM7ConcurrentReadersWriters runs 32 concurrent Get/Scan goroutines while
// another goroutine commits snapshots, under the race detector.
func TestM7ConcurrentReadersWriters(t *testing.T) {
	base := filepath.Join(t.TempDir(), "conc")
	opts := DefaultOptions()
	opts.BlockSize = 2048
	db, fullID := buildConcurrentStore(t, base, opts)
	defer db.Close()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads, scans atomic.Uint64

	// 32 readers.
	for g := 0; g < 32; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			rng := uint64(seed)
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Random Get against the FULL snapshot.
				rowID := (rng % 2000) + 1
				if _, err := db.Get(context.Background(), fullID, 1, rowID); err != nil {
					t.Errorf("get %d: %v", rowID, err)
					return
				}
				reads.Add(1)
				// Periodic Scan.
				if rng%17 == 0 {
					it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
					if err != nil {
						t.Errorf("scan: %v", err)
						return
					}
					n := 0
					for it.Next() {
						n++
					}
					it.Close()
					if it.Err() != nil {
						t.Errorf("scan err: %v", it.Err())
						return
					}
					if n != 2000 {
						t.Errorf("scan returned %d rows", n)
						return
					}
					scans.Add(1)
				}
				rng = rng*6364136223846793005 + 1442695040888963407
			}
		}(g)
	}

	// One writer committing DELTAs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		parent := fullID
		for i := 0; i < 10; i++ {
			w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: parent})
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			rowID := uint64(i + 3000)
			if err := w.Insert(context.Background(), 1, rowID, 1, Row{Uint64(rowID), String("delta")}); err != nil {
				t.Errorf("insert: %v", err)
				return
			}
			info, err := w.Commit(context.Background())
			if err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			parent = info.ID
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()

	wg.Wait()
	if reads.Load() == 0 || scans.Load() == 0 {
		t.Fatalf("no reads (%d) or scans (%d) executed", reads.Load(), scans.Load())
	}
	if reads.Load() < 100 {
		t.Fatalf("too few reads: %d", reads.Load())
	}
}

// TestM7CacheHit verifies AC-005: reading the same block repeatedly hits the
// cache; disabling the cache yields identical results.
func TestM7CacheHit(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cache")
	opts := DefaultOptions()
	opts.BlockSize = 4096
	db, fullID := buildConcurrentStore(t, base, opts)

	// First read misses, subsequent reads hit.
	if _, err := db.Get(context.Background(), fullID, 1, 1); err != nil {
		t.Fatal(err)
	}
	st1 := db.Stats()
	if st1.Cache.Hits != 0 || st1.Cache.Misses == 0 {
		t.Fatalf("first read should miss: hits=%d misses=%d", st1.Cache.Hits, st1.Cache.Misses)
	}
	for i := uint64(1); i <= 20; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i); err != nil {
			t.Fatal(err)
		}
	}
	st2 := db.Stats()
	if st2.Cache.Hits == 0 {
		t.Fatalf("no cache hits after repeated reads: %+v", st2.Cache)
	}
	db.Close()

	// Disabled cache produces identical results.
	db2, err := Open(base, Options{CacheBytes: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for i := uint64(1); i <= 100; i++ {
		r, err := db2.Get(context.Background(), fullID, 1, i)
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := r[0].Uint64(); v != i {
			t.Fatalf("disabled cache row %d = %d", i, v)
		}
	}
}

// TestM7ConcurrentCommitSameBlock verifies concurrent cold reads of the same
// block trigger a single controlled load (miss merging).
func TestM7ConcurrentCommitSameBlock(t *testing.T) {
	base := filepath.Join(t.TempDir(), "merge")
	opts := DefaultOptions()
	opts.BlockSize = 64 << 10 // one big block
	db, fullID := buildConcurrentStore(t, base, opts)
	defer db.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := uint64(0); j < 50; j++ {
				if _, err := db.Get(context.Background(), fullID, 1, 1); err != nil {
					t.Errorf("get: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	st := db.Stats()
	// 2000 rows in one block; loads should be far below Get count.
	if st.Cache.Loads == 0 || st.Cache.Loads > 50 {
		t.Fatalf("unexpected load count: %d (gets=%d)", st.Cache.Loads, st.Cache.Hits+st.Cache.Misses)
	}
}

// TestM7WriterLock verifies the cross-process single-writer lock: a second
// read-write Open fails while the first holds the store.
func TestM7WriterLock(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	base := filepath.Join(t.TempDir(), "lock")
	db, err := Create(base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A second read-write open must fail (locked).
	if _, err := Open(base, DefaultOptions()); err == nil {
		t.Fatal("second writer open succeeded")
	}
	// A read-only open must succeed.
	ro, err := Open(base, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	ro.Close()
}

// TestM7ConcurrentClose verifies Close is safe concurrently and that Close
// after the fact rejects new operations with ErrClosed.
func TestM7ConcurrentClose(t *testing.T) {
	base := filepath.Join(t.TempDir(), "close")
	db, _ := buildConcurrentStore(t, base, DefaultOptions())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = db.Close()
		}()
	}
	wg.Wait()
	if _, err := db.Get(context.Background(), 1, 1, 1); err == nil {
		t.Fatal("Get after Close succeeded")
	}
	if _, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{}); err == nil {
		t.Fatal("BeginSnapshot after Close succeeded")
	}
}

// TestM7CloseAbortsWriter verifies Close aborts an active uncommitted writer.
func TestM7CloseAbortsWriter(t *testing.T) {
	base := filepath.Join(t.TempDir(), "abort")
	db, _ := Create(base, DefaultOptions())
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{{Name: "id", Type: TypeUint64}}})
	_ = w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1)})
	// Not committed; Close must abort and the store must reopen cleanly.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(base, DefaultOptions())
	if err != nil {
		t.Fatalf("reopen after abort-close: %v", err)
	}
	defer db2.Close()
	if _, err := db2.LatestSnapshot(context.Background()); err == nil {
		t.Fatal("aborted snapshot visible after reopen")
	}
}
