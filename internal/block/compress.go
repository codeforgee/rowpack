package block

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/klauspost/compress/zstd"
)

// Limits bound compressed and decompressed sizes during reads.
type Limits struct {
	MaxRawBytes    uint32
	MaxStoredBytes uint32
}

// DefaultLimits returns the v1 default block safety limits.
func DefaultLimits() Limits {
	return Limits{
		MaxRawBytes:    format.DefaultMaxRawBlockBytes,
		MaxStoredBytes: format.DefaultMaxStoredBlockBytes,
	}
}

// Compress encodes src with the given algorithm. Zstd output is a complete
// independent frame (no dictionary) and is deterministic for a fixed level.
func Compress(alg format.Compression, level int, src []byte) ([]byte, error) {
	switch alg {
	case format.CompressionNone:
		return src, nil
	case format.CompressionZstd:
		return compressZstd(level, src)
	}
	return nil, fmt.Errorf("rowpack: unsupported compression %d", alg)
}

// Decompress decodes src, returning the decoded bytes which never exceed
// maxOut; a larger result is a compression-bomb rejection rather than an
// allocation.
func Decompress(alg format.Compression, dst, src []byte, maxOut uint32) ([]byte, error) {
	switch alg {
	case format.CompressionNone:
		if uint32(len(src)) > maxOut {
			return nil, fmt.Errorf("rowpack: stored size %d exceeds limit %d", len(src), maxOut)
		}
		return src, nil
	case format.CompressionZstd:
		return decompressZstd(dst, src, maxOut)
	}
	return nil, fmt.Errorf("rowpack: unsupported compression %d", alg)
}

// ---- pooled zstd encoder/decoder ----
//
// Creating a zstd encoder/decoder allocates a large histogram (~1 MiB) per
// instance; reusing them via sync.Pool removes that cost from every block
// compress/decompress.

// zstdMaxDecoded is the decoder-level memory ceiling. Precise per-block limits
// are still enforced by the caller's maxOut check.
const zstdMaxDecoded = 512 << 20

var (
	encPoolsMu sync.Mutex
	encPools   = map[int]*sync.Pool{}

	// encodeDstPools holds EncodeAll destination scratch, graded by size like
	// rawBufPools. Grading bounds the cost of a pool miss: GC clears sync.Pool
	// every cycle, so one 1 MiB buffer was re-made per 32 KiB page flush.
	encodeDstPools [poolNumClasses]sync.Pool
	encodeDstBytes atomic.Int64 // parked in encodeDstPools, capped by encodeDstBudget

	decPool = sync.Pool{New: func() any {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderMaxMemory(zstdMaxDecoded),
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderConcurrency(1))
		if err != nil {
			// NewReader only fails on invalid options; none are invalid here.
			panic(err)
		}
		return d
	}}
)

// poolForLevel returns the encoder pool for a zstd level.
func poolForLevel(level int) *sync.Pool {
	encPoolsMu.Lock()
	defer encPoolsMu.Unlock()
	if p := encPools[level]; p != nil {
		return p
	}
	p := &sync.Pool{New: func() any {
		// Single-goroutine, CRC-enabled encoding for deterministic output.
		e, err := zstd.NewWriter(nil,
			zstd.WithEncoderConcurrency(1),
			zstd.WithEncoderCRC(true),
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
		if err != nil {
			panic(err)
		}
		return e
	}}
	encPools[level] = p
	return p
}

// ZstdEncoder is the reusable zstd encoder handle (alias of klauspost's
// zstd.Encoder) for store-owned encoders passed to block builders.
type ZstdEncoder = zstd.Encoder

// NewZstdEncoder builds a reusable zstd encoder configured like the pooled
// ones (single-goroutine, CRC-enabled deterministic output). Callers that own
// a store-level encoder (one writer per store) pass it to block builders via
// SetZstdEncoder, which keeps the ~1 MiB histogram alive across block flushes
// even when GC cycles clear the sync.Pool. The encoder is safe for EncodeAll
// from multiple goroutines but serializes internally; block flushing is
// single-threaded per writer anyway. Returned to the caller; Close it when
// the store closes.
func NewZstdEncoder(level int) *ZstdEncoder {
	e, err := zstd.NewWriter(nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(level)))
	if err != nil {
		panic(err)
	}
	return e
}

