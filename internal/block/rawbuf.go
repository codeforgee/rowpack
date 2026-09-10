package block

import (
	"math/bits"
	"os"
	"sync"
	"sync/atomic"
)

// rawBuf is a pooled decompression scratch buffer. Buffers are sized per
// block raw size on first use and reused by later blocks of the same or
// smaller raw size; out-sized buffers are re-pooled as-is. sync.Pool clears
// idle entries during GC, so the pool only retains what concurrent scans
// are actually holding.
type rawBuf struct {
	data []byte
}

// The pool is size-graded by power-of-two classes (4 KiB .. 32 MiB): a
// buffer returned to the pool is filed under the class of its capacity, and
// a Get serves from the class of the requested size. Grading bounds the
// retention waste per buffer at 2x (a 4 KiB workload no longer parks a
// 1 MiB buffer left over from one large block) and keeps allocation sizes
// predictable. Oversized buffers (a single scratch larger than poolBudget)
// never enter the pool at all, so one huge block cannot dominate retention.
const (
	poolClassMinBits = 12 // smallest pooled class: 4 KiB
	poolClassMaxBits = 25 // largest pooled class: 32 MiB
	poolNumClasses   = poolClassMaxBits - poolClassMinBits + 1
	poolBudget       = 32 << 20 // process-wide retained-bytes cap
)

var (
	rawBufPools [poolNumClasses]sync.Pool
	pooledBytes atomic.Int64 // bytes currently sitting in the pools
)

func init() {
	for i := range rawBufPools {
		p := &rawBufPools[i]
		p.New = func() any { return &rawBuf{} }
	}
}

// poolClass maps a byte size to its power-of-two class index, or -1 when the
// size exceeds the largest pooled class. Sizes <= 0 clamp to the smallest
// class.
func poolClass(size int) int {
	if size <= 0 {
		size = 1
	}
	bits := 64 - bits.LeadingZeros(uint(size-1)) // ceil(log2(size))
	switch {
	case bits < poolClassMinBits:
		return poolClassMinBits
	case bits > poolClassMaxBits:
		return -1
	default:
		return bits
	}
}

// noPool bypasses the pooled scratch entirely: every transient read
// allocates a fresh buffer and Release drops it to the GC. This is
// measurement instrumentation (S0 baseline): benchmarks use it to report the
// true per-read temporary allocation that the page-format refactor must cut.
// It is process-wide, read once from ROWPACK_NOPOOL=1 at init, and can be
// flipped at runtime via DisablePool (benchmarks run sequentially).
var noPool = os.Getenv("ROWPACK_NOPOOL") == "1"

// DisablePool forces the pool bypass on or off for the whole process and
// returns the previous value so callers can restore it. It must not be
// flipped while reads are in flight on other goroutines.
func DisablePool(v bool) (prev bool) {
	prev = noPool
	noPool = v
	return prev
}

// getRawBuf returns a scratch buffer with capacity for rawSize bytes.
func getRawBuf(rawSize uint32) *rawBuf {
	if noPool {
		return &rawBuf{data: make([]byte, rawSize)}
	}
	need := int(rawSize)
	c := poolClass(need)
	if c < 0 {
		// Larger than any pooled class (huge explicit BlockSize): fresh
		// allocation, never retained.
		return &rawBuf{data: make([]byte, need)}
	}
	b := rawBufPools[c-poolClassMinBits].Get().(*rawBuf)
	if cap(b.data) > 0 {
		pooledBytes.Add(-int64(cap(b.data)))
	}
	if cap(b.data) < need {
		// A class buffer can still be smaller than the request (request in
		// the upper half of the class); grow and drop the undersized one.
		b.data = make([]byte, need)
	}
	return b
}

func putRawBuf(b *rawBuf) {
	if b == nil || noPool {
		return
	}
	c := poolClass(cap(b.data))
	if c < 0 {
		return // oversized: never retained
	}
	if pooledBytes.Add(int64(cap(b.data))) > poolBudget {
		pooledBytes.Add(-int64(cap(b.data)))
		return // budget exhausted: drop to GC
	}
	rawBufPools[c-poolClassMinBits].Put(b)
}
