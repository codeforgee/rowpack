package iofile

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppenderReadAllSizeFile(t *testing.T) {
	path := filepath.Join(tmpdb(t), "a.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()

	_, err = a.Append([]byte("hello"))
	require.NoError(t, err)
	// Multi-chunk write path (previously AppendZeroes' 8 KiB chunking).
	_, err = a.Append(make([]byte, 20000))
	require.NoError(t, err)
	size, err := a.Size()
	require.NoError(t, err)
	require.Equal(t, int64(20005), size, "Size = %d, want 20005", size)
	require.Equal(t, size, a.Offset(), "Offset %d != Size %d", a.Offset(), size)
	all, err := a.ReadAll()
	require.NoError(t, err)
	require.Equal(t, int(size), len(all), "ReadAll len=%d head=%q", len(all), all[:5])
	require.True(t, bytes.Equal(all[:5], []byte("hello")), "ReadAll len=%d head=%q", len(all), all[:5])
	for i, b := range all[5:] {
		require.Zero(t, b, "padding byte %d = %d", i, b)
	}
	require.NotNil(t, a.File(), "File() returned nil")
}

func TestViewCopyInvalid(t *testing.T) {
	path := filepath.Join(tmpdb(t), "a.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Append([]byte("hello"))
	require.NoError(t, err)
	// Negative ranges.
	for _, r := range [][2]int64{{-1, 2}, {0, -2}, {2, -4}} {
		_, _, err := a.viewCopy(r[0], r[1])
		require.Error(t, err, "viewCopy(%d,%d) accepted", r[0], r[1])
	}
	// Out of range.
	_, _, err = a.viewCopy(0, 6)
	require.Error(t, err, "viewCopy past EOF accepted")
	// Valid copy is caller-owned (not aliased to the mapping).
	b, done, err := a.viewCopy(0, 5)
	require.NoError(t, err)
	done()
	require.Equal(t, "hello", string(b), "viewCopy = %q", b)
}

func TestViewBeyondEOFAndRemap(t *testing.T) {
	path := filepath.Join(tmpdb(t), "a.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()

	_, err = a.Append([]byte("hello"))
	require.NoError(t, err)
	// View past EOF must fail (before any mapping exists).
	_, _, err = a.View(0, 6)
	require.Error(t, err, "View past EOF accepted")
	// Append forces a remap covering the new bytes.
	_, err = a.Append([]byte(" world"))
	require.NoError(t, err)
	b, done, err := a.View(0, 11)
	require.NoError(t, err)
	require.Equal(t, "hello world", string(b), "View after remap = %q", b)
	done()
	// Truncate drops the mapping: a view past the new EOF must fail, never
	// SIGBUS.
	require.NoError(t, a.Truncate(5))
	_, _, err = a.View(0, 6)
	require.Error(t, err, "View past truncated EOF accepted")
	b, done, err = a.View(0, 5)
	require.NoError(t, err, "View after truncate = %q, %v", b, err)
	require.Equal(t, "hello", string(b), "View after truncate = %q, %v", b, err)
	done()
}

func TestFallbackViewWhenMmapDisabled(t *testing.T) {
	path := filepath.Join(tmpdb(t), "a.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Append([]byte("hello world"))
	require.NoError(t, err)
	// Force the ReadAt-copy fallback (sandboxed/32-bit degradation path).
	a.mapper.mu.Lock()
	a.mapper.disabled = true
	a.mapper.mu.Unlock()
	a.mapper.unmap()

	b, done, err := a.View(6, 5)
	require.NoError(t, err)
	require.Equal(t, "world", string(b), "fallback view = %q", b)
	done() // fallback done is a no-op; safe to call
	// fallbackView out of range surfaces the ReadAt error.
	_, _, err = a.mapper.fallbackView(0, 100)
	require.Error(t, err, "fallbackView past EOF accepted")
}
