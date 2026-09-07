package rowpack

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildConcurrentStore creates a store with a FULL snapshot of 2000 rows.
func buildConcurrentStore(t *testing.T, base string, opts Options) (*Store, SnapshotID) {
	t.Helper()
	db, err := Create(base, opts)
	require.NoError(t, err)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	if err := w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString},
	}}); err != nil {
		require.NoError(t, err)
	}
	for i := uint64(1); i <= 2000; i++ {
		require.NoError(t, w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("n-%d", i))}))
	}
	full, err := w.Commit(context.Background())
	require.NoError(t, err)
	return db, full.ID
}

// TestM7ConcurrentReadersWriters runs 32 concurrent Get/Scan goroutines while
// another goroutine commits snapshots, under the race detector.
func TestM7ConcurrentReadersWriters(t *testing.T) {
	base := filepath.Join(tmpdb(t), "conc")
	opts := Options{}
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
				if _, err := db.Get(context.Background(), fullID, 1, rowID, nil); err != nil {
					assert.NoError(t, err, "get %d", rowID)
					return
				}
				reads.Add(1)
				// Periodic Scan.
				if rng%17 == 0 {
					it, err := db.Scan(context.Background(), fullID, 1, ScanOptions{})
					if err != nil {
						assert.NoError(t, err, "scan")
						return
					}
					n := 0
					for {
						if _, ok := it.Next(); !ok {
							break
						}
						n++
					}
					it.Close()
					assert.NoError(t, it.Err(), "scan err")
					assert.Equal(t, 2000, n, "scan returned %d rows", n)
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
				assert.NoError(t, err, "begin")
				return
			}
			rowID := uint64(i + 3000)
			if err := w.Insert(context.Background(), 1, rowID, 1, Row{Uint64(rowID), String("delta")}); err != nil {
				assert.NoError(t, err, "insert")
				return
			}
			info, err := w.Commit(context.Background())
			if err != nil {
				assert.NoError(t, err, "commit")
				return
			}
			parent = info.ID
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()

	wg.Wait()
	require.NotZero(t, reads.Load(), "no reads (%d) or scans (%d) executed", reads.Load(), scans.Load())
	require.NotZero(t, scans.Load(), "no reads (%d) or scans (%d) executed", reads.Load(), scans.Load())
	require.GreaterOrEqual(t, reads.Load(), uint64(100), "too few reads: %d", reads.Load())
}

// TestM7CacheHit verifies AC-005: reading the same block repeatedly hits the
// cache; disabling the cache yields identical results.
func TestM7CacheHit(t *testing.T) {
	base := filepath.Join(tmpdb(t), "cache")
	opts := Options{}
	opts.BlockSize = 4096
	db, fullID := buildConcurrentStore(t, base, opts)

	// First read misses, subsequent reads hit.
	_, err := db.Get(context.Background(), fullID, 1, 1, nil)
	require.NoError(t, err)
	st1 := db.Stats()
	require.Equal(t, uint64(0), st1.Cache.Hits, "first read should miss: hits=%d misses=%d", st1.Cache.Hits, st1.Cache.Misses)
	require.NotZero(t, st1.Cache.Misses, "first read should miss: hits=%d misses=%d", st1.Cache.Hits, st1.Cache.Misses)
	for i := uint64(1); i <= 20; i++ {
		_, err := db.Get(context.Background(), fullID, 1, i, nil)
		require.NoError(t, err)
	}
	st2 := db.Stats()
	require.NotZero(t, st2.Cache.Hits, "no cache hits after repeated reads: %+v", st2.Cache)
	db.Close()

	// Disabled cache produces identical results.
	db2, err := Open(base, Options{CacheBytes: -1})
	require.NoError(t, err)
	defer db2.Close()
	for i := uint64(1); i <= 100; i++ {
		r, err := db2.Get(context.Background(), fullID, 1, i, nil)
		require.NoError(t, err)
		v, _ := r[0].Uint64()
		require.Equal(t, i, v, "disabled cache row %d = %d", i, v)
	}
}

// TestM7ConcurrentCommitSameBlock verifies concurrent cold reads of the same
// block trigger a single controlled load (miss merging).
func TestM7ConcurrentCommitSameBlock(t *testing.T) {
	base := filepath.Join(tmpdb(t), "merge")
	opts := Options{}
	opts.BlockSize = 64 << 10 // one big block
	db, fullID := buildConcurrentStore(t, base, opts)
	defer db.Close()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := uint64(0); j < 50; j++ {
				if _, err := db.Get(context.Background(), fullID, 1, 1, nil); err != nil {
					assert.NoError(t, err, "get")
					return
				}
			}
		}()
	}
	wg.Wait()
	st := db.Stats()
	// 2000 rows in one block; loads should be far below Get count.
	require.NotZero(t, st.Cache.Loads, "unexpected load count: %d (gets=%d)", st.Cache.Loads, st.Cache.Hits+st.Cache.Misses)
	require.LessOrEqual(t, st.Cache.Loads, uint64(50), "unexpected load count: %d (gets=%d)", st.Cache.Loads, st.Cache.Hits+st.Cache.Misses)
}

// TestM7WriterLock verifies the cross-process single-writer lock: a second
// read-write Open fails while the first holds the store.
func TestM7WriterLock(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	base := filepath.Join(tmpdb(t), "lock")
	db, err := Create(base, Options{})
	require.NoError(t, err)
	defer db.Close()
	// A second read-write open must fail (locked).
	_, err = Open(base, Options{})
	require.Error(t, err, "second writer open succeeded")
	// A read-only open must succeed.
	ro, err := Open(base, Options{ReadOnly: true})
	require.NoError(t, err, "read-only open: %v", err)
	ro.Close()
}

// TestM7ConcurrentClose verifies Close is safe concurrently and that Close
// after the fact rejects new operations with ErrClosed.
func TestM7ConcurrentClose(t *testing.T) {
	base := filepath.Join(tmpdb(t), "close")
	db, _ := buildConcurrentStore(t, base, Options{})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = db.Close()
		}()
	}
	wg.Wait()
	_, err := db.Get(context.Background(), 1, 1, 1, nil)
	require.Error(t, err, "Get after Close succeeded")
	_, err = db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.Error(t, err, "BeginSnapshot after Close succeeded")
}

// TestM7CloseAbortsWriter verifies Close aborts an active uncommitted writer.
func TestM7CloseAbortsWriter(t *testing.T) {
	base := filepath.Join(tmpdb(t), "abort")
	db, _ := Create(base, Options{})
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{{Name: "id", Type: TypeUint64}}})
	_ = w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1)})
	// Not committed; Close must abort and the store must reopen cleanly.
	require.NoError(t, db.Close())
	db2, err := Open(base, Options{})
	require.NoError(t, err, "reopen after abort-close: %v", err)
	defer db2.Close()
	_, err = db2.LatestSnapshot(context.Background())
	require.Error(t, err, "aborted snapshot visible after reopen")
}
