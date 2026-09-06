package iofile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAppender(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if a.Offset() != 0 {
		t.Fatalf("initial offset = %d", a.Offset())
	}
	off, err := a.Append([]byte("hello"))
	if err != nil || off != 0 {
		t.Fatalf("append 1: %v %d", err, off)
	}
	off, err = a.Append([]byte("world"))
	if err != nil || off != 5 {
		t.Fatalf("append 2: %v %d", err, off)
	}
	if a.Offset() != 10 {
		t.Fatalf("offset = %d", a.Offset())
	}
	if _, err := a.AppendZeroes(6); err != nil {
		t.Fatal(err)
	}
	if a.Offset() != 16 {
		t.Fatalf("offset after zeroes = %d", a.Offset())
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if _, err := a.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, []byte("helloworld\x00\x00\x00\x00\x00\x00")) {
		t.Fatalf("read back %q", buf)
	}

	// Reopen without create: offset resumes from file size.
	b, err := OpenAppender(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.Offset() != 16 {
		t.Fatalf("reopen offset = %d", b.Offset())
	}
	off, err = b.Append([]byte("!"))
	if err != nil || off != 16 {
		t.Fatalf("reopen append: %v %d", err, off)
	}

	// Truncate rewinds the offset.
	if err := a.Truncate(10); err != nil {
		t.Fatal(err)
	}
	if a.Offset() != 10 {
		t.Fatalf("offset after truncate = %d", a.Offset())
	}
}

func TestCreatePairRollback(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "s.rpk")
	idx := filepath.Join(dir, "s.rpi")

	// Index path already exists -> exclusive create fails and data is removed.
	if err := os.WriteFile(idx, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CreatePair(data, idx, []byte("h1"), []byte("h2")); err == nil {
		t.Fatal("CreatePair succeeded with existing index file")
	}
	if Exists(data) {
		t.Fatal("data file not rolled back after failed create")
	}
	// Cleanup and create both fresh.
	if err := os.Remove(idx); err != nil {
		t.Fatal(err)
	}
	if err := CreatePair(data, idx, []byte("h1"), []byte("h2")); err != nil {
		t.Fatalf("CreatePair: %v", err)
	}
	d, _ := os.ReadFile(data)
	i, _ := os.ReadFile(idx)
	if !bytes.Equal(d, []byte("h1")) || !bytes.Equal(i, []byte("h2")) {
		t.Fatal("headers not written")
	}
	// Second create must fail with exclusive create (no overwrite).
	if err := CreatePair(data, idx, []byte("x"), []byte("y")); err == nil {
		t.Fatal("CreatePair overwrote existing files")
	}
}
