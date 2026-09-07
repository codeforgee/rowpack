package iofile

import "sync/atomic"

// noMmapForced latches the ReadAt fallback for all new views. It is a
// test/benchmark-only hook for comparing the mmap and ReadAt I/O paths;
// production code never sets it.
var noMmapForced atomic.Bool

// ForceReadAt forces every new view through the ReadAt fallback path (no
// memory mapping). Existing mappings are left untouched until the next
// remap/unmap, so callers that need a clean comparison must open fresh
// files while the toggle is active.
func ForceReadAt(on bool) { noMmapForced.Store(on) }
