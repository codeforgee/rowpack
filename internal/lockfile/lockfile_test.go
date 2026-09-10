package lockfile

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release second lock: %v", err)
	}
}

func TestAcquireBlocksSecondWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	l1, err := Acquire(path)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
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
	if err := l.Release(); err != nil {
		t.Fatalf("Release on nil Lock: %v", err)
	}

	l0 := &Lock{}
	if err := l0.Release(); err != nil {
		t.Fatalf("Release on zero Lock: %v", err)
	}
}

func TestAcquireOpenError(t *testing.T) {
	_, err := Acquire(filepath.Join(t.TempDir(), "missing-dir", "store.lock"))
	if err == nil {
		t.Fatal("Acquire on unwritable path succeeded")
	}
	if !strings.Contains(err.Error(), "open lock file") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReacquireAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	l2, err := Acquire(path)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	l2.Release()
}
