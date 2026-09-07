package iofile

import (
	"bytes"
	"crypto/rand"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestViewBasicAndGrow exercises views before and after the file grows past
// the current mapping (remap on demand).
func TestViewBasicAndGrow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err := a.Append([]byte("hello world")); err != nil {
		t.Fatal(err)
	}
	b, done, err := a.View(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("view = %q", b)
	}
	done()

	// Grow beyond the mapping created by the first view and read the tail.
	if _, err := a.Append([]byte(" rowpack")); err != nil {
		t.Fatal(err)
	}
	b, done, err = a.View(12, 4)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "rowp" {
		t.Fatalf("view after grow = %q", b)
	}
	done()

	// The earlier range must still read correctly through the new mapping.
	b, done, err = a.View(6, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "world" {
		t.Fatalf("old range after remap = %q", b)
	}
	done()
}

// TestViewBounds verifies out-of-range views fail.
func TestViewBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Append([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	_, done, err := a.View(0, 5)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if _, _, err := a.View(8, 4); err == nil {
		t.Fatal("view beyond EOF succeeded")
	}
	if _, _, err := a.View(-1, 4); err == nil {
		t.Fatal("negative offset succeeded")
	}
	if _, _, err := a.View(0, -1); err == nil {
		t.Fatal("negative length succeeded")
	}
}

// TestViewAfterTruncate verifies recovery-style truncation invalidates the
// mapping: views inside the new EOF work, beyond it fail.
func TestViewAfterTruncate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if _, err := a.Append([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	_, done, err := a.View(0, 16)
	if err != nil {
		t.Fatal(err)
	}
	done()
	if err := a.Truncate(8); err != nil {
		t.Fatal(err)
	}
	b, done, err := a.View(0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "01234567" {
		t.Fatalf("view after truncate = %q", b)
	}
	done()
	if _, _, err := a.View(8, 4); err == nil {
		t.Fatal("view beyond truncated EOF succeeded")
	}
}

// TestViewConcurrentRemap stresses concurrent views while the file grows;
// run under -race it must not report data races and every view must observe
// bytes written before the view was issued.
func TestViewConcurrentRemap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.bin")
	a, err := OpenAppender(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	var chunk [4096]byte
	const chunks = 64
	for i := 0; i < chunks; i++ {
		if _, err := rand.Read(chunk[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Append(chunk[:]); err != nil {
			t.Fatal(err)
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
				t.Error(err)
				return
			}
			if _, err := a.Append(more[:]); err != nil {
				t.Error(err)
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
					t.Error(err)
					return
				}
				want := make([]byte, n)
				if _, err := a.f.ReadAt(want, off); err != nil {
					t.Error(err)
					done()
					return
				}
				if !bytes.Equal(b, want) {
					t.Errorf("view at %d mismatched ReadAt", off)
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
