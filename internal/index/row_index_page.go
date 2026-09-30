package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
)

// maxUint32 is used for width checks (a TableID/ItemOrdinal exceeding uint32
// is rejected).
const maxUint32 = uint64(0xFFFFFFFF)

// indexPageEntryCount 是每页最大行条目数（4096）。
const indexPageEntryCount = 4096

// rowIndexPageChunkKind is the chunk-sealing kind used for index pages so an
// encrypted store seals pages under the Index-domain nonce/AAD space.
// It reuses the row kind because pages are the row index; pages seal under
// chunk sequences beyond every chunk, so no nonce collision occurs.
const rowIndexPageChunkKind = format.IndexChunkKindRow

var errIndexPageCorrupt = errors.New("rowpack: index page corrupt")

// pageBuild is one encoded (and optionally compressed/sealed) index
// page plus its fence directory entry, produced by the builder for the txn
// body. raw is the uncompressed page (for the plaintext-body CRC); stored is
// the compressed (and, when encrypted, sealed) page bytes written to disk.
type pageBuild struct {
	raw    []byte
	stored []byte
	fence  format.RowIndexFenceEntry
}

// buildPages sorts the builder's rows by (TableID, RowID), partitions
// them into indexPageEntryCount-entry pages, encodes/compresses (and, when
// crypto != nil, seals) each page, and returns the pages plus their fence
// entries. Fence.StoredOffset is zero here and patched by the caller once the
// body layout (chunk region + directory) is known. pageSeqBase is the first
// free chunk sequence in the surrounding txn so pages seal under distinct
// nonces.
func (b *Builder) buildPages(crypto *ChunkCrypto, level int, pageSeqBase uint32) ([]pageBuild, error) {
	n := len(b.rows)
	if n == 0 {
		b.pageCount = 0
		return nil, nil
	}
	sortRowIndexEntries(b.rows)
	out := make([]pageBuild, 0, (n+indexPageEntryCount-1)/indexPageEntryCount)
	// Partition into SINGLE-TABLE pages: a page never splits a table run. This
	// makes each RowIndexFenceEntry a per-table key (TableID + Min/Max RowID fall
	// within one table), so the fence is a correct monotonic binary-search index
	// over (TableID, RowID) for Lazy reads. A table longer than
	// indexPageEntryCount still splits every page boundary.
	for i := 0; i < n; {
		j := i
		for j < n && b.rows[j].TableID == b.rows[i].TableID {
			j++
		}
		for s := i; s < j; s += indexPageEntryCount {
			e := s + indexPageEntryCount
			if e > j {
				e = j
			}
			page, _, _, _, err := encodePage(b.rows[s:e], indexPageEntryCount)
			if err != nil {
				return nil, err
			}
			stored, err := block.Compress(format.CompressionZstd, level, page)
			if err != nil {
				return nil, err
			}
			storedSize := uint32(len(stored))
			if crypto != nil {
				sealed, err := crypto.Seal(pageSeqBase+uint32(len(out)), rowIndexPageChunkKind, uint32(len(out)), len(page), stored)
				if err != nil {
					return nil, err
				}
				stored = sealed
				storedSize = uint32(len(sealed))
			}
			fence, err := pageFence(page, storedSize, b.snapshot.SnapshotID, 0)
			if err != nil {
				return nil, err
			}
			out = append(out, pageBuild{raw: page, stored: stored, fence: fence})
		}
		i = j
	}
	b.pageCount = uint32(len(out))
	return out, nil
}

// appendChange appends the 2-bit changeType for entry ordinal into a packed
// byte stream, zero-extending as needed.
func appendChange(dst []byte, ordinal uint32, packed uint8) []byte {
	byteIdx := ordinal / 4
	for uint32(len(dst)) <= byteIdx {
		dst = append(dst, 0)
	}
	shift := (ordinal % 4) * 2
	dst[byteIdx] |= packed << shift
	return dst
}

func sortRowIndexEntries(entries []format.RowIndexEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.TableID != b.TableID {
			return a.TableID < b.TableID
		}
		return a.RowID < b.RowID
	})
}

