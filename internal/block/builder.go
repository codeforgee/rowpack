package block

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/metadata"
)

// FlushedBlock is the result of one block flush, handed to the onFlush
// callback. Stored is the compressed (or plain) payload written to disk; Raw
// is the validated uncompressed payload; Rows/Meta carry the already-built
// directory entries so callers never need to re-parse the payload to extract
// index information.
//
// Ownership: Rows (rows blocks) transfer to the callback — the builder
// allocates a fresh directory slice for its next block, so the callback may
// retain Rows beyond the call at no copy cost. Raw and Meta (metadata
// blocks) are builder-owned scratch: they alias reused buffers and are only
// valid during the call, so a callback that needs them later must copy.
type FlushedBlock struct {
	Header fileformat.BlockHeader
	Stored []byte
	Raw    []byte
	Rows   []fileformat.RowDirectoryEntry // rows blocks only
	Meta   []metadata.DirectoryEntry      // metadata blocks only
}

// RowsBlockBuilder accumulates body-only TypedTuple records of one
// (snapshot, table) and emits Rows Blocks as page containers (§6 of the
// refactor plan). Each block is:
//
//	[RowsBlockHeader][RowsPageDirEntry × N][stored page 0][stored page 1]…
//
// Records accumulate into an internal RowsPageBuilder (pageSize target); a
// page is compressed into its own independently-decompressed stored page as
// soon as it fills, and the whole block flushes once the sum of page raw
// sizes reaches blockSize. A single record larger than pageSize is isolated
// as its own oversized page (Flags bit 0).
type RowsBlockBuilder struct {
	snapshotID uint64
	tableID    uint32
	blockSize  int
	pageSize   int
	compress   fileformat.Compression
	level      int
	limits     Limits

	// page is the current Rows Page accumulator; it is finished (and stored)
	// whenever it reaches pageSize.
	page *RowsPageBuilder

	// entries carries one RowDirectoryEntry per buffered record in call order
	// (only RowID/SchemaVersion/ChangeType are meaningful in the page layout;
	// RecordOffset/RecordLength are unused). This exact slice is handed to
	// FlushedBlock.Rows for index building, so the writer's index build needs
	// no change.
	entries []fileformat.RowDirectoryEntry

	// dirEntries + storedPages hold the finished stored pages of the current
	// block. rawSize is the Σ page raw sizes; recordOrdinal is the count of
	// records across finished pages (it seeds the next page's
	// FirstRecordOrdinal and equals len(entries) once the current page flushes).
	dirEntries    []fileformat.RowsPageDirEntry
	storedPages   [][]byte
	rawSize       int
	recordOrdinal uint32

	// enc is the caller-owned zstd encoder (store-level, outliving GC pool
	// churn); nil selects the pooled encoder.
	enc *ZstdEncoder

	// Flush returns each finished block; the consumer supplies the BlockID.
	onFlush func(*FlushedBlock) error
}

// NewRowsBlockBuilder creates a builder for the given snapshot/table using
// the default page size (fileformat.DefaultPageSize); override it with
// SetPageSize before the first Add.
func NewRowsBlockBuilder(snapshotID uint64, tableID uint32, blockSize int, compress fileformat.Compression, level int, limits Limits, onFlush func(*FlushedBlock) error) *RowsBlockBuilder {
	ps := fileformat.DefaultPageSize
	if ps > blockSize {
		ps = blockSize
	}
	return &RowsBlockBuilder{
		snapshotID: snapshotID,
		tableID:    tableID,
		blockSize:  blockSize,
		pageSize:   ps,
		compress:   compress,
		level:      level,
		limits:     limits,
		page:       NewRowsPageBuilder(ps),
		onFlush:    onFlush,
	}
}

// SetPageSize overrides the default page target. It must be called before the
// first Add while the current page is still empty; a page never exceeds the
// enclosing block target.
func (b *RowsBlockBuilder) SetPageSize(n int) {
	if n <= 0 {
		return
	}
	if n > b.blockSize {
		n = b.blockSize
	}
	b.pageSize = n
	b.page = NewRowsPageBuilder(n)
}

// SetZstdEncoder attaches a caller-owned zstd encoder used at Flush time
// instead of the pooled one. The caller owns the encoder's lifecycle and
// must keep it valid until the last Flush.
func (b *RowsBlockBuilder) SetZstdEncoder(e *ZstdEncoder) { b.enc = e }

