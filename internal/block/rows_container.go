package block

import (
	"fmt"
	"sync"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
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
	return format.RowsBlockHeaderSize + int(c.Header.DirectoryBytes)
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

// RecordsRegionStart is the byte offset (within stored) where the first page's
// stored bytes begin.
func (c *RowsContainer) RecordsRegionStart() int {
	return format.RowsBlockHeaderSize + int(c.Header.DirectoryBytes)
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
	if err := c.checkBounds(format.RowsBlockHeaderSize + int(rh.DirectoryBytes)); err != nil {
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
	if err := c.checkBounds(format.RowsBlockHeaderSize + int(rh.DirectoryBytes)); err != nil {
		return nil, err
	}
	return c, nil
}

// validateRowCounts cross-checks the container header against the block header.
func validateRowCounts(rh *format.RowsBlockHeader, h format.BlockHeader, _ Limits) error {
	if rh.TotalRecords != h.ItemCount {
		return fmt.Errorf("rowpack: container total records %d != block item count %d", rh.TotalRecords, h.ItemCount)
	}
	// The exact directory length is no longer derivable from PageCount, but it
	// still has to fit inside the block's stored bytes.
	if uint64(rh.DirectoryBytes) > uint64(h.StoredSize) {
		return fmt.Errorf("rowpack: container directory %d bytes overruns stored %d", rh.DirectoryBytes, h.StoredSize)
	}
	return nil
}

// parsePageDir parses the PageCount directory entries from the header+dir
// region. Entries are varint-encoded and therefore variable-width, so they are
// decoded in order and must consume the directory exactly: a directory with
// slack could otherwise hide a second, contradictory reading of the geometry.
//
// StoredOffset is not encoded (pages tile the container), so it is recomputed
// here: the first page starts where the directory ends, and every later page
// starts where the previous one ended.
func parsePageDir(containerOrHeader []byte, rh *format.RowsBlockHeader) ([]format.RowsPageDirEntry, error) {
	dir := make([]format.RowsPageDirEntry, rh.PageCount)
	pos := format.RowsBlockHeaderSize
	dirEnd := format.RowsBlockHeaderSize + int(rh.DirectoryBytes)
	if dirEnd > len(containerOrHeader) {
		return nil, fmt.Errorf("rowpack: container directory truncated")
	}
	off := dirEnd
	for i := range dir {
		n, err := dir[i].Unmarshal(containerOrHeader[pos:dirEnd])
		if err != nil {
			return nil, fmt.Errorf("rowpack: container directory entry %d: %w", i, err)
		}
		pos += n
		dir[i].StoredOffset = uint64(off)
		off += int(dir[i].StoredSize)
	}
	if pos != dirEnd {
		return nil, fmt.Errorf("rowpack: container directory has %d unread bytes", dirEnd-pos)
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
		// StoredOffset is recomputed by the directory parser rather than read
		// from disk, so it is contiguous by construction: the check that used
		// to live here is now a tautology. What still needs proving is that the
		// declared sizes stay inside the container.
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
	if c.stored != nil {
		// Pages must tile the container exactly: nothing may sit between the
		// last page and the container end.
		if expectedOff != len(c.stored) {
			return fmt.Errorf("rowpack: pages end at %d, container is %d", expectedOff, len(c.stored))
		}
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
		maxOut := min(dir.RawSize, c.limits.MaxRawBytes)
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

// parsePage parses the uncompressed page raw bytes. The page header's own
// CRC32C covers the page streams and ParseRowsPage verifies it, so the
// directory no longer carries a second copy of the same checksum.
func (c *RowsContainer) parsePage(i int, raw []byte) (*RowsPage, error) {
	p, err := ParseRowsPage(raw)
	if err != nil {
		return nil, fmt.Errorf("rowpack: page %d: %w", i, err)
	}
	return p, nil
}

// cachedPage returns the already-memoized page i, or nil.
func (c *RowsContainer) cachedPage(i int) *RowsPage {
	c.pagesMu.RLock()
	defer c.pagesMu.RUnlock()
	if c.pages != nil {
		return c.pages[i]
	}
	return nil
}

// decodePage reads and decompresses page i without touching the page memo. For
// a lazy container it goes through the reader; for a whole container it
// decompresses from the owned buffer. The returned page owns its buffer.
func (c *RowsContainer) decodePage(i int) (*RowsPage, error) {
	if c.lazy() {
		return c.reader.ReadRowsPage(c.blockOffset, c, i)
	}
	buf := &rawBuf{data: make([]byte, c.Dir[i].RawSize)}
	raw, err := c.decompress(i, buf)
	if err != nil {
		return nil, err
	}
	return c.parsePage(i, raw)
}

// pageOwned obtains the (possibly memoized) page i. For a lazy container it
// reads+decompresses just that page via the reader; for a whole container it
// decompresses from the owned buffer. The returned page owns its buffer and
// is memoized, so it is valid for the container's lifetime.
func (c *RowsContainer) pageOwned(i int) (*RowsPage, error) {
	if i < 0 || i >= len(c.Dir) {
		return nil, fmt.Errorf("rowpack: page index %d out of range (%d)", i, len(c.Dir))
	}
	if p := c.cachedPage(i); p != nil {
		return p, nil
	}
	// Decompress outside the lock so concurrent first accessors of the same
	// page don't serialize the (expensive) page decode.
	p, err := c.decodePage(i)
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
	// A page's retained bytes: raw payload + the columnar row slices
	// (RowID 8B, page-end 4B, schema version 4B per row).
	c.retainedBytes += int64(len(p.raw)) + int64(len(p.ids))*8 + int64(len(p.ends))*4 + int64(len(p.vers))*4
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

// pageTransient returns page i for a single sequential pass over the container:
// a page that is already memoized is reused, but a cold page is decoded without
// being installed in the memo, so the pass does not pin every page it touches.
//
// Verify and the recovery rebuild are one-shot full-container walks: memoizing
// their pages would retain the whole decoded container (charged against the
// block cache, and displacing or duplicating the random-read hot set) for a
// pass that never revisits a page. The page is still owned and valid for the
// duration of the call; only the memo entry is skipped.
func (c *RowsContainer) pageTransient(i int) (*RowsPage, error) {
	if i < 0 || i >= len(c.Dir) {
		return nil, fmt.Errorf("rowpack: page index %d out of range (%d)", i, len(c.Dir))
	}
	if p := c.cachedPage(i); p != nil {
		return p, nil
	}
	return c.decodePage(i)
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
// page at a time (never the whole block at once). Pages are NOT memoized: this
// is the one-shot sequential accessor (verify / recovery rebuild), so pinning
// every page would charge the whole decoded container to the block cache for a
// pass that never revisits one. Bodies alias the page buffer and must not be
// retained beyond the callback.
// It is the recovery/rebuild/verify accessor.
func (c *RowsContainer) ForEach(fn func(codec.PageRecord) error) error {
	for i := range c.Dir {
		page, err := c.pageTransient(i)
		if err != nil {
			return err
		}
		if err := page.Records(fn); err != nil {
			return err
		}
	}
	return nil
}
