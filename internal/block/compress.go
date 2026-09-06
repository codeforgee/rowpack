// Package block implements v1 block building, Zstd/None compression and
// bounded block reading. A Block is the unit of compression and CRC: it
// belongs to exactly one snapshot and one table, and its payload holds an
// offset directory plus record bytes.
package block

import (
	"errors"
	"fmt"

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

// Decompress decodes src into dst (reused scratch). It returns the decoded
// bytes which never exceed maxOut; a larger result is a compression-bomb
// rejection rather than an allocation.
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

var zstdOpts = []zstd.EOption{
	zstd.WithEncoderConcurrency(1), // deterministic output
	zstd.WithEncoderCRC(true),
}

func compressZstd(level int, src []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstdOpts...)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(src, nil), nil
}

func decompressZstd(dst, src []byte, maxOut uint32) ([]byte, error) {
	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderMaxMemory(uint64(maxOut)),
		zstd.WithDecoderLowmem(true))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	out, err := dec.DecodeAll(src, dst[:0])
	if err != nil {
		return nil, fmt.Errorf("rowpack: zstd decode: %w", err)
	}
	if uint32(len(out)) > maxOut {
		return nil, fmt.Errorf("rowpack: decompressed %d bytes exceeds limit %d", len(out), maxOut)
	}
	return out, nil
}

var errShortBlock = errors.New("rowpack: block payload shorter than declared")