// Add appends one record (a body-only TypedTuple). The record is encoded into
// the current page; a page that reaches pageSize is compressed and stored.
// A record larger than pageSize occupies its own oversized page; a record
// larger than blockSize additionally flushes the current block first so it
// sits in a block by itself.
func (b *RowsBlockBuilder) Add(rowID uint64, schemaVersion uint32, change fileformat.ChangeType, row []byte) error {
	if uint32(len(row)) > b.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: row of %d bytes exceeds limit %d", len(row), b.limits.MaxRawBytes)
	}
	if len(row) > b.pageSize {
		// Single oversized row: flush the current page, then store this one
		// record as its own oversized page; a row that alone exceeds the
		// block target flushes the block so it is isolated.
		if err := b.finishCurrentPage(); err != nil {
			return err
		}
		if len(row) >= b.blockSize {
			if err := b.Flush(); err != nil {
				return err
			}
		}
		if err := b.page.Add(rowID, schemaVersion, change, row); err != nil {
			return err
		}
		b.entries = append(b.entries, fileformat.RowDirectoryEntry{RowID: rowID, SchemaVersion: schemaVersion, ChangeType: change})
		rawPage, err := b.page.Finish()
		if err != nil {
			return err
		}
		if err := b.storePage(rawPage, true); err != nil {
			return err
		}
		if b.rawSize >= b.blockSize {
			return b.Flush()
		}
		return nil
	}
	if b.page.NeedsFlush() {
		if err := b.finishCurrentPage(); err != nil {
			return err
		}
	}
	if err := b.page.Add(rowID, schemaVersion, change, row); err != nil {
		return err
	}
	b.entries = append(b.entries, fileformat.RowDirectoryEntry{RowID: rowID, SchemaVersion: schemaVersion, ChangeType: change})
	if b.page.NeedsFlush() {
		if err := b.finishCurrentPage(); err != nil {
			return err
		}
		if b.rawSize >= b.blockSize {
			return b.Flush()
		}
	}
	return nil
}

// Delete appends a DELETE tombstone record (no row payload).
func (b *RowsBlockBuilder) Delete(rowID uint64, schemaVersion uint32) error {
	return b.Add(rowID, schemaVersion, fileformat.ChangeDelete, nil)
}

// Pending returns the number of buffered records (finished + current page).
func (b *RowsBlockBuilder) Pending() int { return len(b.entries) }

// finishCurrentPage compresses and stores the current (non-empty) page, if
// any, appending its directory entry and updating the running counters.
func (b *RowsBlockBuilder) finishCurrentPage() error {
	if b.page.Count() == 0 {
		return nil
	}
	rawPage, err := b.page.Finish()
	if err != nil {
		return err
	}
	return b.storePage(rawPage, false)
}

// storePage compresses one finished (uncompressed) page and records its
// directory entry. StoredOffset is resolved at block assembly time, once the
// directory length is known.
func (b *RowsBlockBuilder) storePage(rawPage []byte, oversized bool) error {
	var stored []byte
	var err error
	if b.enc != nil && b.compress == fileformat.CompressionZstd {
		stored, err = EncodeZstdWith(b.enc, rawPage)
	} else {
		stored, err = Compress(b.compress, b.level, rawPage)
	}
	if err != nil {
		return err
	}
	var h fileformat.RowsPageHeader
	if err := h.Unmarshal(rawPage, len(rawPage)); err != nil {
		// A page this builder created can never be malformed; treat as a
		// coding error surfaced through the normal error path.
		return fmt.Errorf("rowpack: internal page re-parse failed: %w", err)
	}
	dir := fileformat.RowsPageDirEntry{
		PageOrdinal:        uint32(len(b.dirEntries)),
		FirstRecordOrdinal: b.recordOrdinal,
		RecordCount:        h.EntryCount,
		StoredSize:         uint32(len(stored)),
		RawSize:            uint32(len(rawPage)),
		MinRowID:           h.MinRowID,
		MaxRowID:           h.MaxRowID,
		PageCRC32C:         h.CRC32C,
	}
	if oversized {
		dir.Flags = 1
	}
	b.dirEntries = append(b.dirEntries, dir)
	b.storedPages = append(b.storedPages, stored)
	b.rawSize += len(rawPage)
	b.recordOrdinal += h.EntryCount
	return nil
}

