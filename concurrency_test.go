package rowpack

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConcurrentReadersDuringCommits hammers a store with concurrent readers
// (Get/Scan/ReadBatch/ScanBlocks over immutable snapshots) while a writer
// keeps committing deltas. Run under -race it validates the published-state
// immutability contract: readers capture one state and never observe torn
// views, and snapshot N stays readable forever once committed.
func TestConcurrentReadersDuringCommits(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{PageSize: 256, BlockSize: 1024})
	require.NoError(t, err)

	// FULL baseline: rows 1..50.
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "v", Type: TypeUint64},
	}))
	for i := 1; i <= 50; i++ {
		require.NoError(t, tx.Insert(ctx, "t", RowID(i), Row{Uint64(uint64(i)), Uint64(uint64(i))}))
	}
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	const (
		readers = 8
		commits = 20
	)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Reader waves: each reader grabs the current latest snapshot and reads
	// everything visible at it, forever until stop.
	for g := range readers {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snaps, err := db.ListSnapshots(ctx)
				if err != nil {
					t.Errorf("reader %d: ListSnapshots: %v", g, err)
					return
				}
				snap := snaps[len(snaps)-1].ID
				// Scan: strictly ascending ids, values == id.
				it, err := db.Scan(ctx, snap, "t", ScanOptions{})
				if err != nil {
					t.Errorf("reader %d: Scan: %v", g, err)
					return
				}
				prev := RowID(0)
				for {
					row, ok := it.Next()
					if !ok {
						break
					}
					id := it.RowID()
					if id <= prev {
						t.Errorf("reader %d: scan order %d after %d", g, id, prev)
						return
					}
					prev = id
					v, _ := row[1].Uint64()
					if v != uint64(id) {
						t.Errorf("reader %d: row %d value %d", g, id, v)
						return
					}
				}
				if err := it.Err(); err != nil {
					t.Errorf("reader %d: scan err %v", g, err)
					return
				}
				it.Close()
				// ReadBatch over a slice of ids.
				ids := []RowID{1, 25, 50, 1}
				rows, err := db.ReadBatch(ctx, snap, "t", ids)
				if err != nil {
					t.Errorf("reader %d: ReadBatch: %v", g, err)
					return
				}
				for k, id := range ids {
					v, _ := rows[k][0].Uint64()
					if v != uint64(id) {
						t.Errorf("reader %d: batch slot %d id %d value %d", g, k, id, v)
						return
					}
				}
			}
		}(g)
	}

	// Writer: sequential deltas updating the tail rows.
	for c := range commits {
		tx, err := db.Begin(ctx, Latest)
		require.NoError(t, err)
		id := RowID(40 + c%11)
		require.NoError(t, tx.Update(ctx, "t", id, Row{Uint64(uint64(id)), Uint64(uint64(id))}))
		_, err = tx.Commit(ctx)
		require.NoError(t, err)
	}
	close(stop)
	wg.Wait()

	// Final sanity: latest snapshot still fully readable.
	snaps, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, commits+1)
	last := snaps[len(snaps)-1].ID
	it, err := db.Scan(ctx, last, "t", ScanOptions{})
	require.NoError(t, err)
	n := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		n++
	}
	require.NoError(t, it.Err())
	it.Close()
	require.EqualValues(t, 50, n)
	require.NoError(t, db.Close())
}

// TestCloseWaitsForOpenIterator: Store.Close must block until a leaked-free
// open iterator releases its read lock; a concurrent Close + long scan must
// not corrupt state or panic.
func TestCloseWaitsForOpenIterator(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	for i := 1; i <= 2000; i++ {
		require.NoError(t, tx.Insert(ctx, "t", RowID(i), Row{Uint64(uint64(i))}))
	}
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	it, err := db.Scan(ctx, 1, "t", ScanOptions{})
	require.NoError(t, err)

	var wg sync.WaitGroup
	wg.Add(1)
	closed := make(chan error, 1)
	go func() {
		defer wg.Done()
		closed <- db.Close()
	}()

	// The scan keeps making progress while Close waits; then releases.
	consumed := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		consumed++
	}
	require.NoError(t, it.Err())
	require.NoError(t, it.Close())
	wg.Wait()
	require.NoError(t, <-closed)
	require.EqualValues(t, 2000, consumed)

	// Reads after Close fail cleanly.
	_, err = db.Get(ctx, 1, "t", 1, nil)
	require.ErrorIs(t, err, ErrClosed)
	_, err = db.Scan(ctx, 1, "t", ScanOptions{})
	require.ErrorIs(t, err, ErrClosed)
	_, err = db.ReadBatch(ctx, 1, "t", []RowID{1})
	require.ErrorIs(t, err, ErrClosed)
	_, err = db.Begin(ctx, NoParent)
	require.Error(t, err)
	_, err = db.ListSnapshots(ctx)
	require.ErrorIs(t, err, ErrClosed)
}
