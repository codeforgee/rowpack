package block

import (
	"errors"
	"fmt"
	"io"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Decrypter authenticates and decrypts one sealed block payload before
// decompression. It is optional: a nil decrypter keeps the reader on the
// plain path and fails closed on encrypted blocks.
type Decrypter interface {
	Decrypt(header fileformat.BlockHeader, ciphertext []byte) ([]byte, error)
}

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

	// decrypter restores plaintext before decompression for encrypted
	// blocks. Set via SetDecrypter before any reads; read-only after.
	decrypter Decrypter
}

// NewReader creates a block reader over ra.
func NewReader(ra io.ReaderAt, limits Limits) *Reader {
	return &Reader{ra: ra, limits: limits}
}

// SetDecrypter installs the block decrypter (nil clears it). It must be
// called before any block is read; concurrent reads must not race it.
func (r *Reader) SetDecrypter(d Decrypter) {
	r.decrypter = d
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
	stored, err := r.maybeDecrypt(sb, &h)
	if err != nil {
		return nil, err
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
	if h.Compression == fileformat.CompressionNone && !h.Encrypted {
		// Plain, uncompressed: Decompress returned the view itself; copy so
		// the returned (and potentially cached) Block never aliases the file
		// mapping. Encrypted blocks were decrypted into a fresh buffer.
		cp := make([]byte, len(raw))
		copy(cp, raw)
		raw = cp
	}
	return &Block{Header: h, Raw: raw}, nil
}

// checkHeader validates the stored/raw limits and None-size agreement. An
// encrypted block's StoredSize is the ciphertext length (plain + tag), so the
// None equality only applies to plain blocks.
func (r *Reader) checkHeader(h *fileformat.BlockHeader) error {
	if h.StoredSize > r.limits.MaxStoredBytes {
		return fmt.Errorf("rowpack: stored size %d exceeds limit %d", h.StoredSize, r.limits.MaxStoredBytes)
	}
	if h.RawSize > r.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: raw size %d exceeds limit %d", h.RawSize, r.limits.MaxRawBytes)
	}
	if !h.Encrypted && h.Compression == fileformat.CompressionNone && h.StoredSize != h.RawSize {
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
	plain, err := r.maybeDecrypt(stored, &h)
	if err != nil {
		return nil, err
	}
	raw, err := r.decompress(&h, plain)
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

// maybeDecrypt returns the plaintext for a stored payload: the input slice
// itself for plain blocks, or a fresh buffer for encrypted blocks (which are
// authenticated against the header). It never outlives its view: callers
// must keep the mapping view alive until this returns.
func (r *Reader) maybeDecrypt(stored []byte, h *fileformat.BlockHeader) ([]byte, error) {
	if !h.Encrypted {
		return stored, nil
	}
	if r.decrypter == nil {
		return nil, fmt.Errorf("rowpack: block %d is encrypted but no decrypter is installed", h.BlockID)
	}
	pt, err := r.decrypter.Decrypt(*h, stored)
	if err != nil {
		return nil, err
	}
	// The plaintext is the compressed payload: its length is the ciphertext
	// minus the tag. The decompressed length (== RawSize) is validated by
	// decompress afterwards.
	if len(pt) != int(h.StoredSize)-fileformat.AESGCMTagLen {
		return nil, fmt.Errorf("rowpack: block %d decrypted %d bytes, want stored %d - tag", h.BlockID, len(pt), h.StoredSize)
	}
	return pt, nil
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

// ReadAtBlockTransient reads, validates and decompresses the block whose
// header starts at offset, decompressing into a pooled scratch buffer. The
// returned BlockScratch aliases the scratch; callers must call Release once
// they are done with Raw (and any slices of it). Transient reads never enter
// a cache: they are meant for streaming paths (Scan) where
// each block is consumed once and must not pollute the hot cache.
func (r *Reader) ReadAtBlockTransient(offset int64) (*BlockScratch, error) {
	if v, ok := r.ra.(viewer); ok {
		return r.readAtBlockViewT(offset, v)
	}
	return r.readAtBlockCopyT(offset)
}

// BlockScratch is a validated block whose Raw may alias a pooled scratch
// buffer. Release returns the buffer to the pool and must be called exactly
// once after Raw and all of its slices are no longer referenced.
type BlockScratch struct {
	Block
	buf *rawBuf
}

// Release returns the pooled scratch buffer. It is idempotent.
func (s *BlockScratch) Release() {
	if s.buf != nil {
		putRawBuf(s.buf)
		s.buf = nil
	}
}

// Detach clears the scratch association so Release becomes a no-op,
// transferring buffer ownership to the caller (used to hand an exact-fit
// buffer to a cache without a copy). The detached buffer never returns to
// the pool; the pool replenishes itself on demand.
func (s *BlockScratch) Detach() { s.buf = nil }

// decompressIntoScratch decompresses stored into buf, ensuring the result
// never aliases a file mapping (None copies into the scratch).
func (r *Reader) decompressIntoScratch(h *fileformat.BlockHeader, stored []byte, buf *rawBuf) ([]byte, error) {
	switch h.Compression {
	case fileformat.CompressionNone:
		if uint32(len(stored)) > r.limits.MaxRawBytes {
			return nil, fmt.Errorf("rowpack: stored size %d exceeds limit %d", len(stored), r.limits.MaxRawBytes)
		}
		out := buf.data[:len(stored)]
		copy(out, stored)
		return out, nil
	case fileformat.CompressionZstd:
		out, err := decompressZstd(buf.data, stored, r.limits.MaxRawBytes)
		if err != nil {
			return nil, fmt.Errorf("rowpack: block %d: %w", h.BlockID, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("rowpack: unsupported compression %d", h.Compression)
}

// readAtBlockViewT is the transient zero-copy path; see readAtBlockView.
func (r *Reader) readAtBlockViewT(offset int64, v viewer) (*BlockScratch, error) {
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
	stored, err := r.maybeDecrypt(sb, &h)
	if err != nil {
		return nil, err
	}
	buf := getRawBuf(h.RawSize)
	raw, err := r.decompressIntoScratch(&h, stored, buf)
	if err != nil {
		putRawBuf(buf)
		return nil, err
	}
	if uint32(len(raw)) != h.RawSize {
		putRawBuf(buf)
		return nil, fmt.Errorf("rowpack: decompressed %d bytes, want %d", len(raw), h.RawSize)
	}
	if fileformat.CRC32C(raw) != h.RawCRC32C {
		putRawBuf(buf)
		return nil, fmt.Errorf("rowpack: block %d raw CRC mismatch", h.BlockID)
	}
	buf.data = raw // DecodeAll may have grown past the pooled buffer
	return &BlockScratch{Block: Block{Header: h, Raw: raw}, buf: buf}, nil
}

// readAtBlockCopyT is the transient ReadAt path (plain io.ReaderAt handles).
func (r *Reader) readAtBlockCopyT(offset int64) (*BlockScratch, error) {
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
	plain, err := r.maybeDecrypt(stored, &h)
	if err != nil {
		return nil, err
	}
	buf := getRawBuf(h.RawSize)
	raw, err := r.decompressIntoScratch(&h, plain, buf)
	if err != nil {
		putRawBuf(buf)
		return nil, err
	}
	if uint32(len(raw)) != h.RawSize {
		putRawBuf(buf)
		return nil, fmt.Errorf("rowpack: decompressed %d bytes, want %d", len(raw), h.RawSize)
	}
	if fileformat.CRC32C(raw) != h.RawCRC32C {
		putRawBuf(buf)
		return nil, fmt.Errorf("rowpack: block %d raw CRC mismatch", h.BlockID)
	}
	buf.data = raw
	return &BlockScratch{Block: Block{Header: h, Raw: raw}, buf: buf}, nil
}

var _ = errors.New
