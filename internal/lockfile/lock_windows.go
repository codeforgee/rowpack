//go:build windows

package lockfile

import (
	"fmt"
	"os"
)

// Lock is a cross-process writer lock. On platforms without flock support the
// lock is reported as unsupported rather than silently degraded.
type Lock struct{}

// Acquire reports ErrUnsupportedLocking on platforms without a supported
// locking primitive.
func Acquire(path string) (*Lock, error) {
	_ = os.Stat(path)
	return nil, ErrUnsupportedLocking
}

// Release is a no-op for the unsupported lock.
func (l *Lock) Release() error { return nil }
