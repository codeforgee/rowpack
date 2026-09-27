package block

import (
	"fmt"
	"sync"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
)

// RowsContainer is a validated Rows Block page container:
//
//	[RowsBlockHeader][RowsPageDirEntry × N][stored page 0]…
//
// It has two backing modes:
//
//   - whole: the container plaintext (header + directory + the independently
//     compressed stored pages) was read at once and is owned by the container.
//     Used for encrypted blocks (the whole container is sealed) and by the
//     whole-container consumers (recovery/verify/replay).
//   - lazy: only the block header + container header + directory were read
//     (plain blocks); each page is read + decompressed from the file on first
//     access, so a cold single-row read pulls only the one
//     page it needs instead of the whole container.
//
// Either way, decompressed pages are memoized in the container (keyed by page
// index), so repeated access to a page never re-reads or re-decompresses it.
// A cached page owns its decompressed buffer; release is a no-op and the page
// stays valid for the container's lifetime.
//
// Corruption is always an error, never a panic; lengths/counts/offsets are
// validated before any allocation or slice.
type RowsContainer struct {
	Header format.RowsBlockHeader
	Dir    []format.RowsPageDirEntry

	// blockH is the block header that produced this container (carries the
	// compression / encryption context used to decode pages).
	blockH format.BlockHeader
	comp   format.Compression
	limits Limits

	// stored is the whole container plaintext when mode==whole; nil when lazy.
	stored []byte

	// lazy page-I/O context (mode==lazy): pages are read+decompressed through
	// reader at blockOffset + BlockHeaderSize + StoredOffset.
	reader      *Reader
	blockOffset int64

	// pages memoizes decompressed pages by page index (owned, not scratch).
	// pagesMu guards it: a memoized container is shared between concurrent
	// readers (Get/ReadBatch), so the first access to a page must not race on
	// the map. Decompression itself happens outside the lock so a cold page is
	// not serialized across goroutines; only the memoization is.
	pagesMu sync.RWMutex
	pages   map[int]*RowsPage
	// retainedBytes is the value-byte footprint charged to the owning LRU:
	// container header/directory plus every memoized page raw buffer and its
	// O(1) record-index arrays. onRetainedChange updates the LRU entry whenever
	// a page is installed, so page memoization cannot bypass CacheBytes.
	retainedBytes    int64
	onRetainedChange func(int64)
}

// lazy reports whether this container reads pages on demand rather than from
// an owned whole container buffer.
func (c *RowsContainer) lazy() bool { return c.stored == nil }

// StoredLen returns the size of the whole container plaintext, or the
// header+directory size for a lazy container (used for cache accounting).
func (c *RowsContainer) StoredLen() int {
	if c.stored != nil {
		return len(c.stored)
	}
	return format.RowsBlockHeaderSize + len(c.Dir)*format.RowsPageDirEntrySize
}

// SetCacheAccounting installs the owning cache's size updater. It must be
// called before the container is published in that cache.
func (c *RowsContainer) SetCacheAccounting(update func(int64)) {
	c.pagesMu.Lock()
	c.retainedBytes = int64(c.StoredLen())
	c.onRetainedChange = update
	c.pagesMu.Unlock()
}

// RetainedLen returns the bytes currently owned by this container and its
// memoized decoded pages.
func (c *RowsContainer) RetainedLen() int64 {
	c.pagesMu.RLock()
	defer c.pagesMu.RUnlock()
	if c.retainedBytes == 0 {
		return int64(c.StoredLen())
	}
	return c.retainedBytes
}

func rowsPageRetainedBytes(p *RowsPage) int64 {
	return int64(len(p.raw)) + int64(len(p.ids))*8 + int64(len(p.ends))*4 + int64(len(p.vers))*4
}

// RecordsRegionStart is the byte offset (within stored) where the first page's
// stored bytes begin.
func (c *RowsContainer) RecordsRegionStart() int {
	return format.RowsBlockHeaderSize + len(c.Dir)*format.RowsPageDirEntrySize
}