// encodeDstBudget caps the bytes parked in encodeDstPools.
const encodeDstBudget = 8 << 20

func init() {
	for i := range encodeDstPools {
		size := 1 << (poolClassMinBits + i)
		p := &encodeDstPools[i]
		p.New = func() any { b := make([]byte, 0, size); return &b }
	}
}

// getEncodeDst returns an EncodeAll scratch with capacity for at least n
// bytes: getRawBuf's grading, but a separate pool and budget so read and write
// scratch never compete. Oversized requests bypass the pool.
func getEncodeDst(n int) []byte {
	c := poolClass(n)
	if c < 0 {
		return make([]byte, 0, n)
	}
	b := *encodeDstPools[c-poolClassMinBits].Get().(*[]byte)
	if cap(b) > 0 {
		encodeDstBytes.Add(-int64(cap(b)))
	}
	if cap(b) < n {
		b = make([]byte, 0, n)
	}
	return b[:0]
}

// putEncodeDst files a scratch buffer back under the class of its capacity.
func putEncodeDst(b []byte) {
	if b == nil {
		return
	}
	c := poolClass(cap(b))
	if c < 0 {
		return // oversized: never retained
	}
	if encodeDstBytes.Add(int64(cap(b))) > encodeDstBudget {
		encodeDstBytes.Add(-int64(cap(b)))
		return // budget exhausted: drop to GC
	}
	b = b[:0]
	encodeDstPools[c-poolClassMinBits].Put(&b)
}

// encodeZstdWith compresses src with a caller-owned encoder, returning a
// freshly allocated frame (like Compress). Callers that compress many buffers
// in a row should prefer EncodeZstdInto with a scratch they keep.
func encodeZstdWith(enc *ZstdEncoder, src []byte) ([]byte, error) {
	dst := getEncodeDst(len(src))
	out := enc.EncodeAll(src, dst[:0])
	res := make([]byte, len(out))
	copy(res, out)
	if cap(out) == cap(dst) {
		putEncodeDst(out[:0])
	} else {
		// Outgrew the scratch: pool the class buffer, drop the larger array.
		putEncodeDst(dst)
	}
	return res, nil
}

// EncodeZstdInto is encodeZstdWith with a caller-owned EncodeAll scratch,
// reused across calls through *scratch; the stored frame is still freshly
// allocated for the caller. Builders flush single-threaded owning one scratch
// each, mirroring the store-owned encoder (NewZstdEncoder).
func EncodeZstdInto(enc *ZstdEncoder, scratch *[]byte, src []byte) ([]byte, error) {
	dst := *scratch
	if cap(dst) < len(src) {
		dst = make([]byte, 0, len(src))
	}
	out := enc.EncodeAll(src, dst[:0])
	res := make([]byte, len(out))
	copy(res, out)
	if cap(out) >= cap(dst) {
		*scratch = out[:0]
	} else {
		*scratch = dst[:0]
	}
	return res, nil
}

func compressZstd(level int, src []byte) ([]byte, error) {
	pool := poolForLevel(level)
	enc := pool.Get().(*zstd.Encoder)
	res, err := encodeZstdWith(enc, src)
	pool.Put(enc)
	return res, err
}

func decompressZstd(dst, src []byte, maxOut uint32) ([]byte, error) {
	dec := decPool.Get().(*zstd.Decoder)
	out, err := dec.DecodeAll(src, dst[:0])
	decPool.Put(dec)
	if err != nil {
		return nil, fmt.Errorf("rowpack: zstd decode: %w", err)
	}
	if uint32(len(out)) > maxOut {
		return nil, fmt.Errorf("rowpack: decompressed %d bytes exceeds limit %d", len(out), maxOut)
	}
	return out, nil
}
