package block

import (
	"fmt"
	"sync/atomic"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// RowsContainer is a validated Rows Block page container:
//
//	[RowsBlockHeader][RowsPageDirEntry × N][stored page 0]…
//
// It wraps the container plaintext (header + directory + the independently
// compressed stored pages) without decompressing any page up front. Pages are
// decompressed on demand (RecordAtScratch / PageScratch) so a cold single-row
// read only decompresses the one page it needs, instead of the whole block.
//
// Ownership: RowsContainer owns its stored buffer. Each accessor that
// decompresses a page returns a release func returning the pooled scratch to
// the pool; the returned record views alias that scratch and are only valid
// until release is called.
type RowsContainer struct {
	Header fileformat.RowsBlockHeader
	Dir    []fileformat.RowsPageDirEntry

	comp   fileformat.Compression
	level  int
	limits Limits
	stored []byte // container plaintext: [0:RecordsRegionStart) header+dir, then pages

	// decompCounter is the reader's cumulative decompression counter; page
	// decompression (the actual work that used to be a whole-block decode)
	// is attributed to the reader so loader.readIOStats reports both disk
	// bytes pulled and page raw bytes produced.
	decompCounter *atomic.Uint64
}

// StoredLen returns the size of the container plaintext (used for cache
// accounting).
func (c *RowsContainer) StoredLen() int { return len(c.stored) }

// SetDecompCounter attaches the reader's decompression counter.
func (c *RowsContainer) SetDecompCounter(p *atomic.Uint64) { c.decompCounter = p }

// RecordsRegionStart is the byte offset (within stored) where the first page's
// stored bytes begin.
func (c *RowsContainer) RecordsRegionStart() int {
	return fileformat.RowsBlockHeaderSize + len(c.Dir)*fileformat.RowsPageDirEntrySize
}

// PageCount returns the number of pages.
func (c *RowsContainer) PageCount() int { return int(c.Header.PageCount) }

// ParseRowsContainer validates a Rows Block page container against its block
// header. It checks the container geometry, the aggregate header/directory
// CRC (BlockHeader.RawCRC32C), every page's bounds (inside the container,
// non-overlapping, within limits) and the logical TotalRecords == ItemCount.
// It never decompresses a page. Corruption is always an error, never a panic.
func ParseRowsContainer(container []byte, h fileformat.BlockHeader, limits Limits) (*RowsContainer, error) {
	if h.BlockKind != fileformat.BlockKindRows {
		return nil, fmt.Errorf("rowpack: block %d is kind %d, not rows", h.BlockID, h.BlockKind)
	}
	// h.StoredSize is the on-disk size; an encrypted block carries an extra
	// AEAD tag, so the container plaintext is StoredSize - tag.
	plainLen := int(h.StoredSize)
	if h.Encrypted {
		plainLen -= fileformat.AESGCMTagLen
	}
	if len(container) != plainLen {
		return nil, fmt.Errorf("rowpack: container %d bytes, want %d", len(container), plainLen)
	}
	if len(container) < fileformat.RowsBlockHeaderSize {
		return nil, fmt.Errorf("rowpack: container too short for header")
	}
	var rh fileformat.RowsBlockHeader
	if err := rh.Unmarshal(container[:fileformat.RowsBlockHeaderSize]); err != nil {
		return nil, err
	}
	// The header/directory region is authenticated by the block header CRC.
	dirEnd := fileformat.RowsBlockHeaderSize + int(rh.DirectoryBytes)
	if dirEnd > len(container) {
		return nil, fmt.Errorf("rowpack: container directory %d bytes overruns %d", rh.DirectoryBytes, len(container))
	}
	if fileformat.CRC32C(container[:dirEnd]) != h.RawCRC32C {
		return nil, fmt.Errorf("rowpack: block %d container header/dir CRC mismatch", h.BlockID)
	}
	if rh.TotalRecords != h.ItemCount {
		return nil, fmt.Errorf("rowpack: container total records %d != block item count %d", rh.TotalRecords, h.ItemCount)
	}
	if rh.DirectoryBytes != rh.PageCount*fileformat.RowsPageDirEntrySize {
		return nil, fmt.Errorf("rowpack: container directory %d != pageCount %d * %d", rh.DirectoryBytes, rh.PageCount, fileformat.RowsPageDirEntrySize)
	}
	dir := make([]fileformat.RowsPageDirEntry, rh.PageCount)
	pos := fileformat.RowsBlockHeaderSize
	for i := range dir {
		if pos+fileformat.RowsPageDirEntrySize > dirEnd {
			return nil, fmt.Errorf("rowpack: container directory truncated")
		}
		if err := dir[i].Unmarshal(container[pos : pos+fileformat.RowsPageDirEntrySize]); err != nil {
			return nil, err
		}
		pos += fileformat.RowsPageDirEntrySize
	}
	c := &RowsContainer{Header: rh, Dir: dir, comp: h.Compression, limits: limits, stored: container}
	// Validate page geometry: page 0 starts at RecordsRegionStart, pages are
	// laid out in order and must not overlap or escape the container; each
	// page's raw/stored sizes stay within the safety limits.
	expectedOff := c.RecordsRegionStart()
	firstOrd := uint32(0)
	for i := range dir {
		e := &dir[i]
		if int(e.PageOrdinal) != i {
			return nil, fmt.Errorf("rowpack: page %d ordinal %d out of order", i, e.PageOrdinal)
		}
		if e.FirstRecordOrdinal != firstOrd {
			return nil, fmt.Errorf("rowpack: page %d first ordinal %d, want %d", i, e.FirstRecordOrdinal, firstOrd)
		}
		if e.RecordCount == 0 {
			return nil, fmt.Errorf("rowpack: page %d has zero records", i)
		}
		if int(e.StoredOffset) != expectedOff {
			return nil, fmt.Errorf("rowpack: page %d stored offset %d, want %d", i, e.StoredOffset, expectedOff)
		}
		if e.StoredSize > limits.MaxStoredBytes {
			return nil, fmt.Errorf("rowpack: page %d stored size %d exceeds limit %d", i, e.StoredSize, limits.MaxStoredBytes)
		}
		if e.RawSize > limits.MaxRawBytes {
			return nil, fmt.Errorf("rowpack: page %d raw size %d exceeds limit %d", i, e.RawSize, limits.MaxRawBytes)
		}
		if int(e.StoredOffset)+int(e.StoredSize) > len(container) {
			return nil, fmt.Errorf("rowpack: page %d stored bytes escape container", i)
		}
		if h.Compression == fileformat.CompressionNone && e.StoredSize != e.RawSize {
			return nil, fmt.Errorf("rowpack: none-compressed page %d stored %d != raw %d", i, e.StoredSize, e.RawSize)
		}
		expectedOff += int(e.StoredSize)
		firstOrd += e.RecordCount
	}
	if expectedOff != len(container) {
		return nil, fmt.Errorf("rowpack: pages end at %d, container is %d", expectedOff, len(container))
	}
	if firstOrd != h.ItemCount {
		return nil, fmt.Errorf("rowpack: page records %d != item count %d", firstOrd, h.ItemCount)
	}
	return c, nil
}

// PageIndexForOrdinal returns the index of the page owning itemOrdinal, by
// binary search over the strictly-increasing FirstRecordOrdinal.
func (c *RowsContainer) PageIndexForOrdinal(ordinal uint32) (int, error) {
	return c.pageForOrdinal(ordinal)
}

// pageForOrdinal returns the index of the page owning itemOrdinal, by binary
// search over the strictly-increasing FirstRecordOrdinal.
func (c *RowsContainer) pageForOrdinal(ordinal uint32) (int, error) {
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

// decompressPageInto decompresses the stored page i into buf, returning the
// page's uncompressed bytes. It validates the decompressed length against the
// directory RawSize. buf must have been sized for at least RawSize.
func (c *RowsContainer) decompressPageInto(i int, buf *rawBuf) ([]byte, error) {
	dir := &c.Dir[i]
	stored := c.stored[int(dir.StoredOffset) : int(dir.StoredOffset)+int(dir.StoredSize)]
	var raw []byte
	if c.comp == fileformat.CompressionNone {
		if uint32(len(stored)) > c.limits.MaxRawBytes {
			return nil, fmt.Errorf("rowpack: page %d stored size %d exceeds limit %d", i, len(stored), c.limits.MaxRawBytes)
		}
		raw = buf.data[:len(stored)]
		copy(raw, stored)
	} else {
		var err error
		maxOut := c.limits.MaxRawBytes
		if dir.RawSize < maxOut {
			maxOut = dir.RawSize
		}
		raw, err = decompressZstd(buf.data, stored, maxOut)
		if err != nil {
			return nil, fmt.Errorf("rowpack: page %d: %w", i, err)
		}
	}
	if uint32(len(raw)) != dir.RawSize {
		return nil, fmt.Errorf("rowpack: page %d decompressed %d bytes, want %d", i, len(raw), dir.RawSize)
	}
	if c.decompCounter != nil {
		c.decompCounter.Add(uint64(len(raw)))
	}
	return raw, nil
}

// parsePage decompresses and validates page i, returning the parsed RowsPage
// whose views alias the caller's scratch buffer (released via the returned
// release func).
func (c *RowsContainer) parsePage(i int, buf *rawBuf) (*RowsPage, error) {
	raw, err := c.decompressPageInto(i, buf)
	if err != nil {
		return nil, err
	}
	p, err := ParseRowsPage(raw)
	if err != nil {
		return nil, err
	}
	if p.h.CRC32C != c.Dir[i].PageCRC32C {
		return nil, fmt.Errorf("rowpack: page %d header CRC %d != directory %d", i, p.h.CRC32C, c.Dir[i].PageCRC32C)
	}
	return p, nil
}

// PageScratch decompresses and validates page i into a pooled scratch. The
// returned *RowsPage and its release func bound the scratch lifetime: the
// page views (and any records yielded from it) are valid until release is
// called (exactly once). It is the batch/scan accessor, which decodes several
// records from one page.
func (c *RowsContainer) PageScratch(i int) (*RowsPage, func(), error) {
	if i < 0 || i >= len(c.Dir) {
		return nil, nil, fmt.Errorf("rowpack: page index %d out of range (%d)", i, len(c.Dir))
	}
	buf := getRawBuf(c.Dir[i].RawSize)
	p, err := c.parsePage(i, buf)
	if err != nil {
		putRawBuf(buf)
		return nil, nil, err
	}
	buf.data = p.raw
	release := func() { putRawBuf(buf) }
	return p, release, nil
}

// RecordAtScratch decodes one record by block item ordinal, decompressing only
// the containing page. The returned codec.PageRecord's Body aliases the pooled
// scratch; release must be called exactly once after the caller has consumed
// the record (e.g. decoded it). It is the Get / single-random-read accessor.
func (c *RowsContainer) RecordAtScratch(ordinal uint32) (codec.PageRecord, func(), error) {
	pi, err := c.pageForOrdinal(ordinal)
	if err != nil {
		return codec.PageRecord{}, nil, err
	}
	page, release, err := c.PageScratch(pi)
	if err != nil {
		return codec.PageRecord{}, nil, err
	}
	pageOrd := ordinal - c.Dir[pi].FirstRecordOrdinal
	rec, err := page.RecordAt(pageOrd)
	if err != nil {
		release()
		return codec.PageRecord{}, nil, err
	}
	return rec, release, nil
}

// ForEach iterates every record of every page in call order, decompressing
// one page at a time (never holding the whole block decompressed). Bodies
// alias transient page scratch and must not be retained beyond the callback.
// It is the recovery/rebuild/verify/scan accessor.
func (c *RowsContainer) ForEach(fn func(codec.PageRecord) error) error {
	for i := range c.Dir {
		page, release, err := c.PageScratch(i)
		if err != nil {
			return err
		}
		ierr := page.Records(fn)
		release()
		if ierr != nil {
			return ierr
		}
	}
	return nil
}
