package block

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/format"
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
	Header format.BlockHeader
	Stored []byte
	Raw    []byte
	Rows   []format.RowDirectoryEntry // rows blocks only
	Meta   []metadata.DirectoryEntry  // metadata blocks only
	// OversizedPages is the number of pages in this block that hold a single
	// record larger than the page target (Flags bit 0).
	OversizedPages uint32
}

// RowsBuilder accumulates body-only TypedTuple records of one
// (snapshot, table) and emits Rows Blocks as page containers (§6 of the
// refactor plan). Each block is:
//
//	[RowsBlockHeader][RowsPageDirEntry × N][stored page 0][stored page 1]…
//
// Records accumulate into an internal PageBuilder (pageSize target); a
// page is compressed into its own independently-decompressed stored page as
// soon as it fills, and the whole block flushes once the sum of page raw
// sizes reaches blockSize. A single record larger than pageSize is isolated
// as its own oversized page (Flags bit 0).
type RowsBuilder struct {
	snapshotID uint64
	tableID    uint32
	blockSize  int
	pageSize   int
	compress   format.Compression
	level      int
	limits     Limits

	// page is the current Rows Page accumulator; it is finished (and stored)
	// whenever it reaches pageSize.
	page *PageBuilder

	// entries carries one RowDirectoryEntry per buffered record in call order
	// (only RowID/SchemaVersion/ChangeType are meaningful in the page layout;
	// RecordOffset/RecordLength are unused). This exact slice is handed to
	// FlushedBlock.Rows for index building, so the writer's index build needs
	// no change.
	entries []format.RowDirectoryEntry

	// dirEntries + storedPages hold the finished stored pages of the current
	// block. rawSize is the Σ page raw sizes; recordOrdinal is the count of
	// records across finished pages (it seeds the next page's
	// FirstRecordOrdinal and equals len(entries) once the current page flushes).
	dirEntries    []format.RowsPageDirEntry
	storedPages   [][]byte
	rawSize       int
	recordOrdinal uint32
	// oversizedPages counts pages holding a single record larger than the
	// page target (Flags bit 0), reported per block via FlushedBlock.
	oversizedPages uint32

	// enc is the caller-owned zstd encoder (store-level, outliving GC pool
	// churn); nil selects the pooled encoder. encDst is its EncodeAll scratch,
	// held here for the same reason.
	enc    *ZstdEncoder
	encDst []byte

	// Flush returns each finished block; the consumer supplies the BlockID.
	onFlush func(*FlushedBlock) error
}

// Config is the shared configuration of the Rows and Metadata block
// builders: how large a block may grow, how it is compressed, and where the
// finished block goes. snapshotID/tableID are deliberately NOT part of it —
// they are the builder's identity, not its configuration.
type Config struct {
	BlockSize   int
	Compression format.Compression
	Level       int
	Limits      Limits
	OnFlush     func(*FlushedBlock) error
}

// NewRowsBuilder creates a builder for the given snapshot/table using
// the default page size (fileformat.DefaultPageSize); override it with
// SetPageSize before the first Add.
func NewRowsBuilder(snapshotID uint64, tableID uint32, cfg Config) *RowsBuilder {
	ps := format.DefaultPageSize
	if ps > cfg.BlockSize {
		ps = cfg.BlockSize
	}
	return &RowsBuilder{
		snapshotID: snapshotID,
		tableID:    tableID,
		blockSize:  cfg.BlockSize,
		pageSize:   ps,
		compress:   cfg.Compression,
		level:      cfg.Level,
		limits:     cfg.Limits,
		page:       NewPageBuilder(ps),
		onFlush:    cfg.OnFlush,
	}
}

// SetPageSize overrides the default page target. It must be called before the
// first Add while the current page is still empty; a page never exceeds the
// enclosing block target.
func (b *RowsBuilder) SetPageSize(n int) {
	if n <= 0 {
		return
	}
	if n > b.blockSize {
		n = b.blockSize
	}
	b.pageSize = n
	b.page = NewPageBuilder(n)
}

// SetZstdEncoder attaches a caller-owned zstd encoder used at Flush time
// instead of the pooled one. The caller owns the encoder's lifecycle and
// must keep it valid until the last Flush.
func (b *RowsBuilder) SetZstdEncoder(e *ZstdEncoder) { b.enc = e }