// PageCount returns the number of pages.
func (c *RowsContainer) PageCount() int { return int(c.Header.PageCount) }

// ParseContainer validates a whole Rows Block page container against its
// block header. It checks container geometry, the aggregate header/directory
// CRC (BlockHeader.RawCRC32C), every page's bounds (inside the container,
// non-overlapping, within limits) and the logical TotalRecords == ItemCount.
// It never decompresses a page. Corruption is always an error, never a panic.
func ParseContainer(container []byte, h format.BlockHeader, limits Limits) (*RowsContainer, error) {
	if h.BlockKind != format.BlockKindRows {
		return nil, fmt.Errorf("rowpack: block %d is kind %d, not rows", h.BlockID, h.BlockKind)
	}
	// h.StoredSize is the on-disk size; an encrypted block carries an extra
	// AEAD tag, so the container plaintext is StoredSize - tag.
	plainLen := int(h.StoredSize)
	if h.Encrypted {
		plainLen -= format.AESGCMTagLen
	}
	if len(container) != plainLen {
		return nil, fmt.Errorf("rowpack: container %d bytes, want %d", len(container), plainLen)
	}
	if len(container) < format.RowsBlockHeaderSize {
		return nil, fmt.Errorf("rowpack: container too short for header")
	}
	var rh format.RowsBlockHeader
	if err := rh.Unmarshal(container[:format.RowsBlockHeaderSize]); err != nil {
		return nil, err
	}
	// The header/directory region is authenticated by the block header CRC.
	dirEnd := format.RowsBlockHeaderSize + int(rh.DirectoryBytes)
	if dirEnd > len(container) {
		return nil, fmt.Errorf("rowpack: container directory %d bytes overruns %d", rh.DirectoryBytes, len(container))
	}
	if format.CRC32C(container[:dirEnd]) != h.RawCRC32C {
		return nil, fmt.Errorf("rowpack: block %d container header/dir CRC mismatch", h.BlockID)
	}
	if err := validateRowCounts(&rh, h, limits); err != nil {
		return nil, err
	}
	dir, err := parsePageDir(container, &rh)
	if err != nil {
		return nil, err
	}
	c := &RowsContainer{Header: rh, Dir: dir, blockH: h, comp: h.Compression, limits: limits, stored: container}
	if err := c.checkBounds(format.RowsBlockHeaderSize + len(dir)*format.RowsPageDirEntrySize); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseRowsDir validates a block's header + container header + page directory
// WITHOUT reading any page payload, and returns a lazy RowsContainer that
// reads (and, for per-page-encrypted blocks, OPENs) pages on demand through
// the reader. The directory is plaintext for plain and encrypted blocks alike,
// so this is the read entry point for both.
func ParseRowsDir(offset int64, r *Reader, h format.BlockHeader, limits Limits) (*RowsContainer, error) {
	if h.BlockKind != format.BlockKindRows {
		return nil, fmt.Errorf("rowpack: block %d is kind %d, not rows", h.BlockID, h.BlockKind)
	}
	// The block header + container header + page directory are all plaintext
	// even for encrypted blocks (BINARY_FORMAT_V1 §5.1: pages are sealed, the
	// directory is not). So we can read the directory and OPEN+decompress
	// individual pages on demand, which is the whole point of per-page
	// encryption on the read path.
	// Read the container header first (it carries PageCount so we know the
	// directory length).
	var ch [format.RowsBlockHeaderSize]byte
	if _, err := r.ra.ReadAt(ch[:], offset+format.BlockHeaderSize); err != nil {
		return nil, fmt.Errorf("rowpack: read container header at %d: %w", offset+format.BlockHeaderSize, err)
	}
	var rh format.RowsBlockHeader
	if err := rh.Unmarshal(ch[:]); err != nil {
		return nil, err
	}
	if err := validateRowCounts(&rh, h, limits); err != nil {
		return nil, err
	}
	// Read the directory region.
	dirEnd := format.RowsBlockHeaderSize + int(rh.DirectoryBytes)
	if dirEnd > int(h.StoredSize) {
		return nil, fmt.Errorf("rowpack: container directory %d bytes overruns stored %d", rh.DirectoryBytes, h.StoredSize)
	}
	dirBytes := make([]byte, dirEnd)
	if _, err := r.ra.ReadAt(dirBytes, offset+format.BlockHeaderSize); err != nil {
		return nil, fmt.Errorf("rowpack: read container directory at %d: %w", offset+format.BlockHeaderSize, err)
	}
	// The header/directory region is authenticated by the block header CRC.
	if format.CRC32C(dirBytes) != h.RawCRC32C {
		return nil, fmt.Errorf("rowpack: block %d container header/dir CRC mismatch", h.BlockID)
	}
	dir, err := parsePageDir(dirBytes, &rh)
	if err != nil {
		return nil, err
	}
	c := &RowsContainer{Header: rh, Dir: dir, blockH: h, comp: h.Compression, limits: limits, reader: r, blockOffset: offset}
	if err := c.checkBounds(format.RowsBlockHeaderSize + len(dir)*format.RowsPageDirEntrySize); err != nil {
		return nil, err
	}
	return c, nil
}

// validateRowCounts cross-checks the container header against the block header.
func validateRowCounts(rh *format.RowsBlockHeader, h format.BlockHeader, _ Limits) error {
	if rh.TotalRecords != h.ItemCount {
		return fmt.Errorf("rowpack: container total records %d != block item count %d", rh.TotalRecords, h.ItemCount)
	}
	wantDir := uint64(rh.PageCount) * uint64(format.RowsPageDirEntrySize)
	if wantDir > uint64(h.StoredSize) || uint64(rh.DirectoryBytes) != wantDir {
		return fmt.Errorf("rowpack: container directory %d != pageCount %d * %d", rh.DirectoryBytes, rh.PageCount, format.RowsPageDirEntrySize)
	}
	return nil
}

// parsePageDir parses the PageCount directory entries from the header+dir region.
func parsePageDir(containerOrHeader []byte, rh *format.RowsBlockHeader) ([]format.RowsPageDirEntry, error) {
	dir := make([]format.RowsPageDirEntry, rh.PageCount)
	pos := format.RowsBlockHeaderSize
	dirEnd := format.RowsBlockHeaderSize + int(rh.DirectoryBytes)
	for i := range dir {
		if pos+format.RowsPageDirEntrySize > dirEnd {
			return nil, fmt.Errorf("rowpack: container directory truncated")
		}
		if err := dir[i].Unmarshal(containerOrHeader[pos : pos+format.RowsPageDirEntrySize]); err != nil {
			return nil, err
		}
		pos += format.RowsPageDirEntrySize
	}
	return dir, nil
}

// checkBounds checks every page's geometry: ordinal order, contiguous
// FirstRecordOrdinal, non-overlapping contiguous stored offsets, sizes within
// the safety limits and, for a whole container, that pages end exactly at the
// container length. recordsStart is the byte offset where the first page's
// stored bytes begin (== container header + directory for a whole container).
func (c *RowsContainer) checkBounds(recordsStart int) error {
	expectedOff := recordsStart
	firstOrd := uint32(0)
	for i := range c.Dir {
		e := &c.Dir[i]
		if int(e.PageOrdinal) != i {
			return fmt.Errorf("rowpack: page %d ordinal %d out of order", i, e.PageOrdinal)
		}
		if e.FirstRecordOrdinal != firstOrd {
			return fmt.Errorf("rowpack: page %d first ordinal %d, want %d", i, e.FirstRecordOrdinal, firstOrd)
		}
		if e.RecordCount == 0 {
			return fmt.Errorf("rowpack: page %d has zero records", i)
		}
		if int(e.StoredOffset) != expectedOff {
			return fmt.Errorf("rowpack: page %d stored offset %d, want %d", i, e.StoredOffset, expectedOff)
		}
		if e.StoredSize > c.limits.MaxStoredBytes {
			return fmt.Errorf("rowpack: page %d stored size %d exceeds limit %d", i, e.StoredSize, c.limits.MaxStoredBytes)
		}
		if e.RawSize > c.limits.MaxRawBytes {
			return fmt.Errorf("rowpack: page %d raw size %d exceeds limit %d", i, e.RawSize, c.limits.MaxRawBytes)
		}
		if c.stored != nil && int(e.StoredOffset)+int(e.StoredSize) > len(c.stored) {
			return fmt.Errorf("rowpack: page %d stored bytes escape container", i)
		}
		// None-compression pages are stored == raw, EXCEPT per-page-encrypted
		// blocks where the stored bytes are the sealed page (raw + tag) and
		// the raw size is recovered after OPEN.
		if c.comp == format.CompressionNone && !c.blockH.Encrypted && e.StoredSize != e.RawSize {
			return fmt.Errorf("rowpack: none-compressed page %d stored %d != raw %d", i, e.StoredSize, e.RawSize)
		}
		expectedOff += int(e.StoredSize)
		firstOrd += e.RecordCount
	}
	if c.stored != nil && expectedOff != len(c.stored) {
		return fmt.Errorf("rowpack: pages end at %d, container is %d", expectedOff, len(c.stored))
	}
	return nil
}

// PageFor returns the index of the page owning itemOrdinal, by
// binary search over the strictly-increasing FirstRecordOrdinal.
func (c *RowsContainer) PageFor(ordinal uint32) (int, error) {
	if ordinal >= c.Header.TotalRecords {
		return 0, fmt.Errorf("rowpack: record ordinal %d out of range (%d)", ordinal, c.Header.TotalRecords)
	}
	lo, hi := 0, len(c.Dir)
	for lo < hi {
		mid := (lo + hi) / 2
		if c.Dir[mid].FirstRecordOrdinal <= ordinal {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	i := lo - 1
	if i < 0 || ordinal >= c.Dir[i].FirstRecordOrdinal+c.Dir[i].RecordCount {
		return 0, fmt.Errorf("rowpack: record ordinal %d not covered by any page", ordinal)
	}
	return i, nil
}

// decompress decompresses the stored page i into buf, returning the
// page's uncompressed bytes. buf must have capacity for at least RawSize.
func (c *RowsContainer) decompress(i int, buf *rawBuf) ([]byte, error) {
	dir := &c.Dir[i]
	stored := c.stored[int(dir.StoredOffset) : int(dir.StoredOffset)+int(dir.StoredSize)]
	var raw []byte
	if c.comp == format.CompressionNone {
		if uint32(len(stored)) > c.limits.MaxRawBytes {
			return nil, fmt.Errorf("rowpack: page %d stored size %d exceeds limit %d", i, len(stored), c.limits.MaxRawBytes)
		}
		raw = buf.data[:len(stored)]
		copy(raw, stored)
	} else {
		maxOut := c.limits.MaxRawBytes
		if dir.RawSize < maxOut {
			maxOut = dir.RawSize
		}
		var err error
		raw, err = decompressZstd(buf.data, stored, maxOut)
		if err != nil {
			return nil, fmt.Errorf("rowpack: page %d: %w", i, err)
		}
	}
	if uint32(len(raw)) != dir.RawSize {
		return nil, fmt.Errorf("rowpack: page %d decompressed %d bytes, want %d", i, len(raw), dir.RawSize)
	}
	return raw, nil
}

// parsePage parses and CRC-validates the uncompressed page raw bytes against
// its directory entry.
func (c *RowsContainer) parsePage(i int, raw []byte) (*RowsPage, error) {
	p, err := ParseRowsPage(raw)
	if err != nil {
		return nil, err
	}
	if p.h.CRC32C != c.Dir[i].PageCRC32C {
		return nil, fmt.Errorf("rowpack: page %d header CRC %d != directory %d", i, p.h.CRC32C, c.Dir[i].PageCRC32C)
	}
	return p, nil
}

// pageOwned obtains the (possibly memoized) page i. For a lazy container it
// reads+decompresses just that page via the reader; for a whole container it
// decompresses from the owned buffer. The returned page owns its buffer and
// is memoized, so it is valid for the container's lifetime.
func (c *RowsContainer) pageOwned(i int) (*RowsPage, error) {
	if i < 0 || i >= len(c.Dir) {
		return nil, fmt.Errorf("rowpack: page index %d out of range (%d)", i, len(c.Dir))
	}
	c.pagesMu.RLock()
	if c.pages != nil {
		if p, ok := c.pages[i]; ok {
			c.pagesMu.RUnlock()
			return p, nil
		}
	}
	c.pagesMu.RUnlock()
	// Decompress outside the lock so concurrent first accessors of the same
	// page don't serialize the (expensive) page decode.
	var p *RowsPage
	var err error
	if c.lazy() {
		p, err = c.reader.ReadRowsPage(c.blockOffset, c, i)
	} else {
		buf := &rawBuf{data: make([]byte, c.Dir[i].RawSize)}
		raw, derr := c.decompress(i, buf)
		if derr != nil {
			return nil, derr
		}
		p, err = c.parsePage(i, raw)
	}
	if err != nil {
		return nil, err
	}
	c.pagesMu.Lock()
	if c.pages == nil {
		c.pages = make(map[int]*RowsPage)
	}
	if existing, ok := c.pages[i]; ok {
		c.pagesMu.Unlock()
		return existing, nil
	}
	c.pages[i] = p
	c.retainedBytes += rowsPageRetainedBytes(p)
	// Install and LRU accounting update under one pagesMu critical section:
	// two goroutines memoizing different pages of the same container must not
	// apply their absolute retained sizes out of order (a stale, smaller
	// update landing last would permanently under-count the LRU entry, letting
	// memoized pages bypass CacheBytes). The LRU lock is never acquired in
	// reverse order (nothing calls pagesMu while holding the LRU lock), so
	// taking it here is deadlock-free.
	if c.onRetainedChange != nil {
		c.onRetainedChange(c.retainedBytes)
	}
	c.pagesMu.Unlock()
	return p, nil
}

// PageScratch returns the validated page i. The returned release func is a
// no-op for memoized pages (the page owns its buffer for the container's
// lifetime); it is retained for API compatibility with the prior scratch
// model. Batch/scan paths that decode several records from one page call this
// once per page.
func (c *RowsContainer) PageScratch(i int) (*RowsPage, func(), error) {
	p, err := c.pageOwned(i)
	if err != nil {
		return nil, nil, err
	}
	return p, func() {}, nil
}

// RecordAt decodes one record by block item ordinal, decompressing only
// the containing page. The returned codec.PageRecord's Body aliases the page
// buffer, which is valid for the container's lifetime (release is a no-op). It
// is the Get / single-random-read accessor.
func (c *RowsContainer) RecordAt(ordinal uint32) (codec.PageRecord, func(), error) {
	pi, err := c.PageFor(ordinal)
	if err != nil {
		return codec.PageRecord{}, nil, err
	}
	page, err := c.pageOwned(pi)
	if err != nil {
		return codec.PageRecord{}, nil, err
	}
	pageOrd := ordinal - c.Dir[pi].FirstRecordOrdinal
	rec, err := page.RecordAt(pageOrd)
	if err != nil {
		return codec.PageRecord{}, nil, err
	}
	return rec, func() {}, nil
}

// ForEach iterates every record of every page in call order, decompressing one
// page at a time (memoized in the container, never the whole block at once).
// Bodies alias the page buffer and must not be retained beyond the callback.
// It is the recovery/rebuild/verify/scan accessor.
func (c *RowsContainer) ForEach(fn func(codec.PageRecord) error) error {
	for i := range c.Dir {
		page, err := c.pageOwned(i)
		if err != nil {
			return err
		}
		if err := page.Records(fn); err != nil {
			return err
		}
	}
	return nil
}
