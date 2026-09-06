package block

import (
	"errors"
	"fmt"
	"io"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Reader reads blocks from a file via ReadAt (no shared seek cursor), so
// concurrent readers never interfere. Decompression is bounded by Limits and
// CRC failures never return a block.
type Reader struct {
	ra      io.ReaderAt
	limits  Limits
	scratch []byte
}

// NewReader creates a block reader over ra.
func NewReader(ra io.ReaderAt, limits Limits) *Reader {
	return &Reader{ra: ra, limits: limits, scratch: make([]byte, 0, 1<<20)}
}

// ReadAtBlock reads, validates and decompresses the block whose header starts
// at offset. It returns the validated block; Raw is the checked uncompressed
// payload.
func (r *Reader) ReadAtBlock(offset int64) (*Block, error) {
	var hdr [fileformat.BlockHeaderSize]byte
	if _, err := r.ra.ReadAt(hdr[:], offset); err != nil {
		return nil, fmt.Errorf("rowpack: read block header at %d: %w", offset, err)
	}
	var h fileformat.BlockHeader
	if err := h.Unmarshal(hdr[:]); err != nil {
		return nil, err
	}
	if h.StoredSize > r.limits.MaxStoredBytes {
		return nil, fmt.Errorf("rowpack: stored size %d exceeds limit %d", h.StoredSize, r.limits.MaxStoredBytes)
	}
	if h.RawSize > r.limits.MaxRawBytes {
		return nil, fmt.Errorf("rowpack: raw size %d exceeds limit %d", h.RawSize, r.limits.MaxRawBytes)
	}
	if h.Compression == fileformat.CompressionNone && h.StoredSize != h.RawSize {
		return nil, fmt.Errorf("rowpack: none-compressed block stored %d != raw %d", h.StoredSize, h.RawSize)
	}
	stored := make([]byte, h.StoredSize)
	if _, err := r.ra.ReadAt(stored, offset+fileformat.BlockHeaderSize); err != nil {
		return nil, fmt.Errorf("rowpack: read block payload at %d: %w", offset+fileformat.BlockHeaderSize, err)
	}
	raw, err := r.decompress(&h, stored)
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) != h.RawSize {
		return nil, fmt.Errorf("rowpack: decompressed %d bytes, want %d", len(raw), h.RawSize)
	}
	if fileformat.CRC32C(raw) != h.RawCRC32C {
		return nil, fmt.Errorf("rowpack: block %d raw CRC mismatch", h.BlockID)
	}
	return &Block{Header: h, Raw: raw}, nil
}

func (r *Reader) decompress(h *fileformat.BlockHeader, stored []byte) ([]byte, error) {
	out, err := Decompress(h.Compression, r.scratch, stored, r.limits.MaxRawBytes)
	if err != nil {
		return nil, fmt.Errorf("rowpack: block %d: %w", h.BlockID, err)
	}
	// Reuse whatever backing the decoder produced, but hand the caller an
	// owned copy so cache eviction and concurrent reads never alias.
	r.scratch = out[:0]
	cp := make([]byte, len(out))
	copy(cp, out)
	return cp, nil
}

// Block is a validated block: header plus checked uncompressed payload.
type Block struct {
	Header fileformat.BlockHeader
	Raw    []byte
}

var _ = errors.New
