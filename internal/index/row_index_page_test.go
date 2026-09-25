package index

import (
	"encoding/binary"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
)

// uint32LE reads a little-endian uint32 for corruption tests.
func uint32LE(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

// row_index_page_test.go — Row Index Page + Fence 的生产实现测试。
// 目标：encodePage/decodePage/pageFence 的往返一致性
// 与严格损坏校验（截断、单 bit 翻转、伪造 size/count、非法 changeType、排序破坏、
// Fence 越界/重复页/错误 Snapshot 归属）必须报错且绝不 panic/无界分配。

func rowEq(a, b format.RowIndexEntry) bool {
	return a.TableID == b.TableID && a.RowID == b.RowID && a.BlockID == b.BlockID &&
		a.ItemOrdinal == b.ItemOrdinal && a.ChangeType == b.ChangeType
}

func rowsEq(a, b []format.RowIndexEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !rowEq(a[i], b[i]) {
			return false
		}
	}
	return true
}

func riEntry(tid uint32, rowID uint64, blockID uint64, ord uint32, ct format.ChangeType) format.RowIndexEntry {
	return format.RowIndexEntry{TableID: tid, RowID: rowID, BlockID: blockID, ItemOrdinal: ord, ChangeType: ct}
}

func riSeq(n, blkEvery int) []format.RowIndexEntry {
	rows := make([]format.RowIndexEntry, n)
	for i := 0; i < n; i++ {
		rows[i] = riEntry(1, uint64(i)+1, uint64(i/blkEvery+1), uint32(i%blkEvery), format.ChangeInsert)
	}
	return rows
}

func TestRowIndexPageSequential(t *testing.T) {
	rows := riSeq(300, 100)
	page, n, _, _, _ := encodePage(rows, indexPageEntryCount)
	if n != len(rows) {
		t.Fatalf("entryCount = %d, want %d", n, len(rows))
	}
	got, err := decodePage(page)
	require.NoError(t, err, "decode")
	if !rowsEq(got, rows) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, rows)
	}
}

func TestRowIndexPageMultiTable(t *testing.T) {
	rows := []format.RowIndexEntry{
		riEntry(1, 1, 10, 0, format.ChangeInsert),
		riEntry(1, 2, 10, 1, format.ChangeInsert),
		riEntry(1, 3, 11, 0, format.ChangeUpdate),
		riEntry(1, 4, 11, 1, format.ChangeDelete),
		riEntry(2, 7, 20, 3, format.ChangeInsert),
		riEntry(2, 9, 20, 5, format.ChangeInsert),
		riEntry(2, 20, 21, 0, format.ChangeDelete),
		riEntry(3, 5, 30, 1, format.ChangeInsert),
	}
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	got, err := decodePage(page)
	require.NoError(t, err, "decode")
	if !rowsEq(got, rows) {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, rows)
	}
}

func TestRowIndexPageRowIDBoundaries(t *testing.T) {
	rows := []format.RowIndexEntry{
		riEntry(1, 0, 1, 0, format.ChangeInsert),
		riEntry(1, 1, 1, 1, format.ChangeInsert),
		riEntry(1, math.MaxUint64-1, 2, 0, format.ChangeUpdate),
		riEntry(1, math.MaxUint64, 2, 1, format.ChangeDelete),
	}
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	got, err := decodePage(page)
	require.NoError(t, err, "decode")
	if !rowsEq(got, rows) {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, rows)
	}
}

func TestRowIndexPageOrdinalDelta(t *testing.T) {
	rows := []format.RowIndexEntry{
		riEntry(1, 1, 1, 100, format.ChangeInsert),
		riEntry(1, 2, 1, 101, format.ChangeInsert),
		riEntry(1, 3, 2, 0, format.ChangeInsert), // -101 delta
		riEntry(1, 4, 2, 1, format.ChangeDelete),
		riEntry(1, 5, 2, math.MaxUint32, format.ChangeUpdate),
	}
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	got, err := decodePage(page)
	require.NoError(t, err, "decode")
	if !rowsEq(got, rows) {
		t.Fatalf("round-trip mismatch: got=%+v want=%+v", got, rows)
	}
}

func TestRowIndexPageRandomPreSorted(t *testing.T) {
	seed := int64(0x52504B32)
	rng := rand.New(rand.NewSource(seed))
	n := 500
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i) + 1
	}
	rng.Shuffle(n, func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	rows := make([]format.RowIndexEntry, n)
	for i, id := range ids {
		rows[i] = riEntry(1, id, id/50, uint32(id%50), format.ChangeInsert)
	}
	sortRowIndexEntries(rows)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	got, err := decodePage(page)
	require.NoError(t, err, "decode")
	if !rowsEq(got, rows) {
		t.Fatalf("round-trip mismatch")
	}
}

