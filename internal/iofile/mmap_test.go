package iofile

import (
	"bytes"
	"crypto/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestViewBasicAndGrow exercises views before and after the file grows past
// the current mapping (remap on demand).
func TestViewBasicAndGrow(t *testing.T) {
	path := filepath.Join(tmpdb(t), "v.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()

	_, err = a.Append([]byte("hello world"))
	require.NoError(t, err)
	b, done, err := a.View(0, 5)
	require.NoError(t, err)
	require.Equal(t, "hello", string(b), "view = %q", b)
	done()

	// Grow beyond the mapping created by the first view and read the tail.
	_, err = a.Append([]byte(" rowpack"))
	require.NoError(t, err)
	b, done, err = a.View(12, 4)
	require.NoError(t, err)
	require.Equal(t, "rowp", string(b), "view after grow = %q", b)
	done()

	// The earlier range must still read correctly through the new mapping.
	b, done, err = a.View(6, 5)
	require.NoError(t, err)
	require.Equal(t, "world", string(b), "old range after remap = %q", b)
	done()
}

// TestViewBounds verifies out-of-range views fail.
func TestViewBounds(t *testing.T) {
	path := filepath.Join(tmpdb(t), "b.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()
	_, err = a.Append([]byte("0123456789"))
	require.NoError(t, err)
	_, done, err := a.View(0, 5)
	require.NoError(t, err)
	done()
	_, _, err = a.View(8, 4)
	require.Error(t, err, "view beyond EOF succeeded")
	_, _, err = a.View(-1, 4)
	require.Error(t, err, "negative offset succeeded")
	_, _, err = a.View(0, -1)
	require.Error(t, err, "negative length succeeded")
}

// TestViewAfterTruncate verifies recovery-style truncation invalidates the
// mapping: views inside the new EOF work, beyond it fail.
func TestViewAfterTruncate(t *testing.T) {
	path := filepath.Join(tmpdb(t), "t.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()

	_, err = a.Append([]byte("0123456789abcdef"))
	require.NoError(t, err)
	_, done, err := a.View(0, 16)
	require.NoError(t, err)
	done()
	require.NoError(t, a.Truncate(8))
	b, done, err := a.View(0, 8)
	require.NoError(t, err)
	require.Equal(t, "01234567", string(b), "view after truncate = %q", b)
	done()
	_, _, err = a.View(8, 4)
	require.Error(t, err, "view beyond truncated EOF succeeded")
}

// TestViewConcurrentRemap stresses concurrent views while the file grows;
// run under -race it must not report data races and every view must observe
// bytes written before the view was issued.
func TestViewConcurrentRemap(t *testing.T) {
	path := filepath.Join(tmpdb(t), "c.bin")
	a, err := OpenAppender(path, true)
	require.NoError(t, err)
	defer a.Close()

	var chunk [4096]byte
	const chunks = 64
	for i := 0; i < chunks; i++ {
		if _, err := rand.Read(chunk[:]); err != nil {
			require.NoError(t, err)
		}
		if _, err := a.Append(chunk[:]); err != nil {
			require.NoError(t, err)
		}
	}
	// Fixed address space readers may use; appends after this point only
	// extend the file, never move existing bytes.
	maxOff := int64(chunks) * int64(len(chunk))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Appender goroutine: keeps growing the file (single writer — Append must
	// never be called concurrently).
	wg.Add(1)
	go func() {
		defer wg.Done()
		var more [512]byte
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := rand.Read(more[:]); err != nil {
				assert.NoError(t, err)
				return
			}
			if _, err := a.Append(more[:]); err != nil {
				assert.NoError(t, err)
				return
			}
		}
	}()
	// Reader goroutines: verify views against ReadAt within the stable range.
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				off := int64(i*977) % (maxOff - 300)
				n := int64(1 + i%256)
				b, done, err := a.View(off, n)
				if err != nil {
					assert.NoError(t, err)
					return
				}
				want := make([]byte, n)
				if _, err := a.f.ReadAt(want, off); err != nil {
					assert.NoError(t, err)
					done()
					return
				}
				if !bytes.Equal(b, want) {
					assert.Fail(t, "view at %d mismatched ReadAt", off)
					done()
					return
				}
				done()
			}
		}()
	}
	// Give readers a chance to run while the appender grows the file, then
	// stop it and wait.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
