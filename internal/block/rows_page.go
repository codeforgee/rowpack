package block

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// Rows Page v2 in-memory builder/reader (S2 prototype).
//
// The builder accumulates per-record metadata into column streams
// (RowID deltas, end-offset deltas, schema-version RLE, 2-bit change types)
// and appends body-only TypedTuple bytes, flushing a page when the raw size
// reaches the builder's target. The reader parses a validated page and
// decodes records by ordinal (stream walk, O(ordinal)) or sequentially.
//
// Format: internal/fileformat/rows_page.go — the page CRC covers the
// uncompressed streams; compression/encryption wrap the whole page at the
// block layer exactly like v1 blocks.

// errPageTruncated is returned when a stream read runs past its end.
var errPageTruncated = errors.New("rowpack: page stream truncated")

// PageBuilder accumulates records until Finish produces one page.
type PageBuilder struct {
	target int

	rowIDs     []byte
	offsets    []byte
	schemaRLE  []byte
	changeBits []byte
	tuples     []byte

	lastRowID uint64
	lastEnd   uint32

	curSchema uint32
	curRun    uint32

	firstRowID uint64
	minRowID   uint64
	maxRowID   uint64
	count      uint32
	started    bool
}

// NewPageBuilder creates a page builder flushing at target raw bytes.
// A single record larger than target still forms an (oversized) page.
func NewPageBuilder(targetBytes int) *PageBuilder {
	return &PageBuilder{target: targetBytes}
}

// Add appends one record. tuple is the body-only TypedTuple encoding
// (codec.EncodeInto); for deletes pass nil or an empty slice. Call-order
// semantics are preserved exactly like the v1 change stream; RowIDs need not
// be sorted.
func (b *PageBuilder) Add(rowID uint64, schemaVersion uint32, changeType fileformat.ChangeType, tuple []byte) error {
	packed, err := fileformat.PackChangeType(changeType)
	if err != nil {
		return err
	}
	// RowID stream: first absolute, then zigzag delta.
	if !b.started {
		b.rowIDs = binary.AppendUvarint(b.rowIDs, rowID)
	} else {
		d := int64(rowID - b.lastRowID)
		b.rowIDs = binary.AppendUvarint(b.rowIDs, uint64(d<<1)^uint64(d>>63))
	}
	// End-offset stream: delta of the record end within the tuples region.
	// The first record's delta is its absolute length; deletes (empty bodies)
	// repeat the previous end (delta 0).
	b.offsets = binary.AppendUvarint(b.offsets, uint64(len(tuple)))
	b.lastEnd += uint32(len(tuple))
	// Schema version RLE: accumulate the run, emit pairs on change/finish.
	if b.curRun == 0 || b.curSchema != schemaVersion {
		b.flushRun()
		b.curSchema = schemaVersion
		b.curRun = 1
	} else {
		b.curRun++
	}
	// Change-type 2-bit stream.
	b.changeBits = appendChange(b.changeBits, b.count, packed)
	// Tuple body.
	b.tuples = append(b.tuples, tuple...)

	if !b.started {
		b.firstRowID, b.minRowID, b.maxRowID, b.started = rowID, rowID, rowID, true
	} else {
		if rowID < b.minRowID {
			b.minRowID = rowID
		}
		if rowID > b.maxRowID {
			b.maxRowID = rowID
		}
	}
	b.lastRowID = rowID
	b.count++
	return nil
}

// RawBytes returns the current uncompressed page size if flushed now.
func (b *PageBuilder) RawBytes() int {
	return fileformat.RowsPageHeaderSize + len(b.rowIDs) + len(b.offsets) +
		len(b.schemaRLE) + len(b.changeBits) + len(b.tuples)
}

// Count returns the number of buffered records.
func (b *PageBuilder) Count() uint32 { return b.count }

// NeedsFlush reports whether the pending page has reached the target size.
func (b *PageBuilder) NeedsFlush() bool {
	return b.count > 0 && b.RawBytes() >= b.target
}

