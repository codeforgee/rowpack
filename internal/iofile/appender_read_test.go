package iofile

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestAppenderReadAllSizeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err := a.Append([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AppendZeroes(20000); err != nil { // multi-chunk padding path
		t.Fatal(err)
	}
	size, err := a.Size()
	if err != nil {
		t.Fatal(err)
	}
	if size != 20005 {
		t.Fatalf("Size = %d, want 20005", size)
	}
	if a.Offset() != size {
		t.Fatalf("Offset %d != Size %d", a.Offset(), size)
	}
	all, err := a.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != int(size) || !bytes.Equal(all[:5], []byte("hello")) {
		t.Fatalf("ReadAll len=%d head=%q", len(all), all[:5])
	}
	for i, b := range all[5:] {
		if b != 0 {
			t.Fatalf("padding byte %d = %d", i, b)
		}
	}
	if a.File() == nil {
		t.Fatal("File() returned nil")
	}
}

func TestViewCopyInvalid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Append([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// Negative ranges.
	for _, r := range [][2]int64{{-1, 2}, {0, -2}, {2, -4}} {
		if _, _, err := a.viewCopy(r[0], r[1]); err == nil {
			t.Fatalf("viewCopy(%d,%d) accepted", r[0], r[1])
		}
	}
	// Out of range.
	if _, _, err := a.viewCopy(0, 6); err == nil {
		t.Fatal("viewCopy past EOF accepted")
	}
	// Valid copy is caller-owned (not aliased to the mapping).
	b, done, err := a.viewCopy(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if string(b) != "hello" {
		t.Fatalf("viewCopy = %q", b)
	}
}

func TestViewBeyondEOFAndRemap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err := a.Append([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	// View past EOF must fail (before any mapping exists).
	if _, _, err := a.View(0, 6); err == nil {
		t.Fatal("View past EOF accepted")
	}
	// Append forces a remap covering the new bytes.
	if _, err := a.Append([]byte(" world")); err != nil {
		t.Fatal(err)
	}
	b, done, err := a.View(0, 11)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello world" {
		t.Fatalf("View after remap = %q", b)
	}
	done()
	// Truncate drops the mapping: a view past the new EOF must fail, never
	// SIGBUS.
	if err := a.Truncate(5); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.View(0, 6); err == nil {
		t.Fatal("View past truncated EOF accepted")
	}
	b, done, err = a.View(0, 5)
	if err != nil || string(b) != "hello" {
		t.Fatalf("View after truncate = %q, %v", b, err)
	}
	done()
}

func TestFallbackViewWhenMmapDisabled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Append([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	// Force the ReadAt-copy fallback (sandboxed/32-bit degradation path).
	a.mapper.mu.Lock()
	a.mapper.disabled = true
	a.mapper.mu.Unlock()
	a.mapper.unmap()

	b, done, err := a.View(6, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "world" {
		t.Fatalf("fallback view = %q", b)
	}
	done() // fallback done is a no-op; safe to call
	// fallbackView out of range surfaces the ReadAt error.
	if _, _, err := a.mapper.fallbackView(0, 100); err == nil {
		t.Fatal("fallbackView past EOF accepted")
	}
}