// Flush emits the current pending pages as one page-container block, if any.
func (b *RowsBlockBuilder) Flush() error {
	if len(b.entries) == 0 {
		return nil
	}
	if err := b.finishCurrentPage(); err != nil {
		return err
	}
	if len(b.dirEntries) == 0 {
		return nil
	}
	n := uint32(len(b.dirEntries))
	header := fileformat.RowsBlockHeader{
		PageCount:      n,
		DirectoryBytes: n * fileformat.RowsPageDirEntrySize,
		TotalRecords:   uint32(len(b.entries)),
	}
	// Resolve per-page StoredOffset now that the directory length is known
	// (the directory sits between the container header and the first page).
	dataStart := fileformat.RowsBlockHeaderSize + int(n)*fileformat.RowsPageDirEntrySize
	off := dataStart
	for i := range b.dirEntries {
		b.dirEntries[i].StoredOffset = uint64(off)
		off += int(b.dirEntries[i].StoredSize)
	}
	// The container is the block payload and must remain valid until commit
	// writes it (the onFlush callback retains it), so it is a fresh
	// allocation per block rather than a reused scratch. make zeroes it, so
	// no clearContainer pass is needed.
	container := make([]byte, off)
	var hdr [fileformat.RowsBlockHeaderSize]byte
	_ = header.MarshalTo(hdr[:])
	copy(container[0:fileformat.RowsBlockHeaderSize], hdr[:])
	dir := container[fileformat.RowsBlockHeaderSize:dataStart]
	for i := range b.dirEntries {
		var e [fileformat.RowsPageDirEntrySize]byte
		_ = b.dirEntries[i].MarshalTo(e[:])
		copy(dir[i*fileformat.RowsPageDirEntrySize:], e[:])
	}
	for i := range b.storedPages {
		start := int(b.dirEntries[i].StoredOffset)
		copy(container[start:start+len(b.storedPages[i])], b.storedPages[i])
	}
	h := fileformat.BlockHeader{
		BlockKind:   fileformat.BlockKindRows,
		Compression: b.compress,
		SnapshotID:  b.snapshotID,
		TableID:     b.tableID,
		ItemCount:   uint32(len(b.entries)),
		RawSize:     uint32(b.rawSize),
		StoredSize:  uint32(len(container)),
		RawCRC32C:   fileformat.CRC32C(container[:dataStart]),
	}
	if err := b.onFlush(&FlushedBlock{Header: h, Stored: container, Raw: container, Rows: b.entries}); err != nil {
		return err
	}
	// Directory ownership transferred to the callback; fresh inputs next block.
	b.entries = nil
	b.dirEntries = b.dirEntries[:0]
	b.storedPages = b.storedPages[:0]
	b.rawSize = 0
	b.recordOrdinal = 0
	return nil
}

// RowsPayload is a validated uncompressed Rows block payload.
type RowsPayload struct {
	Header  fileformat.RowsPayloadHeader
	Entries []fileformat.RowDirectoryEntry
	Records [][]byte // record bytes (header+body) in entry order
}

// ParseRowsPayload validates an uncompressed Rows payload against the block
// header's item count, directory/record consistency and record-header
// agreement. It never allocates more than the input length.
func ParseRowsPayload(raw []byte, itemCount uint32) (*RowsPayload, error) {
	var h fileformat.RowsPayloadHeader
	if err := h.Unmarshal(raw); err != nil {
		return nil, err
	}
	if h.ItemCount != itemCount {
		return nil, fmt.Errorf("rowpack: payload item count %d != block item count %d", h.ItemCount, itemCount)
	}
	base := fileformat.RowsPayloadHeaderSize + int(h.DirectoryBytes)
	if base > len(raw) {
		return nil, errors.New("rowpack: rows payload directory exceeds payload")
	}
	if h.RecordsBytes > uint64(len(raw)-base) {
		return nil, errors.New("rowpack: rows payload records exceed payload")
	}
	if base+int(h.RecordsBytes) != len(raw) {
		return nil, fmt.Errorf("rowpack: rows payload %d bytes, want %d", len(raw), base+int(h.RecordsBytes))
	}
	p := &RowsPayload{Header: h}
	pos := fileformat.RowsPayloadHeaderSize
	for i := uint32(0); i < h.ItemCount; i++ {
		if pos+fileformat.RowDirectoryEntrySize > len(raw) {
			return nil, errors.New("rowpack: rows directory truncated")
		}
		var e fileformat.RowDirectoryEntry
		if err := e.Unmarshal(raw[pos : pos+fileformat.RowDirectoryEntrySize]); err != nil {
			return nil, err
		}
		pos += fileformat.RowDirectoryEntrySize
		p.Entries = append(p.Entries, e)
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		off := int(e.RecordOffset)
		ln := int(e.RecordLength)
		if off < 0 || ln < 0 || off+ln > int(h.RecordsBytes) {
			return nil, fmt.Errorf("rowpack: row record %d out of bounds", i)
		}
		rec := raw[base+off : base+off+ln]
		var rh fileformat.RowRecordHeader
		if err := rh.Unmarshal(rec); err != nil {
			return nil, err
		}
		// Directory and record header must agree.
		if rh.RowID != e.RowID || rh.ChangeType != e.ChangeType || rh.SchemaVersion != e.SchemaVersion {
			return nil, fmt.Errorf("rowpack: row record %d header mismatch with directory", i)
		}
		if uint32(len(rec)) != fileformat.RowRecordHeaderSize+rh.RowLength {
			return nil, fmt.Errorf("rowpack: row record %d length mismatch", i)
		}
		if rh.ChangeType == fileformat.ChangeDelete {
			if rh.RowEncoding != fileformat.RowEncodingNone || rh.RowLength != 0 || rh.RowCRC32C != 0 {
				return nil, fmt.Errorf("rowpack: DELETE record %d carries row bytes", i)
			}
		} else if rh.RowEncoding != fileformat.RowEncodingTypedTuple {
			return nil, fmt.Errorf("rowpack: record %d has row encoding %d", i, rh.RowEncoding)
		}
		p.Records = append(p.Records, rec)
	}
	return p, nil
}