// Finish serializes the buffered records into one page and resets the
// builder. Returns nil when nothing is buffered.
func (b *PageBuilder) Finish() ([]byte, error) {
	if b.count == 0 {
		return nil, nil
	}
	b.flushRun()

	h := fileformat.RowsPageHeader{
		EntryCount:      b.count,
		RowIDsBytes:     uint32(len(b.rowIDs)),
		OffsetsBytes:    uint32(len(b.offsets)),
		SchemaRLEBytes:  uint32(len(b.schemaRLE)),
		ChangeBitsBytes: uint32(len(b.changeBits)),
		TuplesBytes:     uint32(len(b.tuples)),
		FirstRowID:      b.firstRowID,
		MinRowID:        b.minRowID,
		MaxRowID:        b.maxRowID,
	}
	page := make([]byte, 0, fileformat.RowsPageHeaderSize+h.StreamsBytes())
	page = append(page, make([]byte, fileformat.RowsPageHeaderSize)...)
	page = append(page, b.rowIDs...)
	page = append(page, b.offsets...)
	page = append(page, b.schemaRLE...)
	page = append(page, b.changeBits...)
	page = append(page, b.tuples...)
	streams := page[fileformat.RowsPageHeaderSize:]
	h.CRC32C = fileformat.CRC32C(streams)
	if err := h.MarshalTo(page); err != nil {
		return nil, err
	}
	b.reset()
	return page, nil
}

// Reset clears all buffers for reuse.
func (b *PageBuilder) Reset() { b.reset() }

func (b *PageBuilder) reset() {
	b.rowIDs = b.rowIDs[:0]
	b.offsets = b.offsets[:0]
	b.schemaRLE = b.schemaRLE[:0]
	b.changeBits = b.changeBits[:0]
	b.tuples = b.tuples[:0]
	b.lastRowID, b.lastEnd, b.firstRowID = 0, 0, 0
	b.curSchema, b.curRun = 0, 0
	b.count = 0
	b.started = false
}

func (b *PageBuilder) flushRun() {
	if b.curRun == 0 {
		return
	}
	b.schemaRLE = binary.AppendUvarint(b.schemaRLE, uint64(b.curSchema))
	b.schemaRLE = binary.AppendUvarint(b.schemaRLE, uint64(b.curRun))
	b.curRun = 0
}

// appendChange sets the 2-bit value for entry ordinal i, growing the bit
// stream as needed.
func appendChange(dst []byte, ordinal uint32, packed uint8) []byte {
	byteIdx := ordinal / 4
	for uint32(len(dst)) <= byteIdx {
		dst = append(dst, 0)
	}
	shift := (ordinal % 4) * 2
	dst[byteIdx] |= packed << shift
	return dst
}

// RowsPage is a parsed, CRC-validated page. All views alias raw; callers must
// not retain them beyond the page's lifetime. A random-access record index
// (ids/ends/vers, one entry per record) is built at parse time so RecordAt is
// O(1) instead of an O(ordinal) stream walk; the page is immutable afterwards,
// so memoized pages can be shared between readers without a lock.
type RowsPage struct {
	h     fileformat.RowsPageHeader
	raw   []byte
	start int // streams region start within raw (= fileformat.RowsPageHeaderSize)

	rowIDs     []byte
	offsets    []byte
	schemaRLE  []byte
	changeBits []byte
	tuples     []byte

	// record index (len == EntryCount): ids[i] = record RowID, ends[i] =
	// cumulative tuple-body end offset, vers[i] = schema version.
	ids  []uint64
	ends []uint32
	vers []uint32
}

// ParseRowsPage validates the header geometry and the page CRC, then hands
// back stream views. Corruption is always an error, never a panic.
func ParseRowsPage(raw []byte) (*RowsPage, error) {
	var h fileformat.RowsPageHeader
	if err := h.Unmarshal(raw, len(raw)); err != nil {
		return nil, err
	}
	start := fileformat.RowsPageHeaderSize
	off := start
	rowIDs := raw[off : off+int(h.RowIDsBytes)]
	off += int(h.RowIDsBytes)
	offsets := raw[off : off+int(h.OffsetsBytes)]
	off += int(h.OffsetsBytes)
	schemaRLE := raw[off : off+int(h.SchemaRLEBytes)]
	off += int(h.SchemaRLEBytes)
	changeBits := raw[off : off+int(h.ChangeBitsBytes)]
	off += int(h.ChangeBitsBytes)
	tuples := raw[off : off+int(h.TuplesBytes)]
	if fileformat.CRC32C(raw[start:]) != h.CRC32C {
		return nil, fmt.Errorf("rowpack: rows page %d records CRC mismatch", h.EntryCount)
	}
	if err := validateChangeBits(changeBits, h.EntryCount); err != nil {
		return nil, err
	}
	p := &RowsPage{
		h: h, raw: raw, start: start,
		rowIDs: rowIDs, offsets: offsets, schemaRLE: schemaRLE,
		changeBits: changeBits, tuples: tuples,
	}
	if err := p.buildIndex(); err != nil {
		return nil, err
	}
	return p, nil
}

