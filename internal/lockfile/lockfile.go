package lockfile

import "errors"

// ErrUnsupportedLocking is returned when the platform provides no supported
// cross-process locking primitive.
var ErrUnsupportedLocking = errors.New("rowpack: file locking not supported on this platform")
