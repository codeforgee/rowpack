//go:build unix

package iofile

import (
	"fmt"
	"os"
	"sync"
	"syscall"
)

// readMapper exposes read-only views into a file through mmap. A single
// mapping covers the whole file; when a view reaches past the current mapping
// (the append-only file has grown), the mapping is replaced under the write
// lock. This is safe because views are only valid while the caller has not
// called done, so no reader can hold a view across a remap.
type readMapper struct {
	mu sync.RWMutex
	f  *os.File

	data []byte // current mapping (nil = not mapped)
	size int64  // mapped size

	// disabled latches to true when mmap fails (e.g. mapping-size limits on
	// 32-bit platforms or hardened sandboxes); all views then degrade to
	// ReadAt copies, matching the pre-mmap behavior.
	disabled bool
}

func newReadMapper(f *os.File) *readMapper {
	return &readMapper{f: f}
}

// view returns a slice of [offset, offset+n), remapping on demand. The
// returned slice is valid until done is called.
func (m *readMapper) view(offset, n int64) ([]byte, func(), error) {
	if n < 0 || offset < 0 {
		return nil, nil, fmt.Errorf("rowpack: invalid view range [%d,%d)", offset, offset+n)
	}
	for {
		m.mu.RLock()
		if m.data != nil && offset+n <= m.size {
			b := m.data[offset : offset+n : offset+n]
			return b, m.mu.RUnlock, nil
		}
		m.mu.RUnlock()

		// (Re)map under the write lock. Readers may not hold views across a
		// remap, so munmap of the previous mapping is safe here.
		m.mu.Lock()
		if m.data != nil && offset+n <= m.size {
			// Covered by a racing remap; retry the read path.
			m.mu.Unlock()
			continue
		}
		fi, err := m.f.Stat()
		if err != nil {
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("rowpack: stat file for view: %w", err)
		}
		if offset+n > fi.Size() {
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("rowpack: view [%d,%d) beyond file size %d", offset, offset+n, fi.Size())
		}
		if noMmapForced.Load() || m.disabled || fi.Size() == 0 {
			m.mu.Unlock()
			return m.fallbackView(offset, n)
		}
		data, err := syscall.Mmap(int(m.f.Fd()), 0, int(fi.Size()), syscall.PROT_READ, syscall.MAP_SHARED)
		if err != nil {
			// Degrade permanently to ReadAt copies rather than failing every
			// subsequent read on constrained systems.
			m.disabled = true
			m.mu.Unlock()
			return m.fallbackView(offset, n)
		}
		if m.data != nil {
			_ = syscall.Munmap(m.data)
		}
		m.data = data
		m.size = fi.Size()
		m.mu.Unlock()
	}
}

// fallbackView serves one view as a fresh ReadAt copy (no mapping held).
func (m *readMapper) fallbackView(offset, n int64) ([]byte, func(), error) {
	b := make([]byte, n)
	if _, err := m.f.ReadAt(b, offset); err != nil {
		return nil, nil, err
	}
	return b, func() {}, nil
}

// unmap drops the current mapping. Called on Truncate (views must never cover
// bytes beyond the new EOF: accessing them would be SIGBUS) and on Close.
func (m *readMapper) unmap() {
	m.mu.Lock()
	if m.data != nil {
		_ = syscall.Munmap(m.data)
		m.data = nil
		m.size = 0
	}
	m.mu.Unlock()
}
