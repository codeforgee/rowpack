//go:build windows

package lockfile

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx   = kernel32.NewProc("LockFileEx")
	unlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// Lock is a cross-process writer lock. On platforms without flock support the
// lock is backed by the Windows LockFileEx API.
type Lock struct {
	f  *os.File
	ov syscall.Overlapped
}

// Acquire takes an exclusive, non-blocking lock on path. The lock file is
// intentionally never removed (see lock_unix.go Acquire for the inode-reuse
// race this avoids).
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("rowpack: open lock file: %w", err)
	}
	lock := &Lock{f: f}
	r, _, callErr := lockFileEx.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0,
		^uintptr(0),
		^uintptr(0),
		uintptr(unsafe.Pointer(&lock.ov)),
	)
	if r == 0 {
		_ = f.Close()
		if callErr == errorLockViolation {
			return nil, fmt.Errorf("rowpack: store is locked by another writer")
		}
		return nil, fmt.Errorf("rowpack: acquire lock: %w", callErr)
	}
	return lock, nil
}

// Release drops the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	r, _, callErr := unlockFileEx.Call(
		l.f.Fd(),
		0,
		^uintptr(0),
		^uintptr(0),
		uintptr(unsafe.Pointer(&l.ov)),
	)
	cerr := l.f.Close()
	l.f = nil
	if r == 0 {
		return fmt.Errorf("rowpack: release lock: %w", callErr)
	}
	return cerr
}