// ---- encode 错误分支 ----

func TestRowIndexPageEncodeErrors(t *testing.T) {
	rows := []format.RowIndexEntry{riEntry(1, 1, 1, 0, format.ChangeInsert), riEntry(1, 2, 1, 1, format.ChangeInsert)}
	if _, _, _, _, err := encodePage(nil, indexPageEntryCount); err == nil {
		t.Fatal("encode(nil) = nil error")
	}
	if _, _, _, _, err := encodePage(rows, 0); err == nil {
		t.Fatal("encode(pageSize=0) = nil error")
	}
	if _, _, _, _, err := encodePage(rows, 1); err == nil {
		t.Fatal("encode(rows > pageSize) = nil error")
	}
	bad := []format.RowIndexEntry{riEntry(1, 2, 1, 0, format.ChangeInsert), riEntry(1, 1, 1, 1, format.ChangeInsert)}
	if _, _, _, _, err := encodePage(bad, indexPageEntryCount); err == nil {
		t.Fatal("encode(unsorted) = nil error")
	}
	dup := []format.RowIndexEntry{riEntry(1, 1, 1, 0, format.ChangeInsert), riEntry(1, 1, 1, 1, format.ChangeInsert)}
	if _, _, _, _, err := encodePage(dup, indexPageEntryCount); err == nil {
		t.Fatal("encode(duplicate row id) = nil error")
	}
	if _, _, _, _, err := encodePage([]format.RowIndexEntry{riEntry(1, 1, 1, 0, 0)}, indexPageEntryCount); err == nil {
		t.Fatal("encode(changeType=0) = nil error")
	}
}

// ---- decode 损坏矩阵 ----

func cloneRI(t *testing.T, page []byte) []byte {
	t.Helper()
	c := make([]byte, len(page))
	copy(c, page)
	return c
}

func TestRowIndexPageDecodeTruncated(t *testing.T) {
	rows := riSeq(200, 50)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	for _, cut := range []int{format.IndexPageHeaderSize - 1, format.IndexPageHeaderSize + 1, len(page) / 2, len(page) - 1} {
		if cut >= len(page) {
			continue
		}
		if _, err := decodePage(page[:cut]); err == nil {
			t.Fatalf("decode(truncated %d/%d) = nil error", cut, len(page))
		}
	}
}

func TestRowIndexPageDecodeBitFlip(t *testing.T) {
	rows := riSeq(200, 50)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	for _, off := range []int{1, 12, 16, 36, 60, format.IndexPageHeaderSize, format.IndexPageHeaderSize + 8, len(page) - 1} {
		if off < 0 || off >= len(page) {
			continue
		}
		m := cloneRI(t, page)
		m[off] ^= 0x01
		if _, err := decodePage(m); err == nil {
			t.Fatalf("decode(bit flip @%d) = nil error", off)
		}
	}
}

func TestRowIndexPageDecodeForgedSize(t *testing.T) {
	rows := riSeq(100, 50)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	m := cloneRI(t, page)
	m[20] = m[20] + 1
	if _, err := decodePage(m); err == nil {
		t.Fatal("decode(forged table run size) = nil error")
	}
	m2 := cloneRI(t, page)
	m2[32] = 0
	if _, err := decodePage(m2); err == nil {
		t.Fatal("decode(forged change bits size) = nil error")
	}
}

func TestRowIndexPageDecodeForgedCount(t *testing.T) {
	rows := riSeq(64, 16)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	m := cloneRI(t, page)
	m[12], m[13], m[14], m[15] = 0xFF, 0xFF, 0xFF, 0x7F
	if _, err := decodePage(m); err == nil {
		t.Fatal("decode(forged entry count) = nil error")
	}
}

func TestRowIndexPageDecodeIllegalChangeType(t *testing.T) {
	rows := riSeq(8, 8)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	m := cloneRI(t, page)
	cbOff := changeBitsOffsetRI(page)
	if cbOff < 0 {
		t.Fatalf("cannot locate change bits region")
	}
	m[cbOff] = m[cbOff] | 0x03
	if _, err := decodePage(m); err == nil {
		t.Fatal("decode(illegal change type 3) = nil error")
	}
}

func TestRowIndexPageDecodeCorruptCRC(t *testing.T) {
	rows := riSeq(50, 10)
	page, _, _, _, _ := encodePage(rows, indexPageEntryCount)
	m := cloneRI(t, page)
	m[format.IndexPageHeaderSize] ^= 0x40
	if _, err := decodePage(m); err == nil {
		t.Fatal("decode(stream mutation) = nil error")
	}
}

// ---- Fence ----

