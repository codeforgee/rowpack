package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/lockfile"
	"github.com/stretchr/testify/require"
)

func TestFailedCommitReleasesWriterSlot(t *testing.T) {
	db := testDB(t, Options{})
	w, err := db.BeginFull(context.Background())
	require.NoError(t, err)
	_, err = w.Commit(context.Background())
	require.ErrorIs(t, err, ErrInvalidArgument)

	next, err := db.BeginFull(context.Background())
	require.NoError(t, err)
	require.NoError(t, next.Abort())
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
	require.Equal(t, 1024, db.opts.BlockSize)
	require.Equal(t, CompressionNone, db.opts.Compression)
}

func TestCloseWaitsForIterator(t *testing.T) {
	db := testDB(t, Options{})
	w, err := db.BeginFull(context.Background())
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", usersSchema()))
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
