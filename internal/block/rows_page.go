package block

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"sync"
	"unsafe"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
)

// Rows Page in-memory builder/reader.
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

	// rawPage is the Finish serialization scratch, reused across pages so a
	// write path does not allocate one fresh ~32 KiB buffer per page. The
	// slice handed out by Finish is valid until the next Finish.
	rawPage []byte
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
func (b *PageBuilder) Add(rowID uint64, schemaVersion uint32, changeType format.ChangeType, tuple []byte) error {
	packed, err := format.PackChangeType(changeType)
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
func (b *PageBuilder) rawBytes() int {
	return format.RowsPageHeaderSize + len(b.rowIDs) + len(b.offsets) +
		len(b.schemaRLE) + len(b.changeBits) + len(b.tuples)
}

// Count returns the number of buffered records.
func (b *PageBuilder) countRows() uint32 { return b.count }

// NeedsFlush reports whether the pending page has reached the target size.
func (b *PageBuilder) NeedsFlush() bool {
	return b.count > 0 && b.rawBytes() >= b.target
}

// Finish serializes the buffered records into one page and resets the
// builder. Returns nil when nothing is buffered.
//
// The page is serialized into a scratch the builder reuses across Finish
// calls: callers consume it before the next Finish (RowsBuilder compresses
// it into an owned stored buffer immediately). Write paths whose compression
// aliases the input (CompressionNone) must copy before retaining.
func (b *PageBuilder) Finish() ([]byte, error) {
	if b.count == 0 {
		return nil, nil
	}
	b.flushRun()

	h := format.RowsPageHeader{
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
	total := format.RowsPageHeaderSize + h.StreamsBytes()
	if cap(b.rawPage) < int(total) {
		b.rawPage = make([]byte, total)
	}
	page := b.rawPage[:total]
	n := format.RowsPageHeaderSize
	n += copy(page[n:], b.rowIDs)
	n += copy(page[n:], b.offsets)
	n += copy(page[n:], b.schemaRLE)
	n += copy(page[n:], b.changeBits)
	n += copy(page[n:], b.tuples)
	streams := page[format.RowsPageHeaderSize:]
	h.CRC32C = format.CRC32C(streams)
	if err := h.MarshalTo(page); err != nil {
		return nil, err
	}
	b.reset()
	return page, nil
}

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
// not retain them beyond the page's lifetime.
//
// Memory model: ParseRowsPage validates the streams with a zero-allocation
// walk (a tampered page whose CRC was recomputed is still rejected at parse
// time), but materializes NO per-record state. Sequential passes (Records)
// never build anything. The random-access index is built lazily on the first
// RecordAt — guarded by indexOnce so concurrent readers of a shared memoized
// page race on a Once instead of the map — and stored as a sparse anchor
// table: one 32-byte anchor per indexWindow records plus the schema-RLE
// stream itself, ~2 B/record instead of the previous 16 B/record of full
// ids/ends/vers arrays. RecordAt re-walks at most indexWindow-1 uvarints
// from the anchor, so the page is immutable afterwards and memoized pages
// can still be shared between readers without a lock.
type RowsPage struct {
	h     format.RowsPageHeader
	raw   []byte
	start int // streams region start within raw (= fileformat.RowsPageHeaderSize)

	rowIDs     []byte
	offsets    []byte
	schemaRLE  []byte
	changeBits []byte
	tuples     []byte

	// lazy random-access index: anchors[i] resumes a stream walk at record
	// i*indexWindow (see pageAnchor). Built on first RecordAt; nil when the
	// page has only been walked sequentially.
	indexOnce sync.Once
	indexErr  error
	anchors   []pageAnchor
	// onIndexBuilt, when set (memoized pages only), is called with the
	// anchor table's byte size so the owning container can re-charge the
	// LRU entry — the lazy index grows a page's retained footprint after
	// memoization, and that growth must not bypass CacheBytes.
	onIndexBuilt func(delta int64)
}

// indexWindow is the anchor granularity: RecordAt walks at most
// indexWindow-1 uvarints from the nearest preceding anchor. 8 halves the
// point-read walk against a 16 window while the table still costs only
// 4 B/record (the pre-sparse layout was 16 B/record).
const indexWindow = 8

// pageAnchor resumes the three per-record streams at one record: the RowID
// and end-offset running values plus the byte positions to continue decoding
// from and the schema-RLE run state. 32 bytes per indexWindow records.
type pageAnchor struct {
	rowID  uint64 // absolute RowID at the window start
	end    uint32 // cumulative tuple end at the window start
	idsOff uint32 // byte offset into the rowIDs stream
	offOff uint32 // byte offset into the offsets stream
	rleOff uint32 // byte offset into the schemaRLE stream
	runEnd uint32 // schema-RLE run covering the window start (exclusive)
	runVer uint32 // that run's schema version
}

// ParseRowsPage validates the header geometry and the page CRC, then hands
// back stream views. Corruption is always an error, never a panic.
func ParseRowsPage(raw []byte) (*RowsPage, error) {
	var h format.RowsPageHeader
	if err := h.Unmarshal(raw, len(raw)); err != nil {
		return nil, err
	}
	start := format.RowsPageHeaderSize
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
	if format.CRC32C(raw[start:]) != h.CRC32C {
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
	if err := p.validateStreams(); err != nil {
		return nil, err
	}
	return p, nil
}

// validateStreams walks every per-record stream once WITHOUT materializing
// any per-record state, enforcing the same guarantees the record index used
// to check at parse time: every varint boundary resolves, end offsets stay
// within uint32 and the tuples region, the schema-RLE runs cover the records
// exactly, no stream carries trailing bytes, and the header's FirstRowID /
// Min/Max agree with the streams. A tampered page whose CRC was recomputed
// is still rejected here rather than panicking later. Sequential readers
// (Records) pay only this walk; the random-access anchor table is built
// lazily on the first RecordAt.
func (p *RowsPage) validateStreams() error {
	count := p.h.EntryCount
	var (
		rowID   uint64
		firstID uint64
		idsPos  int
		offPos  int
		rlePos  int
		runEnd  uint32
		lastEnd uint32
		minID   uint64
		maxID   uint64
	)
	for i := range count {
		v, n := binary.Uvarint(p.rowIDs[idsPos:])
		if n <= 0 {
			return errPageTruncated
		}
		idsPos += n
		if i == 0 {
			rowID, firstID = v, v
		} else {
			rowID = uint64(int64(rowID) + unzigzag64(v))
		}
		if i == 0 {
			minID, maxID = rowID, rowID
		} else {
			if rowID < minID {
				minID = rowID
			}
			if rowID > maxID {
				maxID = rowID
			}
		}

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
			runEnd += uint32(r)
		}
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
	if count > 0 {
		if firstID != p.h.FirstRowID {
			return fmt.Errorf("rowpack: page first row id %d, want %d", p.h.FirstRowID, firstID)
		}
		if minID != p.h.MinRowID || maxID != p.h.MaxRowID {
			return fmt.Errorf("rowpack: page row id range [%d,%d], header [%d,%d]", minID, maxID, p.h.MinRowID, p.h.MaxRowID)
		}
	}
	return nil
}

// ensureIndex builds the sparse anchor table on first use. Safe for
// concurrent callers on a shared memoized page: the Once serializes the one
// build, and afterwards the table is read-only.
func (p *RowsPage) ensureIndex() error {
	p.indexOnce.Do(func() {
		if p.h.EntryCount == 0 {
			return
		}
		p.indexErr = p.buildAnchors()
		if p.indexErr == nil && p.onIndexBuilt != nil {
			p.onIndexBuilt(int64(len(p.anchors)) * int64(unsafe.Sizeof(pageAnchor{})))
		}
	})
	return p.indexErr
}

// buildAnchors walks the streams once, snapshotting the resume state at every
// indexWindow-th record. The streams were fully validated at parse time, so
// the walk cannot fail here; the checks are kept as defensive errors anyway.
func (p *RowsPage) buildAnchors() error {
	count := int(p.h.EntryCount)
	p.anchors = make([]pageAnchor, 0, (count+indexWindow-1)/indexWindow)
	var (
		rowID   uint64
		idsPos  int
		offPos  int
		rlePos  int
		runEnd  uint32
		runVer  uint32
		lastEnd uint32
	)
	for i := 0; i < count; i++ {
		if i%indexWindow == 0 {
			p.anchors = append(p.anchors, pageAnchor{
				rowID:  rowID,
				end:    lastEnd,
				idsOff: uint32(idsPos),
				offOff: uint32(offPos),
				rleOff: uint32(rlePos),
				runEnd: runEnd,
				runVer: runVer,
			})
		}
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
		lastEnd += uint32(d)

		for runEnd <= uint32(i) {
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
	}
	return nil
}

// validateChangeBits rejects the reserved packed value 3 anywhere in the
// stream — the format-level corruption marker.
func validateChangeBits(stream []byte, count uint32) error {
	// Four 2-bit entries share each byte. For a lane to equal binary 11, its
	// low bit and the adjacent high bit must both be set. Folding the high bits
	// down and masking with 0x55 tests all four lanes in parallel. Besides
	// reducing branches this shape is friendly to wider compiler/assembly
	// implementations later, while keeping the portable scalar path cheap.
	full := count / 4
	for byteIdx := range full {
		x := stream[byteIdx]
		bad := x & (x >> 1) & 0x55
		if bad != 0 {
			lane := uint32(bits.TrailingZeros8(bad) / 2)
			return fmt.Errorf("rowpack: rows page record %d: illegal change type marker", byteIdx*4+lane)
		}
	}
	// Ignore the unused high lanes in the final byte, matching the v1 format.
	for i := full * 4; i < count; i++ {
		if (stream[i/4]>>((i%4)*2))&3 == 3 {
			return fmt.Errorf("rowpack: rows page record %d: illegal change type marker", i)
		}
	}
	return nil
}

// Header returns the parsed page header.
func (p *RowsPage) Header() format.RowsPageHeader { return p.h }

// RecordAt decodes one record: RowID, schema version, change type and the
// body view (empty for deletes). The record is located by re-walking at most
// indexWindow-1 uvarints from the nearest preceding sparse anchor (built
// lazily on first use); the body aliases the page bytes.
func (p *RowsPage) RecordAt(ordinal uint32) (rec codec.PageRecord, err error) {
	if ordinal >= p.h.EntryCount {
		return rec, fmt.Errorf("rowpack: record ordinal %d out of range (%d)", ordinal, p.h.EntryCount)
	}
	if err := p.ensureIndex(); err != nil {
		return rec, err
	}
	w := ordinal / indexWindow
	if int(w) >= len(p.anchors) {
		return rec, errPageTruncated
	}
	a := &p.anchors[w]
	winStart := w * indexWindow
	// Re-walk the RowID and end-offset streams from the anchor up to (and
	// including) the target record. The streams were fully validated at parse
	// time, so the truncation guards are defensive only.
	rowID := a.rowID
	lastEnd := a.end
	startEnd := a.end
	idsPos := int(a.idsOff)
	offPos := int(a.offOff)
	for i := winStart; i <= ordinal; i++ {
		v, n := binary.Uvarint(p.rowIDs[idsPos:])
		if n <= 0 {
			return rec, errPageTruncated
		}
		idsPos += n
		d, n2 := binary.Uvarint(p.offsets[offPos:])
		if n2 <= 0 {
			return rec, errPageTruncated
		}
		offPos += n2
		if i > winStart {
			startEnd = lastEnd
		}
		lastEnd += uint32(d)
		if i == 0 {
			rowID = v
		} else {
			rowID = uint64(int64(rowID) + unzigzag64(v))
		}
	}
	// Schema version: resume the RLE from the anchor's run state.
	rlePos := int(a.rleOff)
	runEnd, runVer := a.runEnd, a.runVer
	for runEnd <= ordinal {
		v1, n3 := binary.Uvarint(p.schemaRLE[rlePos:])
		if n3 <= 0 {
			return rec, errPageTruncated
		}
		rlePos += n3
		r, n4 := binary.Uvarint(p.schemaRLE[rlePos:])
		if n4 <= 0 {
			return rec, errPageTruncated
		}
		rlePos += n4
		runVer = uint32(v1)
		runEnd += uint32(r)
	}
	packed := (p.changeBits[ordinal/4] >> ((ordinal % 4) * 2)) & 3
	ct, err := format.UnpackChangeType(packed)
	if err != nil {
		return rec, err
	}
	body := p.tuples[startEnd:lastEnd]
	if ct == format.ChangeDelete && len(body) != 0 {
		return rec, fmt.Errorf("rowpack: delete record %d carries a body", ordinal)
	}
	return codec.PageRecord{
		RowID:         rowID,
		SchemaVersion: runVer,
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
		ct, err := format.UnpackChangeType(packed)
		if err != nil {
			return err
		}
		body := p.tuples[startEnd:lastEnd]
		if ct == format.ChangeDelete && len(body) != 0 {
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

// PageCursor is a forward-only sequential reader over one RowsPage. It holds
// the stream positions so each step decodes exactly one record — no per-record
// anchor re-walk and no index build, which is what sequential walkers
// (scans, ordered batch reads) should use instead of RecordAt. A cursor is
// not safe for concurrent use; each walker owns one.
type PageCursor struct {
	p       *RowsPage
	next    uint32 // ordinal the next step() decodes
	rowID   uint64 // running RowID (value of record next-1)
	lastEnd uint32 // cumulative tuple end at next-1
	idsPos  int
	offPos  int
	rlePos  int
	runEnd  uint32
	runVer  uint32
}

// Cursor starts a sequential pass over the page's records. The zero cursor
// is invalid; use Reset to (re)target it, so hot walkers can reuse one
// cursor across pages without re-allocating.
func (p *RowsPage) Cursor() *PageCursor {
	return new(PageCursor).Reset(p)
}

// Reset retargets the cursor to the start of page p and returns it.
func (c *PageCursor) Reset(p *RowsPage) *PageCursor {
	*c = PageCursor{p: p}
	return c
}

// Next decodes the next record in call order. ok=false reports exhaustion.
func (c *PageCursor) Next() (rec codec.PageRecord, ok bool, err error) {
	if c.next >= c.p.h.EntryCount {
		return codec.PageRecord{}, false, nil
	}
	rec, err = c.step()
	if err != nil {
		return codec.PageRecord{}, false, err
	}
	c.next++
	return rec, true, nil
}

// At returns record ordinal. A target at or ahead of the cursor advances it
// (skipping intermediates at O(1) each); a target behind the cursor falls
// back to the page's random-access path. Callers visiting a page in
// nondecreasing ordinal order pay sequential-decode cost only.
func (c *PageCursor) At(ordinal uint32) (codec.PageRecord, error) {
	if ordinal < c.next {
		return c.p.RecordAt(ordinal)
	}
	for c.next < ordinal {
		if _, err := c.step(); err != nil {
			return codec.PageRecord{}, err
		}
		c.next++
	}
	rec, err := c.step()
	if err != nil {
		return codec.PageRecord{}, err
	}
	c.next++
	return rec, nil
}

// step decodes record c.next and advances every stream position. The streams
// were validated at parse time, so the truncation guards are defensive.
func (c *PageCursor) step() (codec.PageRecord, error) {
	i := c.next
	p := c.p
	v, n := binary.Uvarint(p.rowIDs[c.idsPos:])
	if n <= 0 {
		return codec.PageRecord{}, errPageTruncated
	}
	c.idsPos += n
	d, n2 := binary.Uvarint(p.offsets[c.offPos:])
	if n2 <= 0 {
		return codec.PageRecord{}, errPageTruncated
	}
	c.offPos += n2
	startEnd := c.lastEnd
	c.lastEnd += uint32(d)
	if i == 0 {
		c.rowID = v
	} else {
		c.rowID = uint64(int64(c.rowID) + unzigzag64(v))
	}
	for c.runEnd <= i {
		v1, n3 := binary.Uvarint(p.schemaRLE[c.rlePos:])
		if n3 <= 0 {
			return codec.PageRecord{}, errPageTruncated
		}
		c.rlePos += n3
		r, n4 := binary.Uvarint(p.schemaRLE[c.rlePos:])
		if n4 <= 0 {
			return codec.PageRecord{}, errPageTruncated
		}
		c.rlePos += n4
		c.runVer = uint32(v1)
		c.runEnd += uint32(r)
	}
	packed := (p.changeBits[i/4] >> ((i % 4) * 2)) & 3
	ct, err := format.UnpackChangeType(packed)
	if err != nil {
		return codec.PageRecord{}, err
	}
	body := p.tuples[startEnd:c.lastEnd]
	if ct == format.ChangeDelete && len(body) != 0 {
		return codec.PageRecord{}, fmt.Errorf("rowpack: delete record %d carries a body", i)
	}
	return codec.PageRecord{
		RowID:         c.rowID,
		SchemaVersion: c.runVer,
		ChangeType:    ct,
		Body:          body,
	}, nil
}