func encodePage(entries []format.RowIndexEntry, pageSize int) (page []byte, entryCount int, minRowID, maxRowID uint64, err error) {
	if pageSize <= 0 {
		return nil, 0, 0, 0, errors.New("rowpack: page size must be positive")
	}
	if len(entries) == 0 {
		return nil, 0, 0, 0, errors.New("rowpack: cannot encode an empty index page")
	}
	if len(entries) > pageSize {
		return nil, 0, 0, 0, fmt.Errorf("rowpack: page has %d entries, exceeds page size %d", len(entries), pageSize)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].TableID < entries[i-1].TableID {
			return nil, 0, 0, 0, fmt.Errorf("rowpack: table ids not ascending at %d", i)
		}
		if entries[i].TableID == entries[i-1].TableID && entries[i].RowID <= entries[i-1].RowID {
			return nil, 0, 0, 0, fmt.Errorf("rowpack: row ids not strictly ascending within table at %d", i)
		}
	}

	tableRunBytes := make([]byte, 0)
	type run struct{ start, length int }
	tableRuns := make([]run, 0, 8)
	for tpos := 0; tpos < len(entries); {
		tid := entries[tpos].TableID
		j := tpos
		for j < len(entries) && entries[j].TableID == tid {
			j++
		}
		tableRunBytes = binary.AppendUvarint(tableRunBytes, uint64(tid))
		tableRunBytes = binary.AppendUvarint(tableRunBytes, uint64(j-tpos))
		tableRuns = append(tableRuns, run{start: tpos, length: j - tpos})
		tpos = j
	}

	rowIDBytes := make([]byte, 0)
	for _, tr := range tableRuns {
		b, e := tr.start, tr.start+tr.length
		rowIDBytes = binary.AppendUvarint(rowIDBytes, entries[b].RowID)
		for k := b + 1; k < e; k++ {
			rowIDBytes = binary.AppendUvarint(rowIDBytes, entries[k].RowID-entries[k-1].RowID)
		}
	}

	blockRunBytes := make([]byte, 0)
	for bpos := 0; bpos < len(entries); {
		bid := entries[bpos].BlockID
		j := bpos
		for j < len(entries) && entries[j].BlockID == bid {
			j++
		}
		blockRunBytes = binary.AppendUvarint(blockRunBytes, bid)
		blockRunBytes = binary.AppendUvarint(blockRunBytes, uint64(j-bpos))
		bpos = j
	}

	ordinalBytes := make([]byte, 0)
	ordinalBytes = binary.AppendUvarint(ordinalBytes, uint64(entries[0].ItemOrdinal))
	for i := 1; i < len(entries); i++ {
		d := int64(entries[i].ItemOrdinal) - int64(entries[i-1].ItemOrdinal)
		ordinalBytes = binary.AppendUvarint(ordinalBytes, zigzag(d))
	}

	// ChangeType 2bit stream。
	changeBits := make([]byte, 0)
	for i := range entries {
		packed, err := format.PackChangeType(entries[i].ChangeType)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		changeBits = appendChange(changeBits, uint32(i), packed)
	}

	minRowID, maxRowID = entries[0].RowID, entries[0].RowID
	for _, r := range entries {
		if r.RowID < minRowID {
			minRowID = r.RowID
		}
		if r.RowID > maxRowID {
			maxRowID = r.RowID
		}
	}

	h := format.RowIndexPageHeader{
		EntryCount:      uint32(len(entries)),
		TableRunBytes:   uint32(len(tableRunBytes)),
		RowIDBytes:      uint32(len(rowIDBytes)),
		BlockRunBytes:   uint32(len(blockRunBytes)),
		OrdinalBytes:    uint32(len(ordinalBytes)),
		ChangeBitsBytes: uint32(len(changeBits)),
		FirstRowID:      entries[0].RowID,
		MinRowID:        minRowID,
		MaxRowID:        maxRowID,
	}

	page = make([]byte, 0, format.IndexPageHeaderSize+len(tableRunBytes)+len(rowIDBytes)+len(blockRunBytes)+len(ordinalBytes)+len(changeBits))
	page = append(page, make([]byte, format.IndexPageHeaderSize)...)
	page = append(page, tableRunBytes...)
	page = append(page, rowIDBytes...)
	page = append(page, blockRunBytes...)
	page = append(page, ordinalBytes...)
	page = append(page, changeBits...)
	streams := page[format.IndexPageHeaderSize:]
	h.CRC32C = format.CRC32C(streams)
	if err := h.MarshalTo(page); err != nil {
		return nil, 0, 0, 0, err
	}
	return page, len(entries), minRowID, maxRowID, nil
}