// RowsIndex is a lightweight view of a rows payload: the payload header and
// directory entries only. Record bytes are sliced on demand, so building the
// index costs O(ItemCount) without parsing every record header or allocating
// per-record slices. Used by sequential scans where the caller walks the
// directory in order.
type RowsIndex struct {
	Header  fileformat.RowsPayloadHeader
	Entries []fileformat.RowDirectoryEntry
	raw     []byte // entire payload
	recBase int    // records region start
}

// ParseRowsDirectory validates a rows payload's header and directory region
// and returns the directory index. Unlike ParseRowsPayload it does not parse
// record headers or materialize record slices. The block's RawCRC already
// guards payload integrity, so the per-record header cross-check is deferred
// to the single-record reader when needed. When cap(entries) covers
// itemCount the slice is reused (each entry is overwritten), avoiding a
// per-block allocation on sequential scans; the returned RowsIndex aliases
// entries, so the caller must not reuse entries until it is done with the
// index. Pass nil to allocate a fresh slice.
func ParseRowsDirectory(raw []byte, itemCount uint32, entries []fileformat.RowDirectoryEntry) (*RowsIndex, error) {
	var h fileformat.RowsPayloadHeader
	if err := h.Unmarshal(raw); err != nil {
		return nil, err
	}
	if h.ItemCount != itemCount {
		return nil, fmt.Errorf("rowpack: payload item count %d != block item count %d", h.ItemCount, itemCount)
	}
	base := fileformat.RowsPayloadHeaderSize + int(h.DirectoryBytes)
	if base > len(raw) {
		return nil, errors.New("rowpack: rows payload directory exceeds payload")
	}
	if h.RecordsBytes > uint64(len(raw)-base) || base+int(h.RecordsBytes) != len(raw) {
		return nil, errors.New("rowpack: rows payload records region mismatch")
	}
	if cap(entries) < int(itemCount) {
		entries = make([]fileformat.RowDirectoryEntry, itemCount)
	}
	entries = entries[:itemCount]
	pos := fileformat.RowsPayloadHeaderSize
	for i := uint32(0); i < h.ItemCount; i++ {
		if pos+fileformat.RowDirectoryEntrySize > base {
			return nil, errors.New("rowpack: rows directory truncated")
		}
		if err := entries[i].Unmarshal(raw[pos : pos+fileformat.RowDirectoryEntrySize]); err != nil {
			return nil, err
		}
		pos += fileformat.RowDirectoryEntrySize
	}
	// Validate every directory entry's record bounds once (still O(n), but
	// cheap and allocation-free).
	for i := range entries {
		e := &entries[i]
		if e.RecordLength < fileformat.RowRecordHeaderSize {
			return nil, fmt.Errorf("rowpack: row record %d too short", i)
		}
		if int(e.RecordOffset)+int(e.RecordLength) > int(h.RecordsBytes) {
			return nil, fmt.Errorf("rowpack: row record %d out of bounds", i)
		}
	}
	return &RowsIndex{Header: h, Entries: entries, raw: raw, recBase: base}, nil
}

// RowBytes returns the row payload bytes of the record at ordinal (nil for
// DELETE). It slices on demand; the result aliases the index's raw buffer.
func (r *RowsIndex) RowBytes(ordinal int) []byte {
	e := &r.Entries[ordinal]
	rec := r.raw[r.recBase+int(e.RecordOffset) : r.recBase+int(e.RecordOffset)+int(e.RecordLength)]
	if e.ChangeType == fileformat.ChangeDelete {
		return nil
	}
	return rec[fileformat.RowRecordHeaderSize:]
}

