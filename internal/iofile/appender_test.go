package iofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestAppender(t *testing.T) (string, *Appender) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatalf("OpenAppender: %v", err)
	}
	return path, a
}

func TestOpenAppender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatalf("exclusive create: %v", err)
	}
	if a.Offset() != 0 {
		t.Fatalf("new appender offset %d, want 0", a.Offset())
	}
	a.Close()

	if _, err := OpenAppender(path, true); err == nil {
		t.Fatal("second exclusive create should fail")
	}

	a, err = OpenAppender(path, false)
	if err != nil {
		t.Fatalf("open existing: %v", err)
	}
	a.Close()

	if _, err := OpenAppender(filepath.Join(dir, "nope.bin"), false); err == nil {
		t.Fatal("open non-existent without create should fail")
	}
}

func TestAppendAndReadAt(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	off, err := a.Append([]byte("hello"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off != 0 {
		t.Fatalf("first append offset %d, want 0", off)
	}
	off, err = a.Append([]byte(" world"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off != 5 {
		t.Fatalf("second append offset %d, want 5", off)
	}
	if a.Offset() != 11 {
		t.Fatalf("offset %d, want 11", a.Offset())
	}

	buf := make([]byte, 11)
	n, err := a.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 11 || string(buf) != "hello world" {
		t.Fatalf("ReadAt got %q n=%d, want %q", buf, n, "hello world")
	}
}

func TestAppendZeroes(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	_, err := a.Append([]byte("abc"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	zoff, err := a.AppendZeroes(20000)
	if err != nil {
		t.Fatalf("AppendZeroes: %v", err)
	}
	if zoff != 3 {
		t.Fatalf("zero append offset %d, want 3", zoff)
	}
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
	if err != nil {
		t.Fatalf("Append after zeroes: %v", err)
	}
	if off != 20003 {
		t.Fatalf("append after zeroes offset %d, want 20003", off)
	}
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
	if err != nil {
		t.Fatalf("View: %v", err)
	}
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
	if err != nil {
		t.Fatalf("View fallback: %v", err)
	}
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
	if err != nil {
		t.Fatalf("View across growth: %v", err)
	}
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
	} else if !strings.Contains(err.Error(), "invalid view range") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, _, err := a.View(0, -3); err == nil {
		t.Fatal("negative length view succeeded")
	}
	if _, _, err := a.View(0, 10); err == nil {
		t.Fatal("view beyond EOF succeeded")
	} else if !strings.Contains(err.Error(), "beyond file size") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReadAllAndSize(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	if _, err := a.Append([]byte("one-two")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	sz, err := a.Size()
	if err != nil {
		t.Fatalf("Size: %v", err)
	}
	if sz != 7 {
		t.Fatalf("size %d, want 7", sz)
	}
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
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	done()

	if err := a.Truncate(4); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	if a.Offset() != 4 {
		t.Fatalf("offset after truncate %d, want 4", a.Offset())
	}
	sz, _ := a.Size()
	if sz != 4 {
		t.Fatalf("size after truncate %d, want 4", sz)
	}

	n, err := a.ReadAt([]byte("0123456789"), 0)
	if err != nil && err.Error() != "EOF" {
		t.Fatalf("ReadAt truncated region: %v", err)
	}
	if n != 4 {
		t.Fatalf("read %d, want 4", n)
	}
}

func TestTruncateGrowsOffsetBack(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()

	if err := a.Truncate(100); err != nil {
		t.Fatalf("Truncate grows file: %v", err)
	}
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
	if err := a.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
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
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !Exists(p) {
		t.Fatal("Exists false for existing file")
	}
}

func TestCreateSingle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.rpk")

	if err := CreateSingle(path, []byte("HEADER")); err != nil {
		t.Fatalf("CreateSingle: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "HEADER" {
		t.Fatalf("header persisted %q, want %q", b, "HEADER")
	}

	if err := CreateSingle(path, []byte("X")); err == nil {
		t.Fatal("second CreateSingle on existing file succeeded")
	}
}

func TestCreateSingleUnwritableDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "store.rpk")
	if err := CreateSingle(path, []byte("HEADER")); err == nil {
		t.Fatal("CreateSingle in missing dir succeeded")
	}
	if Exists(path) {
		t.Fatal("partial file left behind after failed create")
	}
}

func TestAppenderDoubleClose(t *testing.T) {
	_, a := newTestAppender(t)
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	a.Close()
}