// buildIndex decodes the column streams once into the per-record arrays used
// by RecordAt/Records. It re-validates every varint boundary, so a tampered
// page whose CRC was recomputed is still rejected here rather than panicking.
func (p *RowsPage) buildIndex() error {
	count := p.h.EntryCount
	p.ids = make([]uint64, count)
	p.ends = make([]uint32, count)
	p.vers = make([]uint32, count)
	var (
		rowID   uint64
		idsPos  int
		offPos  int
		rlePos  int
		runEnd  uint32
		runVer  uint32
		lastEnd uint32
	)
	for i := uint32(0); i < count; i++ {
		v, n := binary.Uvarint(p.rowIDs[idsPos:])
		if n <= 0 {
			return errPageTruncated
		}
		idsPos += n
		if i == 0 {
			rowID = v
		} else {
			rowID = uint64(int64(rowID) + unzigzag64(v))
		}
		p.ids[i] = rowID

		d, n2 := binary.Uvarint(p.offsets[offPos:])
		if n2 <= 0 {
			return errPageTruncated
		}
		offPos += n2
		if d > uint64(^uint32(0))-uint64(lastEnd) {
			return fmt.Errorf("rowpack: page record %d end offset overflows uint32", i)
		}
		lastEnd += uint32(d)
		if uint64(lastEnd) > uint64(len(p.tuples)) {
			return fmt.Errorf("rowpack: page record %d end offset %d exceeds tuples %d", i, lastEnd, len(p.tuples))
		}
		p.ends[i] = lastEnd

		for runEnd <= i {
			v1, n3 := binary.Uvarint(p.schemaRLE[rlePos:])
			if n3 <= 0 {
				return errPageTruncated
			}
			rlePos += n3
			r, n4 := binary.Uvarint(p.schemaRLE[rlePos:])
			if n4 <= 0 {
				return errPageTruncated
			}
			rlePos += n4
			if v1 > uint64(^uint32(0)) {
				return fmt.Errorf("rowpack: page schema version %d exceeds uint32", v1)
			}
			if r == 0 || r > uint64(count-runEnd) {
				return fmt.Errorf("rowpack: page schema run %d exceeds remaining records", r)
			}
			runVer = uint32(v1)
			runEnd += uint32(r)
		}
		p.vers[i] = runVer
	}
	if idsPos != len(p.rowIDs) || offPos != len(p.offsets) || rlePos != len(p.schemaRLE) {
		return fmt.Errorf("rowpack: page metadata streams contain trailing bytes")
	}
	if runEnd != count {
		return fmt.Errorf("rowpack: page schema runs cover %d records, want %d", runEnd, count)
	}
	if lastEnd != uint32(len(p.tuples)) {
		return fmt.Errorf("rowpack: page tuple ends at %d, tuples contain %d bytes", lastEnd, len(p.tuples))
	}
	if p.ids[0] != p.h.FirstRowID {
		return fmt.Errorf("rowpack: page first row id %d, want %d", p.h.FirstRowID, p.ids[0])
	}
	minID, maxID := p.ids[0], p.ids[0]
	for _, id := range p.ids[1:] {
		if id < minID {
			minID = id
		}
		if id > maxID {
			maxID = id
		}
	}
	if minID != p.h.MinRowID || maxID != p.h.MaxRowID {
		return fmt.Errorf("rowpack: page row id range [%d,%d], header [%d,%d]", minID, maxID, p.h.MinRowID, p.h.MaxRowID)
	}
	return nil
}

// validateChangeBits rejects the reserved packed value 3 anywhere in the
// stream — the format-level corruption marker.
func validateChangeBits(bits []byte, count uint32) error {
	for i := uint32(0); i < count; i++ {
		if (bits[i/4]>>((i%4)*2))&3 == 3 {
			return fmt.Errorf("rowpack: rows page record %d: illegal change type marker", i)
		}
	}
	return nil
}

