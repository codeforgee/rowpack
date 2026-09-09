package block

import (
	"os"
	"sync"
)

// rawBuf is a pooled decompression scratch buffer. Buffers are sized per
// block raw size on first use and reused by later blocks of the same or
// smaller raw size; out-sized buffers are re-pooled as-is. sync.Pool clears
// idle entries during GC, so the pool only retains what concurrent scans
// are actually holding.
type rawBuf struct {
	data []byte
}

var rawBufPool = sync.Pool{New: func() any { return &rawBuf{} }}

// noPool bypasses the pooled scratch entirely: every transient read
// allocates a fresh buffer and Release drops it to the GC. This is
// measurement instrumentation (S0 baseline): benchmarks use it to report the
// true per-read temporary allocation that the page-format refactor must cut.
// It is process-wide, read once from ROWPACK_NOPOOL=1 at init, and can be
// flipped at runtime via SetPoolDisabled (benchmarks run sequentially).
var noPool = os.Getenv("ROWPACK_NOPOOL") == "1"

// SetPoolDisabled forces the pool bypass on or off for the whole process and
// returns the previous value so callers can restore it. It must not be
// flipped while reads are in flight on other goroutines.
func SetPoolDisabled(v bool) (prev bool) {
	prev = noPool
	noPool = v
	return prev
}

// getRawBuf returns a scratch buffer with capacity for rawSize bytes.
func getRawBuf(rawSize uint32) *rawBuf {
	if noPool {
		return &rawBuf{data: make([]byte, rawSize)}
	}
	b := rawBufPool.Get().(*rawBuf)
	if cap(b.data) < int(rawSize) {
		// Grow to the requested raw size (header-validated by the caller).
		b.data = make([]byte, int(rawSize))
	}
	return b
}

func putRawBuf(b *rawBuf) {
	if b == nil || noPool {
		return
	}
	rawBufPool.Put(b)
}
