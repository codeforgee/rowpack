package block

import (
	"fmt"
	"io"
	"sync/atomic"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Decrypter authenticates and decrypts one sealed block payload before
// decompression. It is optional: a nil decrypter keeps the reader on the
// plain path and fails closed on encrypted blocks.
//
// Decrypt opens a whole sealed block payload (metadata blocks, which remain
// whole-sealed). OpenPage opens one sealed Rows Page stored bytes (per-page
// encryption, BINARY_FORMAT_V1 §5.1); the page directory is plaintext, so a
// reader OPENs only the page it needs.
type Decrypter interface {
	Decrypt(header fileformat.BlockHeader, ciphertext []byte) ([]byte, error)
	OpenPage(header fileformat.BlockHeader, page fileformat.RowsPageDirEntry, ciphertext []byte) ([]byte, error)
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

	// Cumulative I/O counters (measurement instrumentation):
	// readBytes counts header+stored bytes pulled from the handle before
	// decryption; decompressedBytes counts validated raw payload bytes;
	// pageCtrs counts Rows-page reads. Together they
	// quantify cold-read amplification: with a 256 KiB block a single-row
	// read still pulls and decompresses the whole block, but only the
	// accessed page is loaded from a page container.
	readBytes         atomic.Uint64
	decompressedBytes atomic.Uint64
	pageCtrs          PageStatCtrs
}

// PageStats is a snapshot of the per-page read counters.
type PageStats struct {
	PageLoads       uint64 // pages decompressed on demand
	PageRawBytes    uint64 // validated raw page payload bytes produced
	PageStoredBytes uint64 // stored page bytes pulled from the handle
}

// PageStatCtrs is the shared per-page counter set written by RowsContainer.
type PageStatCtrs struct {
	loads  atomic.Uint64
	raw    atomic.Uint64
	stored atomic.Uint64
}

// pageCounts returns the per-page counters for a RowsContainer to attribute
// its lazy page reads to the reader.
func (r *Reader) pageCounts() *PageStatCtrs { return &r.pageCtrs }

// IOStats is a snapshot of the reader's cumulative I/O counters.
type IOStats struct {
	ReadBytes         uint64 // header + stored bytes pulled from the handle
	DecompressedBytes uint64 // validated raw payload bytes produced
	PageLoads         uint64 // Rows-page decompressions on demand
	PageRawBytes      uint64 // validated raw page payload bytes
	PageStoredBytes   uint64 // stored page bytes pulled from the handle
}

// Stats returns the cumulative counters since the Reader was created.
func (r *Reader) Stats() IOStats {
	return IOStats{
		ReadBytes:         r.readBytes.Load(),
		DecompressedBytes: r.decompressedBytes.Load(),
		PageLoads:         r.pageCtrs.loads.Load(),
		PageRawBytes:      r.pageCtrs.raw.Load(),
		PageStoredBytes:   r.pageCtrs.stored.Load(),
	}
}

// count records one successful validated read: bytes pulled from the handle
// (header + stored payload) and the raw payload produced. Called exactly
// once per successful block read on every path.
func (r *Reader) count(h *fileformat.BlockHeader) {
	r.readBytes.Add(uint64(fileformat.BlockHeaderSize + int64(h.StoredSize)))
	r.decompressedBytes.Add(uint64(h.RawSize))
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

// ReadRowsDir reads a Rows block's header + container header + page directory
// into a lazy RowsContainer that reads individual pages on demand (page-level
// I/O). The block header, container header and page directory are
// plaintext even for per-page-encrypted blocks, so this is the single read
// entry point for plain and encrypted Rows blocks alike; each page is OPENed
// (authenticated) and decompressed only when accessed.
func (r *Reader) ReadRowsDir(offset int64) (*RowsContainer, error) {
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
	if h.BlockKind != fileformat.BlockKindRows {
		return nil, fmt.Errorf("rowpack: block %d is kind %d, expected rows", h.BlockID, h.BlockKind)
	}
	return ParseRowsDir(offset, r, h, r.limits)
}

// ReadRowsPage reads and decompresses a single page of a lazy container. Only
// that page's stored bytes are pulled from the file, so a cold single-row read
// does not read the whole container. Returns an owned RowsPage (its buffer is
// a fresh allocation and valid for the containing container's lifetime).
func (r *Reader) ReadRowsPage(offset int64, c *RowsContainer, pageIdx int) (*RowsPage, error) {
	dir := &c.Dir[pageIdx]
	pgOff := offset + fileformat.BlockHeaderSize + int64(dir.StoredOffset)
	stored := make([]byte, dir.StoredSize)
	if _, err := r.ra.ReadAt(stored, pgOff); err != nil {
		return nil, fmt.Errorf("rowpack: read page %d at %d: %w", pageIdx, pgOff, err)
	}
	r.readBytes.Add(uint64(len(stored)))
	// Per-page encryption: the page directory is plaintext, so the stored
	// bytes here are the sealed page; OPEN it (authenticate) before
	// decompressing. Metadata blocks remain whole-sealed (Decrypt).
	if c.blockH.Encrypted {
		if r.decrypter == nil {
			return nil, fmt.Errorf("rowpack: block %d page %d encrypted but no decrypter", c.blockH.BlockID, pageIdx)
		}
		pt, err := r.decrypter.OpenPage(c.blockH, *dir, stored)
		if err != nil {
			return nil, err
		}
		if len(pt) != int(dir.StoredSize)-fileformat.AESGCMTagLen {
			return nil, fmt.Errorf("rowpack: page %d opened %d bytes, want stored %d - tag", pageIdx, len(pt), dir.StoredSize)
		}
		stored = pt
	}
	var raw []byte
	if c.comp == fileformat.CompressionNone {
		raw = stored
	} else {
		maxOut := c.limits.MaxRawBytes
		if dir.RawSize < maxOut {
			maxOut = dir.RawSize
		}
		var err error
		raw, err = decompressZstd(nil, stored, maxOut)
		if err != nil {
			return nil, fmt.Errorf("rowpack: page %d: %w", pageIdx, err)
		}
	}
	if uint32(len(raw)) != dir.RawSize {
		return nil, fmt.Errorf("rowpack: page %d decompressed %d bytes, want %d", pageIdx, len(raw), dir.RawSize)
	}
	r.decompressedBytes.Add(uint64(len(raw)))
	return c.parsePage(pageIdx, raw)
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
	r.count(&h)
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
	r.count(&h)
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
