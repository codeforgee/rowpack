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
type FlushedBlock struct {
	Header fileformat.BlockHeader
	Stored []byte
	Raw    []byte
	Rows   []fileformat.RowDirectoryEntry // rows blocks only
	Meta   []metadata.DirectoryEntry      // metadata blocks only
}

// RowsBlockBuilder accumulates row records of one (snapshot, table) and emits
// Rows Blocks, flushing at the target raw size and isolating any single row
// that exceeds it.
type RowsBlockBuilder struct {
	snapshotID uint64
	tableID    uint32
	blockSize  int
	compress   fileformat.Compression
	level      int
	limits     Limits

	entries []fileformat.RowDirectoryEntry
	records []byte // raw record bytes in call order
	count   uint32
	rawBuf  []byte // reused uncompressed payload scratch

	// Flush returns each finished block; the consumer supplies the BlockID.
	onFlush func(*FlushedBlock) error
}

// NewRowsBlockBuilder creates a builder for the given snapshot/table.
func NewRowsBlockBuilder(snapshotID uint64, tableID uint32, blockSize int, compress fileformat.Compression, level int, limits Limits, onFlush func(*FlushedBlock) error) *RowsBlockBuilder {
	return &RowsBlockBuilder{
		snapshotID: snapshotID,
		tableID:    tableID,
		blockSize:  blockSize,
		compress:   compress,
		level:      level,
		limits:     limits,
		onFlush:    onFlush,
	}
}

// Add appends one row record. The directory and record header are written in
// call order, and the record bytes are copied. A record whose encoded size
// reaches or exceeds the target block size is flushed as its own block.
func (b *RowsBlockBuilder) Add(rowID uint64, schemaVersion uint32, change fileformat.ChangeType, row []byte) error {
	if uint32(len(row)) > b.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: row of %d bytes exceeds limit %d", len(row), b.limits.MaxRawBytes)
	}
	recordLen := fileformat.RowRecordHeaderSize + len(row)
	if recordLen >= b.blockSize {
		// Single oversized row must occupy its own block.
		if err := b.Flush(); err != nil {
			return err
		}
		if err := b.append(rowID, schemaVersion, change, row); err != nil {
			return err
		}
		return b.Flush()
	}
	if err := b.append(rowID, schemaVersion, change, row); err != nil {
		return err
	}
	if len(b.records) >= b.blockSize {
		return b.Flush()
	}
	return nil
}

// Delete appends a DELETE tombstone record (no row payload).
func (b *RowsBlockBuilder) Delete(rowID uint64, schemaVersion uint32) error {
	return b.Add(rowID, schemaVersion, fileformat.ChangeDelete, nil)
}

func (b *RowsBlockBuilder) append(rowID uint64, schemaVersion uint32, change fileformat.ChangeType, row []byte) error {
	recordLen := fileformat.RowRecordHeaderSize + len(row)
	dir := fileformat.RowDirectoryEntry{
		RowID:         rowID,
		RecordOffset:  uint32(len(b.records)),
		RecordLength:  uint32(recordLen),
		ChangeType:    change,
		SchemaVersion: schemaVersion,
	}
	// Validate offsets fit.
	if dir.RecordOffset+dir.RecordLength > b.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: block payload exceeds limit %d", b.limits.MaxRawBytes)
	}
	var rh fileformat.RowRecordHeader
	rh.RowID = rowID
	rh.SchemaVersion = schemaVersion
	rh.ChangeType = change
	if change == fileformat.ChangeDelete {
		rh.RowEncoding = fileformat.RowEncodingNone
		rh.RowLength = 0
		rh.RowCRC32C = 0
	} else {
		rh.RowEncoding = fileformat.RowEncodingTypedTuple
		rh.RowLength = uint32(len(row))
		rh.RowCRC32C = fileformat.CRC32C(row)
	}
	var hdr [fileformat.RowRecordHeaderSize]byte
	if err := rh.MarshalTo(hdr[:]); err != nil {
		return err
	}
	b.entries = append(b.entries, dir)
	b.records = append(b.records, hdr[:]...)
	b.records = append(b.records, row...)
	b.count++
	return nil
}

// Pending returns the number of buffered records.
func (b *RowsBlockBuilder) Pending() int { return int(b.count) }

// Flush emits the current pending records as one block, if any.
func (b *RowsBlockBuilder) Flush() error {
	if b.count == 0 {
		return nil
	}
	raw := b.buildRawPayload()
	compressed, err := Compress(b.compress, b.level, raw)
	if err != nil {
		return err
	}
	_ = err
	h := fileformat.BlockHeader{
		BlockKind:   fileformat.BlockKindRows,
		Compression: b.compress,
		SnapshotID:  b.snapshotID,
		TableID:     b.tableID,
		ItemCount:   b.count,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   fileformat.CRC32C(raw),
	}
	if err := b.onFlush(&FlushedBlock{Header: h, Stored: compressed, Raw: raw, Rows: b.entries}); err != nil {
		return err
	}
	b.entries = b.entries[:0]
	b.records = b.records[:0]
	b.count = 0
	return nil
}