// RowRef locates one record within a validated rows payload.
type RowRef struct {
	Entry  fileformat.RowDirectoryEntry
	Header fileformat.RowRecordHeader
	// Row is the row payload bytes (nil for DELETE). It references the
	// caller's raw buffer and must not outlive it.
	Row []byte
}

// ParseRowAt validates the rows payload header and the single record at
// ordinal, returning its directory entry, record header and row payload.
// Unlike ParseRowsPayload it does not parse the whole directory, so single-row
// random reads cost O(1) in the block size. RowRef is returned by value: hot
// random reads call this per row, and a heap-escaped pointer would cost one
// allocation per Get.
func ParseRowAt(raw []byte, itemCount uint32, ordinal uint32) (RowRef, error) {
	var h fileformat.RowsPayloadHeader
	if err := h.Unmarshal(raw); err != nil {
		return RowRef{}, err
	}
	if h.ItemCount != itemCount {
		return RowRef{}, fmt.Errorf("rowpack: payload item count %d != block item count %d", h.ItemCount, itemCount)
	}
	if ordinal >= itemCount {
		return RowRef{}, fmt.Errorf("rowpack: ordinal %d out of range (count %d)", ordinal, itemCount)
	}
	base := fileformat.RowsPayloadHeaderSize + int(h.DirectoryBytes)
	if base > len(raw) {
		return RowRef{}, errors.New("rowpack: rows payload directory exceeds payload")
	}
	if h.RecordsBytes > uint64(len(raw)-base) || base+int(h.RecordsBytes) != len(raw) {
		return RowRef{}, errors.New("rowpack: rows payload records region mismatch")
	}
	dirOff := fileformat.RowsPayloadHeaderSize + int(ordinal)*fileformat.RowDirectoryEntrySize
	if dirOff+fileformat.RowDirectoryEntrySize > base {
		return RowRef{}, errors.New("rowpack: rows directory truncated")
	}
	var e fileformat.RowDirectoryEntry
	if err := e.Unmarshal(raw[dirOff : dirOff+fileformat.RowDirectoryEntrySize]); err != nil {
		return RowRef{}, err
	}
	recOff := base + int(e.RecordOffset)
	recLen := int(e.RecordLength)
	if recOff < base || recOff+recLen > base+int(h.RecordsBytes) {
		return RowRef{}, fmt.Errorf("rowpack: row record out of bounds")
	}
	rec := raw[recOff : recOff+recLen]
	var rh fileformat.RowRecordHeader
	if err := rh.Unmarshal(rec); err != nil {
		return RowRef{}, err
	}
	if rh.RowID != e.RowID || rh.ChangeType != e.ChangeType || rh.SchemaVersion != e.SchemaVersion {
		return RowRef{}, fmt.Errorf("rowpack: row record header mismatch with directory")
	}
	if uint32(len(rec)) != fileformat.RowRecordHeaderSize+rh.RowLength {
		return RowRef{}, fmt.Errorf("rowpack: row record length mismatch")
	}
	if rh.ChangeType == fileformat.ChangeDelete {
		if rh.RowEncoding != fileformat.RowEncodingNone || rh.RowLength != 0 || rh.RowCRC32C != 0 {
			return RowRef{}, fmt.Errorf("rowpack: DELETE record carries row bytes")
		}
	} else if rh.RowEncoding != fileformat.RowEncodingTypedTuple {
		return RowRef{}, fmt.Errorf("rowpack: record has row encoding %d", rh.RowEncoding)
	}
	ref := RowRef{Entry: e, Header: rh}
	if rh.ChangeType != fileformat.ChangeDelete {
		ref.Row = rec[fileformat.RowRecordHeaderSize:]
	}
	return ref, nil
}

// RowBytes returns the row payload bytes of record i (nil for DELETE).
func (p *RowsPayload) RowBytes(i int) []byte {
	rec := p.Records[i]
	if p.Entries[i].ChangeType == fileformat.ChangeDelete {
		return nil
	}
	return rec[fileformat.RowRecordHeaderSize:]
}

// RowCRC returns the stored row CRC of record i.
func (p *RowsPayload) RowCRC(i int) uint32 {
	var rh fileformat.RowRecordHeader
	_ = rh.Unmarshal(p.Records[i])
	return rh.RowCRC32C
}

var _ = binary.LittleEndian
