package iofile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newTestAppender(t *testing.T) (string, *Appender) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err, "OpenAppender")
	return path, a
}

func TestOpenAppender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	a, err := OpenAppender(path, true)
	require.NoError(t, err, "exclusive create")
	if a.Offset() != 0 {
		t.Fatalf("new appender offset %d, want 0", a.Offset())
	}
	a.Close()

	if _, err := OpenAppender(path, true); err == nil {
		t.Fatal("second exclusive create should fail")
	}

	a, err = OpenAppender(path, false)
	require.NoError(t, err, "open existing")
	a.Close()

	if _, err := OpenAppender(filepath.Join(dir, "nope.bin"), false); err == nil {
		t.Fatal("open non-existent without create should fail")
	}
}

func TestAppendAndReadAt(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	off, err := a.Append([]byte("hello"))
	require.NoError(t, err, "Append")
	require.Equal(t, int64(0), off, "first append offset %d, want 0", off)
	off, err = a.Append([]byte(" world"))
	require.NoError(t, err, "Append")
	require.Equal(t, int64(5), off, "second append offset %d, want 5", off)
	if a.Offset() != 11 {
		t.Fatalf("offset %d, want 11", a.Offset())
	}

	buf := make([]byte, 11)
	n, err := a.ReadAt(buf, 0)
	require.NoError(t, err, "ReadAt")
	if n != 11 || string(buf) != "hello world" {
		t.Fatalf("ReadAt got %q n=%d, want %q", buf, n, "hello world")
	}
}

func TestAppendZeroes(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	_, err := a.Append([]byte("abc"))
	require.NoError(t, err, "Append")
	zoff, err := a.AppendZeroes(20000)
	require.NoError(t, err, "AppendZeroes")
	require.Equal(t, int64(3), zoff, "zero append offset %d, want 3", zoff)
	if a.Offset() != 20003 {
		t.Fatalf("offset %d, want 20003", a.Offset())
	}

	buf := make([]byte, 3)
	if _, err := a.ReadAt(buf, 3); err != nil {
		t.Fatalf("ReadAt zeros: %v", err)
	}
	if buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
		t.Fatalf("zeroes padding not zero, got %v", buf)
	}

	off, err := a.Append([]byte("x"))
	require.NoError(t, err, "Append after zeroes")
	require.Equal(t, int64(20003), off, "append after zeroes offset %d, want 20003", off)
}

func TestViewMmap(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()
	defer ForceReadAt(false)

	ForceReadAt(false)
	if _, err := a.Append([]byte("0123456789")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	v, done, err := a.View(2, 6)
	require.NoError(t, err, "View")
	defer done()
	if string(v) != "234567" {
		t.Fatalf("view content %q, want %q", v, "234567")
	}
}

func TestViewForceReadAt(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()
	ForceReadAt(true)
	defer ForceReadAt(false)

	if _, err := a.Append([]byte("abcdef")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	v, done, err := a.View(1, 3)
	require.NoError(t, err, "View fallback")
	defer done()
	if string(v) != "bcd" {
		t.Fatalf("view content %q, want %q", v, "bcd")
	}
}

func TestViewGrowsOnDemand(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	if _, err := a.Append([]byte("short")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := a.AppendZeroes(1000); err != nil {
		t.Fatalf("AppendZeroes: %v", err)
	}
	v, done, err := a.View(0, a.Offset())
	require.NoError(t, err, "View across growth")
	done()
	if len(v) != 1005 {
		t.Fatalf("view length %d, want 1005", len(v))
	}
}

func TestViewErrors(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	if _, _, err := a.View(-1, 4); err == nil {
		t.Fatal("negative offset view succeeded")
	} else {
		require.Contains(t, err.Error(), "invalid view range", "unexpected error: %v", err)
	}
	if _, _, err := a.View(0, -3); err == nil {
		t.Fatal("negative length view succeeded")
	}
	if _, _, err := a.View(0, 10); err == nil {
		t.Fatal("view beyond EOF succeeded")
	} else {
		require.Contains(t, err.Error(), "beyond file size", "unexpected error: %v", err)
	}
}

func TestReadAllAndSize(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	if _, err := a.Append([]byte("one-two")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	sz := a.Size()
	require.Equal(t, int64(7), sz, "size %d, want 7", sz)
	b := make([]byte, 7)
	if _, err := a.ReadAt(b, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(b) != "one-two" {
		t.Fatalf("ReadAt got %q", b)
	}
}

func TestTruncate(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	if _, err := a.Append([]byte("0123456789")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	_, done, err := a.View(0, 10)
	require.NoError(t, err, "View")
	done()

	err = a.Truncate(4)
	require.NoError(t, err, "Truncate")
	if a.Offset() != 4 {
		t.Fatalf("offset after truncate %d, want 4", a.Offset())
	}
	sz := a.Size()
	require.Equal(t, int64(4), sz, "size after truncate %d, want 4", sz)

	n, err := a.ReadAt([]byte("0123456789"), 0)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("ReadAt truncated region: %v", err)
	}
	require.EqualValues(t, 4, n, "read %d, want 4", n)
}

func TestTruncateGrowsOffsetBack(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	err := a.Truncate(100)
	require.NoError(t, err, "Truncate grows file")
	if a.Offset() != 0 {
		t.Fatalf("offset %d, want 0 (offset can only rewind, never forward)", a.Offset())
	}
}

func TestSyncAndFile(t *testing.T) {
	path, a := newTestAppender(t)
	defer a.Close()

	if _, err := a.Append([]byte("data")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	err := a.Sync()
	require.NoError(t, err, "Sync")
	fi, err := os.Stat(path)
	require.NoError(t, err, "Stat")
	if fi.Size() != 4 {
		t.Fatalf("file size %d, want 4", fi.Size())
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if Exists(filepath.Join(dir, "nope")) {
		t.Fatal("Exists true for missing file")
	}
	p := filepath.Join(dir, "yes")
	err := os.WriteFile(p, nil, 0o644)
	require.NoError(t, err, "WriteFile")
	if !Exists(p) {
		t.Fatal("Exists false for existing file")
	}
}

func TestCreateSingle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.rpk")

	err := CreateSingle(path, []byte("HEADER"), false)
	require.NoError(t, err, "CreateSingle")
	b, err := os.ReadFile(path)
	require.NoError(t, err, "ReadFile")
	if string(b) != "HEADER" {
		t.Fatalf("header persisted %q, want %q", b, "HEADER")
	}

	require.Error(t, CreateSingle(path, []byte("X"), false), "second CreateSingle on existing file succeeded")
}

func TestCreateSingleUnwritableDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "store.rpk")
	require.Error(t, CreateSingle(path, []byte("HEADER"), false), "CreateSingle in missing dir succeeded")
	if Exists(path) {
		t.Fatal("partial file left behind after failed create")
	}
}

func TestAppenderDoubleClose(t *testing.T) {
	_, a := newTestAppender(t)
	err := a.Close()
	require.NoError(t, err, "Close")
	a.Close()
}