func TestFenceForRowIndexPage(t *testing.T) {
	rows := riSeq(100, 25)
	page, n, mn, mx := encodeRowIndexPage1(rows)
	f, err := pageFence(page, 42, 9, 0x1234)
	require.NoError(t, err, "pageFence")
	if f.TableID != 1 || f.SnapshotID != 9 || f.StoredOffset != 0x1234 {
		t.Fatalf("fence identity fields wrong: %+v", f)
	}
	require.Equal(t, uint32(n), f.EntryCount, "fence EntryCount = %d, want %d", f.EntryCount, n)
	if f.MinRowID != mn || f.MaxRowID != mx {
		t.Fatalf("fence min/max = %d/%d, want %d/%d", f.MinRowID, f.MaxRowID, mn, mx)
	}
	if f.RawSize != uint32(len(page)) || f.StoredSize != 42 {
		t.Fatalf("fence sizes = %d/%d", f.RawSize, f.StoredSize)
	}
	if f.PageCRC32C != format.CRC32C(page[format.IndexPageHeaderSize:]) {
		t.Fatalf("fence PageCRC32C mismatch")
	}
	if err := f.MarshalTo(nil); err == nil {
		t.Fatal("fence nil marshal = nil error") // impossible: dst too short
	}
}

// ---- helpers ----

func encodeRowIndexPage1(rows []format.RowIndexEntry) ([]byte, int, uint64, uint64) {
	page, n, mn, mx, err := encodePage(rows, indexPageEntryCount)
	if err != nil {
		panic(err)
	}
	return page, n, mn, mx
}

func changeBitsOffsetRI(page []byte) int {
	if len(page) < format.IndexPageHeaderSize {
		return -1
	}
	tableRun := int(uint32LE(page, 16))
	rowid := int(uint32LE(page, 20))
	blockRun := int(uint32LE(page, 24))
	ordinal := int(uint32LE(page, 28))
	off := format.IndexPageHeaderSize + tableRun + rowid + blockRun + ordinal
	if off > len(page) {
		return -1
	}
	return off
}

// TestEncodeCrossTableDescending pins the cross-table ascending check: rows
// are sorted by (TableID, RowID), so a smaller TableID after a larger one is
// a builder bug and must be rejected.
func TestEncodeCrossTableDescending(t *testing.T) {
	desc := []format.RowIndexEntry{
		riEntry(2, 1, 1, 0, format.ChangeInsert),
		riEntry(1, 9, 1, 1, format.ChangeInsert),
	}
	_, _, _, _, err := encodePage(desc, indexPageEntryCount)
	if err == nil || !strings.Contains(err.Error(), "table ids not ascending") {
		t.Fatalf("encode(table-descending) = %v, want table-ids-not-ascending", err)
	}
}

// TestSplitCountExceedsRowIDStream forges a page whose header geometry is
// fully consistent (sums, change-bits width, body CRC all restamped) but whose
// EntryCount exceeds the decodable row-id stream: splitPage must reject it
// before the columnar decoder indexes out of the shorter streams.
func TestSplitCountExceedsRowIDStream(t *testing.T) {
	rows := riSeq(200, 50)
	page, _, _, _, err := encodePage(rows, indexPageEntryCount)
	if err != nil {
		t.Fatal(err)
	}
	m := cloneRI(t, page)
	// Drop the last byte of the row-id stream (offset HeaderSize+TableRun+RowID-1)
	// and shift the three trailing streams left one byte, shrinking the page by
	// one byte. RowIDBytes then decodes one value short of EntryCount.
	const (
		offTableRun        = format.IndexPageHeaderSize
		rowIDBytesOff      = 20
		blockRunBytesOff   = 24
		ordinalBytesOff    = 28
		changeBitsBytesOff = 32
		crcOff             = 60
	)
	tableRun := binary.LittleEndian.Uint32(m[16:20])
	rowIDBytes := binary.LittleEndian.Uint32(m[20:24])
	drop := offTableRun + tableRun + rowIDBytes - 1 // last row-id byte
	copy(m[drop:len(m)-1], m[drop+1:])
	m = m[:len(m)-1]
	binary.LittleEndian.PutUint32(m[rowIDBytesOff:], rowIDBytes-1)
	// BlockRun/Ordinal/ChangeBits sizes unchanged in value; their regions just
	// moved. EntryCount stays 200 (> decoded 199). Restamp the body CRC.
	binary.LittleEndian.PutUint32(m[crcOff:], format.CRC32C(m[format.IndexPageHeaderSize:]))

	if _, err := decodePage(m); err == nil || !strings.Contains(err.Error(), "exceeds row id stream") {
		t.Fatalf("decode(short row-id stream) = %v, want exceeds-row-id-stream", err)
	}
}
