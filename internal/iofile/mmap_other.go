//go:build !unix

package iofile

import (
	"fmt"
	"os"
)

// readMapper is the ReadAt fallback for platforms without mmap support
// (e.g. Windows): every view is a fresh copy, so aliasing degrades to the
// plain ReadAt path and no mapping is held.
type readMapper struct {
	f *os.File
}

func newReadMapper(f *os.File) *readMapper {
	return &readMapper{f: f}
}

// view returns a fresh ReadAt copy of [offset, offset+n); done is a no-op.
func (m *readMapper) view(offset, n int64) ([]byte, func(), error) {
	if n < 0 || offset < 0 {
		return nil, nil, fmt.Errorf("rowpack: invalid view range [%d,%d)", offset, offset+n)
	}
	b := make([]byte, n)
	if _, err := m.f.ReadAt(b, offset); err != nil {
		return nil, nil, err
	}
	return b, func() {}, nil
}

// unmap is a no-op on platforms without mmap support.
func (m *readMapper) unmap() {}