type pageStreams struct {
	header                    format.RowIndexPageHeader
	count                     int
	tableRun, rowID, blockRun []byte
	ordinal, changeBits       []byte
}

func splitPage(raw []byte) (pageStreams, error) {
	var out pageStreams
	n := len(raw)
	if n < format.IndexPageHeaderSize {
		return out, errIndexPageCorrupt
	}
	if err := out.header.Unmarshal(raw, n); err != nil {
		return out, err
	}
	out.count = int(out.header.EntryCount)
	start := format.IndexPageHeaderSize
	if format.CRC32C(raw[start:]) != out.header.CRC32C {
		return out, fmt.Errorf("rowpack: index page CRC mismatch")
	}
	// ChangeBitsBytes vs EntryCount and the streams-sum-vs-page-length
	// geometry were already cross-checked by header.Unmarshal above, so the
	// sequential takes below can only run out of bytes if the sums lie —
	// which they can't.
	off := start
	// Stream geometry (each take fits, and the streams sum to exactly the
	// page body) was cross-checked by header.Unmarshal against the full page
	// length, so the takes below cannot overrun.
	take := func(byteLen uint32) []byte {
		s := raw[off : off+int(byteLen)]
		off += int(byteLen)
		return s
	}
	out.tableRun = take(out.header.TableRunBytes)
	out.rowID = take(out.header.RowIDBytes)
	out.blockRun = take(out.header.BlockRunBytes)
	out.ordinal = take(out.header.OrdinalBytes)
	out.changeBits = take(out.header.ChangeBitsBytes)
	if uint64(out.count) > uint64(len(out.rowID)) {
		return out, fmt.Errorf("rowpack: index page entry count %d exceeds row id stream %d", out.count, len(out.rowID))
	}
	return out, nil
}

// pageRows is one decoded index page in columnar form — the same layout the
// in-memory rowShard uses, so a sink can append a whole page without a
// per-entry closure or interface call. The streaming parser reuses one
// instance across pages, so decoding a page allocates nothing after warmup.
type pageRows struct {
	rowIDs   []uint64 // sorted by RowID within each table run
	ordinals []uint32 // ItemOrdinal per entry
	changes  []uint8  // ChangeType per entry

	// Table runs: tableIDs[t] owns entries
	// [tableRunStart[t], tableRunStart[t+1]). A page normally holds exactly
	// one table, but the decoder accepts any number of ascending table runs.
	tableRunStart []uint32
	tableIDs      []uint32

	// Block runs: blockIDs[b] owns entries
	// [blockRunStart[b], blockRunStart[b+1]). Runs are global over the page and
	// may cross a table boundary.
	blockRunStart []uint32
	blockIDs      []uint64
}

// tableOf returns the table ID owning entry i, advancing the caller's run
// cursor. Entries must be visited in ascending order.
func (p *pageRows) tableOf(i int, t *int) uint32 {
	for int(p.tableRunStart[*t+1]) <= i {
		*t = *t + 1
	}
	return p.tableIDs[*t]
}

// blockOf returns the BlockID owning entry i, advancing the caller's run
// cursor. Entries must be visited in ascending order.
func (p *pageRows) blockOf(i int, b *int) uint64 {
	for int(p.blockRunStart[*b+1]) <= i {
		*b = *b + 1
	}
	return p.blockIDs[*b]
}

