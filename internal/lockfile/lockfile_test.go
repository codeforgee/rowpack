package lockfile

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	l, err := Acquire(path)
	require.NoError(t, err, "Acquire")
	err = l.Release()
	require.NoError(t, err, "Release")

	l2, err := Acquire(path)
	require.NoError(t, err, "Acquire after release")
	err = l2.Release()
	require.NoError(t, err, "Release second lock")
}

func TestAcquireBlocksSecondWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	l1, err := Acquire(path)
	require.NoError(t, err, "first Acquire")
	defer l1.Release()

	l2, err := Acquire(path)
	if err == nil {
		l2.Release()
		t.Fatal("second Acquire succeeded while first lock held")
	}
	if !strings.Contains(err.Error(), "locked by another writer") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReleaseNilLockLarger(t *testing.T) {
	var l *Lock
	err := l.Release()
	require.NoError(t, err, "Release on nil Lock")

	l0 := &Lock{}
	err = l0.Release()
	require.NoError(t, err, "Release on zero Lock")
}

func TestAcquireOpenError(t *testing.T) {
	_, err := Acquire(filepath.Join(t.TempDir(), "missing-dir", "store.lock"))
	require.Error(t, err, "Acquire on unwritable path succeeded")
	if !strings.Contains(err.Error(), "open lock file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReacquireAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	l, err := Acquire(path)
	require.NoError(t, err, "Acquire")
	err = l.Release()
	require.NoError(t, err, "Release")

	l2, err := Acquire(path)
	require.NoError(t, err, "reacquire")
	l2.Release()
}
