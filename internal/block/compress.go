package block

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// Limits bound compressed and decompressed sizes during reads.
type Limits struct {
	MaxRawBytes    uint32
	MaxStoredBytes uint32
}

// DefaultLimits returns the v1 default block safety limits.
func DefaultLimits() Limits {
	return Limits{
		MaxRawBytes:    fileformat.DefaultMaxRawBlockBytes,
		MaxStoredBytes: fileformat.DefaultMaxStoredBlockBytes,
	}
}

// Compress encodes src with the given algorithm. Zstd output is a complete
// independent frame (no dictionary) and is deterministic for a fixed level.
func Compress(alg fileformat.Compression, level int, src []byte) ([]byte, error) {
	switch alg {
	case fileformat.CompressionNone:
		return src, nil
	case fileformat.CompressionZstd:
		return compressZstd(level, src)
	}
	return nil, fmt.Errorf("rowpack: unsupported compression %d", alg)
}

// Decompress decodes src, returning the decoded bytes which never exceed
// maxOut; a larger result is a compression-bomb rejection rather than an
// allocation.
func Decompress(alg fileformat.Compression, dst, src []byte, maxOut uint32) ([]byte, error) {
	switch alg {
	case fileformat.CompressionNone:
		if uint32(len(src)) > maxOut {
			return nil, fmt.Errorf("rowpack: stored size %d exceeds limit %d", len(src), maxOut)
		}
		return src, nil
	case fileformat.CompressionZstd:
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

	// encodeDstPool holds output scratch buffers for EncodeAll. klauspost's
	// EncodeAll pre-allocates a make([]byte, 0, len(src)) destination when the
	// caller passes nil; passing our own large buffer avoids that per-block
	// ~256 KiB allocation.
	encodeDstPool = sync.Pool{New: func() any {
		return make([]byte, 0, 1<<20) // 1 MiB scratch, plenty for any block
	}}

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

// EncodeZstdWith compresses src with a caller-owned encoder, returning a
// freshly allocated frame (like Compress). The pool scratch is used for the
// output and returned before copying, so enc can be reused immediately.
func EncodeZstdWith(enc *ZstdEncoder, src []byte) ([]byte, error) {
	dst := encodeDstPool.Get().([]byte)
	out := enc.EncodeAll(src, dst[:0])
	res := make([]byte, len(out))
	copy(res, out)
	encodeDstPool.Put(dst)
	return res, nil
}

func compressZstd(level int, src []byte) ([]byte, error) {
	pool := poolForLevel(level)
	enc := pool.Get().(*zstd.Encoder)
	res, err := EncodeZstdWith(enc, src)
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