// walkPage decodes one index page and emits every entry in ascending order.
// Callers that consume a whole page should prefer decodePageInto, which skips
// the per-entry callback; walkPage remains for the buffered path and tests.
func walkPage(raw []byte, emit func(format.RowIndexEntry) error) error {
	var p pageRows
	if err := decodePageInto(&p, raw); err != nil {
		return err
	}
	ti, bi := 0, 0
	for i := range p.rowIDs {
		e := format.RowIndexEntry{
			TableID:     p.tableOf(i, &ti),
			RowID:       p.rowIDs[i],
			BlockID:     p.blockOf(i, &bi),
			ItemOrdinal: p.ordinals[i],
			ChangeType:  format.ChangeType(p.changes[i]),
		}
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}

// decodePageInto decodes and fully validates one index page into p, reusing
// p's buffers. It performs every check walkPage performs and reports the same
// entry indices in its errors; the decoded columns and run directories are the
// exact shape a rowShard stores, so a sink can append them wholesale.
func decodePageInto(p *pageRows, raw []byte) error {
	streams, err := splitPage(raw)
	if err != nil {
		return err
	}
	h, count := streams.header, streams.count
	tableRun, rowIDStream := streams.tableRun, streams.rowID
	blockRun, ordinalStream, changeBits := streams.blockRun, streams.ordinal, streams.changeBits

	readVar := func(s []byte, pos *int) (uint64, error) {
		v, num := binary.Uvarint(s[*pos:])
		if num <= 0 {
			return 0, errIndexPageCorrupt
		}
		*pos += num
		return v, nil
	}

	p.rowIDs = slices.Grow(p.rowIDs[:0], count)
	p.ordinals = slices.Grow(p.ordinals[:0], count)
	p.changes = slices.Grow(p.changes[:0], count)
	p.tableRunStart = p.tableRunStart[:0]
	p.tableIDs = p.tableIDs[:0]
	p.blockRunStart = p.blockRunStart[:0]
	p.blockIDs = p.blockIDs[:0]

	var (
		tp, rp, bp, op, ti   int
		currentTable         uint32
		tableRunLeft         int
		rowRunFirst          bool
		prevRowID            uint64
		curBlock             uint64
		curBlockLeft         int
		firstOrdinal         bool
		prevOrdinal          uint32
		lastTable            uint32
		lastRowID            uint64
		haveFirst            bool
		globalMin, globalMax uint64
		firstRowID           uint64
	)
	firstOrdinal = true
	for ti < count {
		if tableRunLeft == 0 {
			tv, err := readVar(tableRun, &tp)
			if err != nil {
				return err
			}
			if tv > maxUint32 {
				return fmt.Errorf("rowpack: index page table id %d exceeds uint32", tv)
			}
			rl, err := readVar(tableRun, &tp)
			if err != nil {
				return err
			}
			if rl == 0 || rl > uint64(count-ti) {
				return fmt.Errorf("rowpack: index page table run len %d, want 1..%d", rl, count-ti)
			}
			currentTable = uint32(tv)
			tableRunLeft = int(rl)
			rowRunFirst = true
			p.tableIDs = append(p.tableIDs, currentTable)
			p.tableRunStart = append(p.tableRunStart, uint32(ti))
		}
		var rowID uint64
		if rowRunFirst {
			abs, err := readVar(rowIDStream, &rp)
			if err != nil {
				return err
			}
			rowID = abs
			rowRunFirst = false
		} else {
			d, err := readVar(rowIDStream, &rp)
			if err != nil {
				return err
			}
			rowID = prevRowID + d
		}
		prevRowID = rowID
		if curBlockLeft == 0 {
			bid, err := readVar(blockRun, &bp)
			if err != nil {
				return err
			}
			rl, err := readVar(blockRun, &bp)
			if err != nil {
				return err
			}
			if rl == 0 || rl > uint64(count-ti) {
				return fmt.Errorf("rowpack: index page block run len %d, want 1..%d", rl, count-ti)
			}
			curBlock = bid
			curBlockLeft = int(rl)
			p.blockIDs = append(p.blockIDs, curBlock)
			p.blockRunStart = append(p.blockRunStart, uint32(ti))
		}
		curBlockLeft--
		var ordinal uint32
		if firstOrdinal {
			ov, err := readVar(ordinalStream, &op)
			if err != nil {
				return err
			}
			if ov > maxUint32 {
				return fmt.Errorf("rowpack: index page record ordinal %d exceeds uint32", ov)
			}
			prevOrdinal = uint32(ov)
			firstOrdinal = false
		} else {
			d, err := readVar(ordinalStream, &op)
			if err != nil {
				return err
			}
			v := int64(prevOrdinal) + unzigzag(d)
			if v < 0 || uint64(v) > maxUint32 {
				return fmt.Errorf("rowpack: index page record ordinal %d out of range", v)
			}
			prevOrdinal = uint32(v)
		}
		ordinal = prevOrdinal
		// ChangeType（2bit）。
		packed := (changeBits[ti/4] >> ((ti % 4) * 2)) & 3
		ct, err := format.UnpackChangeType(packed)
		if err != nil {
			return fmt.Errorf("rowpack: index page entry %d: %w", ti, err)
		}
		if ti > 0 {
			if currentTable < lastTable {
				return fmt.Errorf("rowpack: index page table ids not sorted at %d", ti)
			}
			if currentTable == lastTable && rowID <= lastRowID {
				return fmt.Errorf("rowpack: index page row ids not strictly ascending at %d", ti)
			}
		}
		lastTable = currentTable
		lastRowID = rowID
		if !haveFirst {
			globalMin, globalMax, firstRowID = rowID, rowID, rowID
			haveFirst = true
		} else {
			if rowID < globalMin {
				globalMin = rowID
			}
			if rowID > globalMax {
				globalMax = rowID
			}
		}
		p.rowIDs = append(p.rowIDs, rowID)
		p.ordinals = append(p.ordinals, ordinal)
		p.changes = append(p.changes, uint8(ct))
		tableRunLeft--
		ti++
	}
	// Terminal sentinels: run r owns [start[r], start[r+1]).
	p.tableRunStart = append(p.tableRunStart, uint32(count))
	p.blockRunStart = append(p.blockRunStart, uint32(count))

	if tp != len(tableRun) {
		return fmt.Errorf("rowpack: index page table run has %d trailing bytes", len(tableRun)-tp)
	}
	if rp != len(rowIDStream) {
		return fmt.Errorf("rowpack: index page row id stream has %d trailing bytes", len(rowIDStream)-rp)
	}
	if bp != len(blockRun) {
		return fmt.Errorf("rowpack: index page block run has %d trailing bytes", len(blockRun)-bp)
	}
	if op != len(ordinalStream) {
		return fmt.Errorf("rowpack: index page record ordinal stream has %d trailing bytes", len(ordinalStream)-op)
	}
	if h.FirstRowID != firstRowID {
		return fmt.Errorf("rowpack: index page first row id %d, want %d", h.FirstRowID, firstRowID)
	}
	if h.MinRowID != globalMin {
		return fmt.Errorf("rowpack: index page min row id %d, want %d", h.MinRowID, globalMin)
	}
	if h.MaxRowID != globalMax {
		return fmt.Errorf("rowpack: index page max row id %d, want %d", h.MaxRowID, globalMax)
	}
	return nil
}

func decodePage(raw []byte) ([]format.RowIndexEntry, error) {
	count := 0
	if len(raw) >= format.IndexPageHeaderSize {
		var h format.RowIndexPageHeader
		if err := h.Unmarshal(raw, len(raw)); err == nil {
			count = int(h.EntryCount)
		}
	}
	out := make([]format.RowIndexEntry, 0, count)
	if err := walkPage(raw, func(e format.RowIndexEntry) error {
		out = append(out, e)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func pageFence(raw []byte, storedSize uint32, snapshotID uint64, storedOffset uint64) (format.RowIndexFenceEntry, error) {
	if len(raw) < format.IndexPageHeaderSize {
		return format.RowIndexFenceEntry{}, errIndexPageCorrupt
	}
	var h format.RowIndexPageHeader
	if err := h.Unmarshal(raw, len(raw)); err != nil {
		return format.RowIndexFenceEntry{}, err
	}
	tid, ok := firstTableIDOfPage(raw)
	if !ok {
		return format.RowIndexFenceEntry{}, errIndexPageCorrupt
	}
	return format.RowIndexFenceEntry{
		SnapshotID:   snapshotID,
		TableID:      tid,
		MinRowID:     h.MinRowID,
		MaxRowID:     h.MaxRowID,
		StoredOffset: storedOffset,
		StoredSize:   storedSize,
		RawSize:      uint32(len(raw)),
		EntryCount:   h.EntryCount,
		PageCRC32C:   h.CRC32C,
	}, nil
}

func firstTableIDOfPage(raw []byte) (uint32, bool) {
	if len(raw) < format.IndexPageHeaderSize {
		return 0, false
	}
	t, n := binary.Uvarint(raw[format.IndexPageHeaderSize:])
	if n <= 0 || t > maxUint32 {
		return 0, false
	}
	return uint32(t), true
}
