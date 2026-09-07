package block

import (
	"errors"
	"fmt"
	"io"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Reader reads blocks from a file via ReadAt (no shared seek cursor), so
// concurrent readers never interfere. When the underlying handle supports
// zero-copy views (mmap-backed Appender), ReadAtBlock slices the stored
// payload directly out of the mapping and decompresses into a fresh buffer.
// It is stateless and safe for concurrent use; decompression allocates per
// call. Decompression is bounded by Limits and CRC failures never return a
// block.
type Reader struct {
	ra     io.ReaderAt
	limits Limits
}

// NewReader creates a block reader over ra.
func NewReader(ra io.ReaderAt, limits Limits) *Reader {
	return &Reader{ra: ra, limits: limits}
}

// viewer is an optional interface for handles that can expose direct views
// into the file (mmap-backed appenders). Views are transient: valid only
// until done is called.
type viewer interface {
	View(offset, n int64) (b []byte, done func(), err error)
}

// ReadAtBlock reads, validates and decompresses the block whose header starts
// at offset. It returns the validated block; Raw is the checked uncompressed
// payload and never aliases a file mapping.
func (r *Reader) ReadAtBlock(offset int64) (*Block, error) {
	if v, ok := r.ra.(viewer); ok {
		return r.readAtBlockView(offset, v)
	}
	return r.readAtBlockCopy(offset)
}

// readAtBlockView is the zero-copy path: header and stored payload are sliced
// out of the handle's mapping. The header view must be released before the
// payload view is taken (a remap between the two needs the write lock).
func (r *Reader) readAtBlockView(offset int64, v viewer) (*Block, error) {
	hb, hdone, err := v.View(offset, fileformat.BlockHeaderSize)
	if err != nil {
		return nil, fmt.Errorf("rowpack: read block header at %d: %w", offset, err)
	}
	var h fileformat.BlockHeader
	herr := h.Unmarshal(hb)
	hdone()
	if herr != nil {
		return nil, herr
	}
	if err := r.checkHeader(&h); err != nil {
		return nil, err
	}
	sb, sdone, err := v.View(offset+fileformat.BlockHeaderSize, int64(h.StoredSize))
	if err != nil {
		return nil, fmt.Errorf("rowpack: read block payload at %d: %w", offset+fileformat.BlockHeaderSize, err)
	}
	defer sdone()
	raw, err := r.decompress(&h, sb)
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) != h.RawSize {
		return nil, fmt.Errorf("rowpack: decompressed %d bytes, want %d", len(raw), h.RawSize)
	}
	if fileformat.CRC32C(raw) != h.RawCRC32C {
		return nil, fmt.Errorf("rowpack: block %d raw CRC mismatch", h.BlockID)
	}
	if h.Compression == fileformat.CompressionNone {
		// Decompress returned the view itself; copy so the returned (and
		// potentially cached) Block never aliases the file mapping.
		cp := make([]byte, len(raw))
		copy(cp, raw)
		raw = cp
	}
	return &Block{Header: h, Raw: raw}, nil
}

// checkHeader validates the stored/raw limits and None-size agreement.
func (r *Reader) checkHeader(h *fileformat.BlockHeader) error {
	if h.StoredSize > r.limits.MaxStoredBytes {
		return fmt.Errorf("rowpack: stored size %d exceeds limit %d", h.StoredSize, r.limits.MaxStoredBytes)
	}
	if h.RawSize > r.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: raw size %d exceeds limit %d", h.RawSize, r.limits.MaxRawBytes)
	}
	if h.Compression == fileformat.CompressionNone && h.StoredSize != h.RawSize {
		return fmt.Errorf("rowpack: none-compressed block stored %d != raw %d", h.StoredSize, h.RawSize)
	}
	return nil
}

// readAtBlockCopy is the ReadAt path (plain io.ReaderAt handles).
func (r *Reader) readAtBlockCopy(offset int64) (*Block, error) {
	var hdr [fileformat.BlockHeaderSize]byte
	if _, err := r.ra.ReadAt(hdr[:], offset); err != nil {
		return nil, fmt.Errorf("rowpack: read block header at %d: %w", offset, err)
	}
	var h fileformat.BlockHeader
	if err := h.Unmarshal(hdr[:]); err != nil {
		return nil, err
	}
	if err := r.checkHeader(&h); err != nil {
		return nil, err
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
	out, err := Decompress(h.Compression, nil, stored, r.limits.MaxRawBytes)
	if err != nil {
		return nil, fmt.Errorf("rowpack: block %d: %w", h.BlockID, err)
	}
	return out, nil
}

// Block is a validated block: header plus checked uncompressed payload.
type Block struct {
	Header fileformat.BlockHeader
	Raw    []byte
}

var _ = errors.New