// Add appends one record (a body-only TypedTuple). The record is encoded into
// the current page; a page that reaches pageSize is compressed and stored.
// A record larger than pageSize occupies its own oversized page; a record
// larger than blockSize additionally flushes the current block first so it
// sits in a block by itself.
func (b *RowsBuilder) Add(rowID uint64, schemaVersion uint32, change format.ChangeType, row []byte) error {
	if uint32(len(row)) > b.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: row of %d bytes exceeds limit %d", len(row), b.limits.MaxRawBytes)
	}
	if len(row) > b.pageSize {
		// Single oversized row: flush the current page, then store this one
		// record as its own oversized page; a row that alone exceeds the
		// block target flushes the block so it is isolated.
		if err := b.finishPage(); err != nil {
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
		b.entries = append(b.entries, format.RowDirectoryEntry{RowID: rowID, SchemaVersion: schemaVersion, ChangeType: change})
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
		if err := b.finishPage(); err != nil {
			return err
		}
	}
	if err := b.page.Add(rowID, schemaVersion, change, row); err != nil {
		return err
	}
	b.entries = append(b.entries, format.RowDirectoryEntry{RowID: rowID, SchemaVersion: schemaVersion, ChangeType: change})
	if b.page.NeedsFlush() {
		if err := b.finishPage(); err != nil {
			return err
		}
		if b.rawSize >= b.blockSize {
			return b.Flush()
		}
	}
	return nil
}

// finishPage compresses and stores the current (non-empty) page, if
// any, appending its directory entry and updating the running counters.
func (b *RowsBuilder) finishPage() error {
	if b.page.countRows() == 0 {
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
func (b *RowsBuilder) storePage(rawPage []byte, oversized bool) error {
	var stored []byte
	var err error
	if b.enc != nil && b.compress == format.CompressionZstd {
		stored, err = EncodeZstdInto(b.enc, &b.encDst, rawPage)
	} else {
		stored, err = Compress(b.compress, b.level, rawPage)
	}
	if err != nil {
		return err
	}
	var h format.RowsPageHeader
	if err := h.Unmarshal(rawPage, len(rawPage)); err != nil {
		// A page this builder created can never be malformed; treat as a
		// coding error surfaced through the normal error path.
		return fmt.Errorf("rowpack: internal page re-parse failed: %w", err)
	}
	dir := format.RowsPageDirEntry{
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
		b.oversizedPages++
	}
	b.dirEntries = append(b.dirEntries, dir)
	b.storedPages = append(b.storedPages, stored)
	b.rawSize += len(rawPage)
	b.recordOrdinal += h.EntryCount
	return nil
}

// Flush emits the current pending pages as one page-container block, if any.
func (b *RowsBuilder) Flush() error {
	if len(b.entries) == 0 {
		return nil
	}
	if err := b.finishPage(); err != nil {
		return err
	}
	if len(b.dirEntries) == 0 {
		return nil
	}
	n := uint32(len(b.dirEntries))
	header := format.RowsBlockHeader{
		PageCount:      n,
		DirectoryBytes: n * format.RowsPageDirEntrySize,
		TotalRecords:   uint32(len(b.entries)),
	}
	// Resolve per-page StoredOffset now that the directory length is known
	// (the directory sits between the container header and the first page).
	dataStart := format.RowsBlockHeaderSize + int(n)*format.RowsPageDirEntrySize
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
	var hdr [format.RowsBlockHeaderSize]byte
	_ = header.MarshalTo(hdr[:])
	copy(container[0:format.RowsBlockHeaderSize], hdr[:])
	dir := container[format.RowsBlockHeaderSize:dataStart]
	for i := range b.dirEntries {
		var e [format.RowsPageDirEntrySize]byte
		_ = b.dirEntries[i].MarshalTo(e[:])
		copy(dir[i*format.RowsPageDirEntrySize:], e[:])
	}
	for i := range b.storedPages {
		start := int(b.dirEntries[i].StoredOffset)
		copy(container[start:start+len(b.storedPages[i])], b.storedPages[i])
	}
	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: b.compress,
		SnapshotID:  b.snapshotID,
		TableID:     b.tableID,
		ItemCount:   uint32(len(b.entries)),
		RawSize:     uint32(b.rawSize),
		StoredSize:  uint32(len(container)),
		RawCRC32C:   format.CRC32C(container[:dataStart]),
	}
	fb := &FlushedBlock{Header: h, Stored: container, Raw: container, Rows: b.entries, OversizedPages: b.oversizedPages}
	if err := b.onFlush(fb); err != nil {
		return err
	}
	// Directory ownership transferred to the callback; fresh inputs next block.
	b.entries = nil
	b.dirEntries = b.dirEntries[:0]
	b.storedPages = b.storedPages[:0]
	b.rawSize = 0
	b.recordOrdinal = 0
	b.oversizedPages = 0
	return nil
}
