package block

import "sync"

// rawBuf is a pooled decompression scratch buffer. Buffers are sized per
// block raw size on first use and reused by later blocks of the same or
// smaller raw size; out-sized buffers are re-pooled as-is. sync.Pool clears
// idle entries during GC, so the pool only retains what concurrent scans
// are actually holding.
type rawBuf struct {
	data []byte
}

var rawBufPool = sync.Pool{New: func() any { return &rawBuf{} }}

// getRawBuf returns a scratch buffer with capacity for rawSize bytes.
func getRawBuf(rawSize uint32) *rawBuf {
	b := rawBufPool.Get().(*rawBuf)
	if cap(b.data) < int(rawSize) {
		// Grow to the requested raw size (header-validated by the caller).
		b.data = make([]byte, int(rawSize))
	}
	return b
}

func putRawBuf(b *rawBuf) {
	if b == nil {
		return
	}
	rawBufPool.Put(b)
}
