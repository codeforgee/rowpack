//go:build unix

package lockfile

import (
	"fmt"
	"os"
	"syscall"
)

// Lock is an advisory cross-process writer lock backed by flock on the store's
// .lock file. The lock is released automatically if the process dies.
type Lock struct {
	f *os.File
}

// Acquire takes an exclusive lock on path (creating it if needed). It blocks
// until the lock is free or returns an error if the platform cannot lock.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("rowpack: open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, fmt.Errorf("rowpack: store is locked by another writer")
		}
		return nil, fmt.Errorf("rowpack: acquire lock: %w", err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}
