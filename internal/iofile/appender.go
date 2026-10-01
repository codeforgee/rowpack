// Package iofile provides the append-only file abstraction used by RowPack's
// data and index files, plus the paired-file creation with rollback. Append
// writers maintain their own write offset and never share a seek cursor;
// readers access files through ReadAt so they never disturb writers.
package iofile

import (
	"errors"
	"fmt"
	"os"
)

// Appender wraps an append-only file. It tracks its own write offset and is
// the only writer to the file, matching the single-writer model. Reads go
// through ReadAt or View so they never disturb the write cursor.
type Appender struct {
	f      *os.File
	offset int64
	size   int64
	mapper *readMapper
}

// OpenAppender opens path for read/write appending, creating it when create
// is true (exclusive create is enforced by the caller when needed).
func OpenAppender(path string, create bool) (*Appender, error) {
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Appender{f: f, offset: fi.Size(), size: fi.Size(), mapper: newReadMapper(f)}, nil
}

// View returns a stable view of [offset, offset+n). On platforms with mmap
// support the returned slice aliases an internal read-only mapping of the
// whole file and is valid only until done is called: a racing append may
// trigger a remap, which is serialized against active views. On platforms
// without mmap support it is a fresh ReadAt copy. The caller must not retain
// the slice past done and must not write through it.
func (a *Appender) View(offset, n int64) ([]byte, func(), error) {
	return a.mapper.view(offset, n)
}

// Offset returns the current write offset (bytes appended so far).
func (a *Appender) Offset() int64 { return a.offset }

// Append writes b at the current offset and advances it. It returns the start
// offset. Callers must not call Append concurrently.
func (a *Appender) Append(b []byte) (int64, error) {
	start := a.offset
	n, err := a.f.WriteAt(b, start)
	if err != nil {
		return start, err
	}
	if n != len(b) {
		return start, errors.New("rowpack: short write")
	}
	a.offset += int64(n)
	a.size = a.offset
	return start, nil
}

// AppendZeroes appends n zero bytes (padding), advancing the offset.
func (a *Appender) AppendZeroes(n int) (int64, error) {
	start := a.offset
	block := make([]byte, 8192)
	remaining := n
	for remaining > 0 {
		chunk := block
		if remaining < len(chunk) {
			chunk = chunk[:remaining]
		}
		if _, err := a.f.WriteAt(chunk, a.offset); err != nil {
			return start, err
		}
		a.offset += int64(len(chunk))
		remaining -= len(chunk)
	}
	a.size = a.offset
	return start, nil
}

// Sync flushes the file to stable storage.
func (a *Appender) Sync() error { return a.f.Sync() }

// Truncate cuts the file back to n bytes (used by recovery to drop invalid
// tails). It also rewinds the write offset and drops any read mapping so
// views never cover bytes beyond the new EOF.
func (a *Appender) Truncate(n int64) error {
	if err := a.f.Truncate(n); err != nil {
		return err
	}
	if n < a.offset {
		a.offset = n
	}
	a.size = n
	a.mapper.unmap()
	return nil
}

// ReadAt reads from the file without touching the write offset.
func (a *Appender) ReadAt(b []byte, off int64) (int, error) { return a.f.ReadAt(b, off) }

// Size returns the current file size. The Appender is the file's only writer
// (single-writer model), so the size it tracks across Append/AppendZeroes/
// Truncate is authoritative; the read path (recover, Stats) never needs a
// per-call fstat, and the accessor cannot fail.
func (a *Appender) Size() int64 { return a.size }

// Close unmaps any read view and closes the file.
func (a *Appender) Close() error {
	a.mapper.unmap()
	return a.f.Close()
}

// Exists reports whether path exists.
func Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// CreateSingle creates the single store file with exclusive create, writes
// its header, and syncs it. On any failure it removes the file so a partially
// created store never survives.
// CreateSingle creates the store file holding dataHeader. With truncate the
// call replaces an existing file atomically through O_TRUNC instead of
// failing on it; without it an existing file is an error (O_EXCL).
func CreateSingle(dataPath string, dataHeader []byte, truncate bool) error {
	flags := os.O_RDWR | os.O_CREATE | os.O_EXCL
	if truncate {
		flags = os.O_RDWR | os.O_CREATE | os.O_TRUNC
	}
	df, err := os.OpenFile(dataPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("rowpack: create store file: %w", err)
	}
	remove := func() { os.Remove(dataPath) }
	if _, err := df.Write(dataHeader); err != nil {
		df.Close()
		remove()
		return fmt.Errorf("rowpack: write store header: %w", err)
	}
	if err := df.Sync(); err != nil {
		df.Close()
		remove()
		return fmt.Errorf("rowpack: sync store header: %w", err)
	}
	if err := df.Close(); err != nil {
		remove()
		return fmt.Errorf("rowpack: close store file: %w", err)
	}
	return nil
}
