package iofile

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppender(t *testing.T) {
	path := filepath.Join(tmpdb(t), "a.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()

	require.Equal(t, int64(0), a.Offset(), "initial offset = %d", a.Offset())
	off, err := a.Append([]byte("hello"))
	require.NoError(t, err, "append 1: %v %d", err, off)
	require.Equal(t, int64(0), off, "append 1: %v %d", err, off)
	off, err = a.Append([]byte("world"))
	require.NoError(t, err, "append 2: %v %d", err, off)
	require.Equal(t, int64(5), off, "append 2: %v %d", err, off)
	require.Equal(t, int64(10), a.Offset(), "offset = %d", a.Offset())
	_, err = a.Append(make([]byte, 6))
	require.NoError(t, err)
	require.Equal(t, int64(16), a.Offset(), "offset after zeroes = %d", a.Offset())
	require.NoError(t, a.Sync())
	buf := make([]byte, 16)
	_, err = a.ReadAt(buf, 0)
	require.NoError(t, err)
	require.Equal(t, "helloworld\x00\x00\x00\x00\x00\x00", string(buf), "read back %q", buf)

	// Reopen without create: offset resumes from file size.
	b, err := OpenAppender(path, false)
	require.NoError(t, err)
	defer b.Close()
	require.Equal(t, int64(16), b.Offset(), "reopen offset = %d", b.Offset())
	off, err = b.Append([]byte("!"))
	require.NoError(t, err, "reopen append: %v %d", err, off)
	require.Equal(t, int64(16), off, "reopen append: %v %d", err, off)

	// Truncate rewinds the offset.
	require.NoError(t, a.Truncate(10))
	require.Equal(t, int64(10), a.Offset(), "offset after truncate = %d", a.Offset())
}

func TestCreatePairRollback(t *testing.T) {
	dir := tmpdb(t)
	data := filepath.Join(dir, "s.rpk")
	idx := filepath.Join(dir, "s.rpi")

	// Index path already exists -> exclusive create fails and data is removed.
	require.NoError(t, os.WriteFile(idx, []byte("x"), 0o644))
	require.Error(t, CreatePair(data, idx, []byte("h1"), []byte("h2")), "CreatePair succeeded with existing index file")
	require.False(t, Exists(data), "data file not rolled back after failed create")
	// Cleanup and create both fresh.
	require.NoError(t, os.Remove(idx))
	require.NoError(t, CreatePair(data, idx, []byte("h1"), []byte("h2")), "CreatePair should succeed")
	d, _ := os.ReadFile(data)
	i, _ := os.ReadFile(idx)
	require.True(t, bytes.Equal(d, []byte("h1")), "headers not written")
	require.True(t, bytes.Equal(i, []byte("h2")), "headers not written")
	// Second create must fail with exclusive create (no overwrite).
	require.Error(t, CreatePair(data, idx, []byte("x"), []byte("y")), "CreatePair overwrote existing files")
}
