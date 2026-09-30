package rowpack

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/codeforgee/rowpack/internal/lockfile"
	"github.com/stretchr/testify/require"
)

func TestFailedCommitReleasesWriterSlot(t *testing.T) {
	db := testDB(t, Options{})
	w, err := db.Begin(context.Background(), NoParent)
	require.NoError(t, err)
	_, err = w.Commit(context.Background())
	require.ErrorIs(t, err, ErrInvalidArgument)

	next, err := db.Begin(context.Background(), NoParent)
	require.NoError(t, err)
	require.NoError(t, next.Rollback())
}

func TestCreateCleansFileWhenOpenFails(t *testing.T) {
	base := filepath.Join(tmpdb(t), "create-cleanup")
	l, err := lockfile.Acquire(base + ".lock")
	require.NoError(t, err)
	defer l.Release()

	_, err = Create(base, Options{})
	require.Error(t, err)
	_, statErr := os.Stat(base + ".rpk")
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestOpenUsesFileWriterDefaults(t *testing.T) {
	base := filepath.Join(tmpdb(t), "defaults")
	db, err := Create(base, Options{BlockSize: 1024, Compression: CompressionNone})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(base, Options{BlockSize: 4096, Compression: CompressionZstd})
	require.NoError(t, err)
	defer db.Close()
	require.EqualValues(t, 1024, db.opts.BlockSize)
	require.Equal(t, CompressionNone, db.opts.Compression)
}

func TestCloseWaitsForIterator(t *testing.T) {
	db := testDB(t, Options{})
	w, err := db.Begin(context.Background(), NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 2)
	snap, err := w.Commit(context.Background())
	require.NoError(t, err)

	it, err := db.Scan(context.Background(), snap, "users", ScanOptions{})
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- db.Close() }()
	select {
	case err := <-done:
		t.Fatalf("Close returned before iterator closed: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	require.NoError(t, it.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close did not resume after iterator closed")
	}
}

// TestUnknownCommitFailureRequiresReopen drives the unknown-outcome commit
// contract: a failure AFTER the durability sync (here: snapshot depth limit
// rejected inside View.Apply) leaves the on-disk snapshot's visibility
// ambiguous, so the store must refuse new writers (ErrMustReopen) while
// reads stay served from the last published view — and reopening must run
// recovery, align the view with the file, and restore writability.
func TestUnknownCommitFailureRequiresReopen(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "unknown-commit")
	db, err := Create(base, Options{Limits: Limits{MaxSnapshotDepth: 1}})
	require.NoError(t, err)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", usersSchema()))
	insertUsers(t, tx, 3)
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// The DELTA commits fine on disk (sync succeeded) but the in-memory view
	// rejects it: depth 2 exceeds MaxSnapshotDepth 1. The commit fails with
	// an unknown outcome after the durability point.
	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, tx2.Insert("users", 4, Row{Uint64(4), String("user-4"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(400), Scale: 2})}))
	_, err = tx2.Commit(ctx)
	var ce *CommitError
	require.ErrorAs(t, err, &ce)
	require.True(t, ce.Unknown, "post-sync failure must report unknown outcome")

	// New writers are refused until reopen...
	_, err = db.Begin(ctx, NoParent)
	require.ErrorIs(t, err, ErrMustReopen)
	_, err = db.Begin(ctx, Latest)
	require.ErrorIs(t, err, ErrMustReopen)

	// ...while reads stay self-consistent on the last published snapshot.
	row, err := db.Get(ctx, full, "users", 1, nil)
	require.NoError(t, err)
	_, ok := row[0].Uint64()
	require.True(t, ok)

	// Reopen with a larger depth budget: recovery replays the file, adopts
	// the ambiguous snapshot, and writability is restored.
	require.NoError(t, db.Close())
	db2, err := Open(base, Options{Limits: Limits{MaxSnapshotDepth: 4}})
	require.NoError(t, err)
	defer db2.Close()
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	row, err = db2.Get(ctx, snaps[1].ID, "users", 4, nil)
	require.NoError(t, err)
	v, ok := row[0].Uint64()
	require.True(t, ok)
	require.Equal(t, uint64(4), v)

	tx3, err := db2.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, tx3.Insert("users", 5, Row{Uint64(5), String("user-5"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(500), Scale: 2})}))
	_, err = tx3.Commit(ctx)
	require.NoError(t, err)
}

// TestIteratorFinalizerUnblocksClose verifies the leak safety net: an
// iterator that is never closed holds the store's read lock, but its GC
// finalizer releases it, so Store.Close resumes instead of hanging forever.
func TestIteratorFinalizerUnblocksClose(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", usersSchema()))
	insertUsers(t, tx, 2)
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	leakIterator(t, db, snap)

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, db.Close())
	}()
	// The finalizer runs once GC observes the iterator is unreachable;
	// conservative stack scanning may retain it for a few cycles, so keep
	// collecting until Close resumes or the deadline expires.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		select {
		case <-done:
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("Store.Close still blocked on a leaked iterator after finalization")
}

// leakIterator opens a Scan iterator and drops it without Close.
func leakIterator(t *testing.T, db *Store, snap SnapshotID) {
	t.Helper()
	it, err := db.Scan(context.Background(), snap, "users", ScanOptions{})
	require.NoError(t, err)
	if row, ok := it.Next(); !ok {
		t.Fatalf("scan produced no rows: %v", it.Err())
	} else {
		_ = row
	}
	// it goes out of scope unreferenced: no Close call.
}