// Header returns the parsed page header.
func (p *RowsPage) Header() fileformat.RowsPageHeader { return p.h }

// Raw returns the full page bytes.
func (p *RowsPage) Raw() []byte { return p.raw }

// RowIDAt decodes the RowID of one record (stream walk, O(ordinal)).
func (p *RowsPage) RowIDAt(ordinal uint32) (uint64, error) {
	var rowID uint64
	pos := 0
	for i := uint32(0); i <= ordinal; i++ {
		v, n := binary.Uvarint(p.rowIDs[pos:])
		if n <= 0 {
			return 0, errPageTruncated
		}
		pos += n
		if i == 0 {
			rowID = v
		} else {
			rowID = uint64(int64(rowID) + unzigzag64(v))
		}
	}
	return rowID, nil
}

// RecordAt decodes one record: RowID, schema version, change type and the
// body view (empty for deletes). O(1) via the parse-time record index; the
// body aliases the page bytes.
func (p *RowsPage) RecordAt(ordinal uint32) (rec codec.PageRecord, err error) {
	if ordinal >= p.h.EntryCount {
		return rec, fmt.Errorf("rowpack: record ordinal %d out of range (%d)", ordinal, p.h.EntryCount)
	}
	startEnd := uint32(0)
	if ordinal > 0 {
		startEnd = p.ends[ordinal-1]
	}
	end := p.ends[ordinal]
	packed := (p.changeBits[ordinal/4] >> ((ordinal % 4) * 2)) & 3
	ct, err := fileformat.UnpackChangeType(packed)
	if err != nil {
		return rec, err
	}
	body := p.tuples[startEnd:end]
	if ct == fileformat.ChangeDelete && len(body) != 0 {
		return rec, fmt.Errorf("rowpack: delete record %d carries a body", ordinal)
	}
	return codec.PageRecord{
		RowID:         p.ids[ordinal],
		SchemaVersion: p.vers[ordinal],
		ChangeType:    ct,
		Body:          body,
	}, nil
}

// Records iterates all records in call order, decoding streams once. Bodies
// alias the page bytes; fn must not retain them beyond the call.
func (p *RowsPage) Records(fn func(rec codec.PageRecord) error) error {
	var (
		rowID   uint64
		idsPos  int
		offPos  int
		lastEnd uint32
		rlePos  int
		runEnd  uint32
		runVer  uint32
	)
	for i := uint32(0); i < p.h.EntryCount; i++ {
		v, n := binary.Uvarint(p.rowIDs[idsPos:])
		if n <= 0 {
			return errPageTruncated
		}
		idsPos += n
		if i == 0 {
			rowID = v
		} else {
			rowID = uint64(int64(rowID) + unzigzag64(v))
		}

		d, n2 := binary.Uvarint(p.offsets[offPos:])
		if n2 <= 0 {
			return errPageTruncated
		}
		offPos += n2
		startEnd := lastEnd
		lastEnd += uint32(d)

		for runEnd <= i {
			v1, n3 := binary.Uvarint(p.schemaRLE[rlePos:])
			if n3 <= 0 {
				return errPageTruncated
			}
			rlePos += n3
			r, n4 := binary.Uvarint(p.schemaRLE[rlePos:])
			if n4 <= 0 {
				return errPageTruncated
			}
			rlePos += n4
			runVer = uint32(v1)
			runEnd += uint32(r)
		}

		packed := (p.changeBits[i/4] >> ((i % 4) * 2)) & 3
		ct, err := fileformat.UnpackChangeType(packed)
		if err != nil {
			return err
		}
		body := p.tuples[startEnd:lastEnd]
		if ct == fileformat.ChangeDelete && len(body) != 0 {
			return fmt.Errorf("rowpack: delete record %d carries a body", i)
		}
		if err := fn(codec.PageRecord{
			RowID:         rowID,
			SchemaVersion: runVer,
			ChangeType:    ct,
			Body:          body,
		}); err != nil {
			return err
		}
	}
	return nil
}

func unzigzag64(v uint64) int64 { return int64(v>>1) ^ -int64(v&1) }