// buildRawPayload assembles the deterministic uncompressed Rows payload:
// RowsPayloadHeader + directory + records. The returned slice aliases the
// builder's reused scratch buffer: it is valid only until the next build, so
// Flush must consume it synchronously (compress + onFlush).
func (b *RowsBlockBuilder) buildRawPayload() []byte {
	dirBytes := len(b.entries) * fileformat.RowDirectoryEntrySize
	total := fileformat.RowsPayloadHeaderSize + dirBytes + len(b.records)
	if cap(b.rawBuf) < total {
		b.rawBuf = make([]byte, 0, total)
	}
	b.rawBuf = b.rawBuf[:0]
	raw := b.rawBuf
	h := fileformat.RowsPayloadHeader{
		ItemCount:      b.count,
		DirectoryBytes: uint32(dirBytes),
		RecordsBytes:   uint64(len(b.records)),
	}
	var hdr [fileformat.RowsPayloadHeaderSize]byte
	_ = h.MarshalTo(hdr[:])
	raw = append(raw, hdr[:]...)
	for i := range b.entries {
		var e [fileformat.RowDirectoryEntrySize]byte
		_ = b.entries[i].MarshalTo(e[:])
		raw = append(raw, e[:]...)
	}
	raw = append(raw, b.records...)
	return raw
}

// RawPayload builds the payload for the current pending records without
// flushing (used by tests to check determinism).
func (b *RowsBlockBuilder) RawPayload() []byte { return b.buildRawPayload() }

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
// to the single-record reader when needed.
func ParseRowsDirectory(raw []byte, itemCount uint32) (*RowsIndex, error) {
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
	idx := &RowsIndex{Header: h, raw: raw, recBase: base}
	pos := fileformat.RowsPayloadHeaderSize
	for i := uint32(0); i < h.ItemCount; i++ {
		if pos+fileformat.RowDirectoryEntrySize > base {
			return nil, errors.New("rowpack: rows directory truncated")
		}
		var e fileformat.RowDirectoryEntry
		if err := e.Unmarshal(raw[pos : pos+fileformat.RowDirectoryEntrySize]); err != nil {
			return nil, err
		}
		pos += fileformat.RowDirectoryEntrySize
		idx.Entries = append(idx.Entries, e)
	}
	// Validate every directory entry's record bounds once (still O(n), but
	// cheap and allocation-free).
	for i := range idx.Entries {
		e := &idx.Entries[i]
		if e.RecordLength < fileformat.RowRecordHeaderSize {
			return nil, fmt.Errorf("rowpack: row record %d too short", i)
		}
		if int(e.RecordOffset)+int(e.RecordLength) > int(h.RecordsBytes) {
			return nil, fmt.Errorf("rowpack: row record %d out of bounds", i)
		}
	}
	return idx, nil
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
// random reads cost O(1) in the block size.
func ParseRowAt(raw []byte, itemCount uint32, ordinal uint32) (*RowRef, error) {
	var h fileformat.RowsPayloadHeader
	if err := h.Unmarshal(raw); err != nil {
		return nil, err
	}
	if h.ItemCount != itemCount {
		return nil, fmt.Errorf("rowpack: payload item count %d != block item count %d", h.ItemCount, itemCount)
	}
	if ordinal >= itemCount {
		return nil, fmt.Errorf("rowpack: ordinal %d out of range (count %d)", ordinal, itemCount)
	}
	base := fileformat.RowsPayloadHeaderSize + int(h.DirectoryBytes)
	if base > len(raw) {
		return nil, errors.New("rowpack: rows payload directory exceeds payload")
	}
	if h.RecordsBytes > uint64(len(raw)-base) || base+int(h.RecordsBytes) != len(raw) {
		return nil, errors.New("rowpack: rows payload records region mismatch")
	}
	dirOff := fileformat.RowsPayloadHeaderSize + int(ordinal)*fileformat.RowDirectoryEntrySize
	if dirOff+fileformat.RowDirectoryEntrySize > base {
		return nil, errors.New("rowpack: rows directory truncated")
	}
	var e fileformat.RowDirectoryEntry
	if err := e.Unmarshal(raw[dirOff : dirOff+fileformat.RowDirectoryEntrySize]); err != nil {
		return nil, err
	}
	recOff := base + int(e.RecordOffset)
	recLen := int(e.RecordLength)
	if recOff < base || recOff+recLen > base+int(h.RecordsBytes) {
		return nil, fmt.Errorf("rowpack: row record out of bounds")
	}
	rec := raw[recOff : recOff+recLen]
	var rh fileformat.RowRecordHeader
	if err := rh.Unmarshal(rec); err != nil {
		return nil, err
	}
	if rh.RowID != e.RowID || rh.ChangeType != e.ChangeType || rh.SchemaVersion != e.SchemaVersion {
		return nil, fmt.Errorf("rowpack: row record header mismatch with directory")
	}
	if uint32(len(rec)) != fileformat.RowRecordHeaderSize+rh.RowLength {
		return nil, fmt.Errorf("rowpack: row record length mismatch")
	}
	if rh.ChangeType == fileformat.ChangeDelete {
		if rh.RowEncoding != fileformat.RowEncodingNone || rh.RowLength != 0 || rh.RowCRC32C != 0 {
			return nil, fmt.Errorf("rowpack: DELETE record carries row bytes")
		}
	} else if rh.RowEncoding != fileformat.RowEncodingTypedTuple {
		return nil, fmt.Errorf("rowpack: record has row encoding %d", rh.RowEncoding)
	}
	ref := &RowRef{Entry: e, Header: rh}
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
