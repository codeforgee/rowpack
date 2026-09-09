package index

import (
	"math"
	"math/rand"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// page_proto_test.go — S3-⑦ 前置原型单元测试。
//
// 目标：encodePage/decodePage/fenceFor 的往返一致性与严格损坏校验（截断、单 bit
// 翻转、伪造 size/count、非法 changeType、排序破坏）必须报错且绝不 panic/无界分配。

// rowsEqual 比较两个 ProtoRowEntry 切片逐字段相等。
func rowsEqual(a, b []ProtoRowEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// mustEncode 编码并断言成功。
func mustEncode(t *testing.T, rows []ProtoRowEntry, pageSize int) ([]byte, int, uint64, uint64) {
	t.Helper()
	page, n, mn, mx, err := encodePage(rows, pageSize)
	if err != nil {
		t.Fatalf("encodePage(%d rows, pageSize=%d): %v", len(rows), pageSize, err)
	}
	return page, n, mn, mx
}

// roundTrip 编码后解码，断言返回的条目与输入一致且元数据锚点正确。
func roundTrip(t *testing.T, rows []ProtoRowEntry, pageSize int) []byte {
	t.Helper()
	page, n, mn, mx := mustEncode(t, rows, pageSize)
	if n != len(rows) {
		t.Fatalf("entryCount = %d, want %d", n, len(rows))
	}
	got, err := decodePage(page)
	if err != nil {
		t.Fatalf("decodePage: %v", err)
	}
	if !rowsEqual(got, rows) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, rows)
	}
	// 校验 header 导出的极值与输入一致。
	var wmn, wmx = rows[0].RowID, rows[0].RowID
	for _, r := range rows {
		if r.RowID < wmn {
			wmn = r.RowID
		}
		if r.RowID > wmx {
			wmx = r.RowID
		}
	}
	if mn != wmn || mx != wmx {
		t.Fatalf("min/max = %d/%d, want %d/%d", mn, mx, wmn, wmx)
	}
	return page
}

func TestIndexPageSingleTableSequential(t *testing.T) {
	n := 300
	rows := make([]ProtoRowEntry, n)
	for i := 0; i < n; i++ {
		rows[i] = ProtoRowEntry{
			TableID:       1,
			RowID:         uint64(i) + 1,
			BlockID:       uint64(i/100 + 1), // 每 100 行一个块
			RecordOrdinal: uint32(i % 100),
			ChangeType:    changeFor(i),
		}
	}
	roundTrip(t, rows, 4096)
}

func TestIndexPageMultiTableRun(t *testing.T) {
	rows := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 10, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 2, BlockID: 10, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 3, BlockID: 11, RecordOrdinal: 0, ChangeType: fileformat.ChangeUpdate},
		{TableID: 1, RowID: 4, BlockID: 11, RecordOrdinal: 1, ChangeType: fileformat.ChangeDelete},
		// 切换到表 2：RowID 从头开始，RowID 流必须重新采绝对值。
		{TableID: 2, RowID: 7, BlockID: 20, RecordOrdinal: 3, ChangeType: fileformat.ChangeInsert},
		{TableID: 2, RowID: 9, BlockID: 20, RecordOrdinal: 5, ChangeType: fileformat.ChangeInsert},
		{TableID: 2, RowID: 20, BlockID: 21, RecordOrdinal: 0, ChangeType: fileformat.ChangeDelete},
		{TableID: 3, RowID: 5, BlockID: 30, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
	}
	roundTrip(t, rows, 4096)
}

func TestIndexPageRandomPreSorted(t *testing.T) {
	seed := int64(0x52504B32)
	rng := rand.New(rand.NewSource(seed))
	n := 500
	ids := make([]uint64, n)
	for i := range ids {
		ids[i] = uint64(i) + 1
	}
	rng.Shuffle(n, func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	rows := make([]ProtoRowEntry, n)
	for i, id := range ids {
		rows[i] = ProtoRowEntry{
			TableID:       1,
			RowID:         id,
			BlockID:       id / 50,
			RecordOrdinal: uint32(id % 50),
			ChangeType:    fileformat.ChangeInsert,
		}
	}
	// 乱序后编码前必须先按 (TableID, RowID) 升序。
	sortProtoRows(rows)
	roundTrip(t, rows, 4096)
}

func TestIndexPageRowIDBoundaries(t *testing.T) {
	// RowID 0、MaxUint64、跨 delta 溢出级别的跨度；同一表内必须严格递增。
	rows := []ProtoRowEntry{
		{TableID: 1, RowID: 0, BlockID: 1, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: math.MaxUint64 - 1, BlockID: 2, RecordOrdinal: 0, ChangeType: fileformat.ChangeUpdate},
		{TableID: 1, RowID: math.MaxUint64, BlockID: 2, RecordOrdinal: 1, ChangeType: fileformat.ChangeDelete},
	}
	roundTrip(t, rows, 4096)
}

func TestIndexPageBlockRuns(t *testing.T) {
	// 多个 block run，且块 ID 与 ordinal 都在块边界重置。
	rows := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 5, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 2, BlockID: 5, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 3, BlockID: 7, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert}, // block 切换，ordinal 归零
		{TableID: 1, RowID: 4, BlockID: 7, RecordOrdinal: 1, ChangeType: fileformat.ChangeDelete},
		{TableID: 1, RowID: 5, BlockID: 9, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
	}
	roundTrip(t, rows, 4096)
}

func TestIndexPageRecordOrdinalDelta(t *testing.T) {
	// 块内孤行导致 ordinal 大幅回落（负 delta），必须用 zigzag 正确还原。
	rows := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 100, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 2, BlockID: 1, RecordOrdinal: 101, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 3, BlockID: 2, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert}, // -101 delta
		{TableID: 1, RowID: 4, BlockID: 2, RecordOrdinal: 1, ChangeType: fileformat.ChangeDelete},
		{TableID: 1, RowID: 5, BlockID: 2, RecordOrdinal: math.MaxUint32, ChangeType: fileformat.ChangeUpdate},
	}
	roundTrip(t, rows, 4096)
}

func TestIndexPageChangeDelete(t *testing.T) {
	rows := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 2, BlockID: 1, RecordOrdinal: 1, ChangeType: fileformat.ChangeUpdate},
		{TableID: 1, RowID: 3, BlockID: 1, RecordOrdinal: 2, ChangeType: fileformat.ChangeDelete},
	}
	// 往返已断言逐字段一致（含 ChangeDelete）；这里再确认第 3 条解码出的即 DELETE。
	page, _, _, _ := mustEncode(t, rows, 4096)
	got, err := decodePage(page)
	if err != nil {
		t.Fatalf("decodePage: %v", err)
	}
	if len(got) < 3 || got[2].ChangeType != fileformat.ChangeDelete {
		t.Fatalf("entry[2].ChangeType = %v, want ChangeDelete (got=%+v)", got[2].ChangeType, got)
	}
}

// sortProtoRows 按 (TableID, RowID) 升序就地排序（原型假定已升序）。
func sortProtoRows(rows []ProtoRowEntry) {
	// 排序键：TableID 升序，表内 RowID 升序。
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && protoLess(rows[j], rows[j-1]); j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

func protoLess(a, b ProtoRowEntry) bool {
	if a.TableID != b.TableID {
		return a.TableID < b.TableID
	}
	return a.RowID < b.RowID
}

// changeFor 返回一轮变化类型（Insert/Update/Delete 混合）。
func changeFor(i int) fileformat.ChangeType {
	switch i % 7 {
	case 3:
		return fileformat.ChangeUpdate
	case 5:
		return fileformat.ChangeDelete
	default:
		return fileformat.ChangeInsert
	}
}

// ---- encode 错误分支 ----

func TestIndexPageEncodeErrors(t *testing.T) {
	rows := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 2, BlockID: 1, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
	}
	if _, _, _, _, err := encodePage(nil, 4096); err == nil {
		t.Fatal("encodePage(nil) = nil error")
	}
	if _, _, _, _, err := encodePage(rows, 0); err == nil {
		t.Fatal("encodePage(pageSize=0) = nil error")
	}
	if _, _, _, _, err := encodePage(rows, 1); err == nil {
		t.Fatal("encodePage(rows > pageSize) = nil error")
	}
	// 未排序 / 表内重复 RowID。
	bad := []ProtoRowEntry{
		{TableID: 1, RowID: 2, BlockID: 1, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
	}
	if _, _, _, _, err := encodePage(bad, 4096); err == nil {
		t.Fatal("encodePage(unsorted) = nil error")
	}
	dup := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 0, ChangeType: fileformat.ChangeInsert},
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 1, ChangeType: fileformat.ChangeInsert},
	}
	if _, _, _, _, err := encodePage(dup, 4096); err == nil {
		t.Fatal("encodePage(duplicate row id) = nil error")
	}
	// 非法 changeType（ChangeInsert/Update/Delete=1/2/3 之外，如 0）。
	badCT := []ProtoRowEntry{
		{TableID: 1, RowID: 1, BlockID: 1, RecordOrdinal: 0, ChangeType: fileformat.ChangeType(0)},
	}
	if _, _, _, _, err := encodePage(badCT, 4096); err == nil {
		t.Fatal("encodePage(changeType=0) = nil error")
	}
}

// ---- decode 损坏矩阵 ----

// mutate 复制并修改 src 的一个字节/位。
func clonePage(t *testing.T, page []byte) []byte {
	t.Helper()
	c := make([]byte, len(page))
	copy(c, page)
	return c
}

func TestIndexPageDecodeTruncated(t *testing.T) {
	rows := mkSeq(200, 50)
	page, _, _, _ := mustEncode(t, rows, 4096)
	// 逐一切短：头部不完整、流被截断。
	if _, err := decodePage(page[:indexPageHeaderSize-1]); err == nil {
		t.Fatal("decodePage(short header) = nil error")
	}
	for _, cut := range []int{indexPageHeaderSize + 1, indexPageHeaderSize + 5, len(page) / 2, len(page) - 1} {
		if cut >= len(page) {
			continue
		}
		if _, err := decodePage(page[:cut]); err == nil {
			t.Fatalf("decodePage(truncated %d/%d) = nil error", cut, len(page))
		}
	}
}

func TestIndexPageDecodeBitFlip(t *testing.T) {
	rows := mkSeq(200, 50)
	page, _, _, _ := mustEncode(t, rows, 4096)
	offsets := []int{
		1,                       // magic/version
		12,                      // EntryCount 低位
		16,                      // TableRunBytes
		36,                      // FirstRowID 低位
		60,                      // CRC 低位
		indexPageHeaderSize,     // 流区首字节
		indexPageHeaderSize + 8, // 流区中部
		len(page) - 1,           // 流区末字节
	}
	for _, off := range offsets {
		if off < 0 || off >= len(page) {
			continue
		}
		m := clonePage(t, page)
		m[off] ^= 0x01
		if _, err := decodePage(m); err == nil {
			t.Fatalf("decodePage(bit flip @%d) = nil error", off)
		}
	}
}

func TestIndexPageDecodeForgedSize(t *testing.T) {
	rows := mkSeq(100, 50)
	page, _, _, _ := mustEncode(t, rows, 4096)
	// 伪造 StreamBytes（表 run 长度 +1），几何汇总应被拒绝。
	m := clonePage(t, page)
	orig := m[20] // table run 低字节
	_ = orig
	m[20] = m[20] + 1 // 改动 TableRunBytes 低位
	if _, err := decodePage(m); err == nil {
		t.Fatal("decodePage(forged table run size) = nil error")
	}
	// 伪造小 changeBits 长度。
	m2 := clonePage(t, page)
	m2[32] = 0 // ChangeBitsBytes = 0
	if _, err := decodePage(m2); err == nil {
		t.Fatal("decodePage(forged change bits size) = nil error")
	}
}

func TestIndexPageDecodeForgedCount(t *testing.T) {
	rows := mkSeq(64, 16)
	page, _, _, _ := mustEncode(t, rows, 4096)
	// EntryCount = MaxUint32，必须在分配前报错，不得 OOM/panic。
	m := clonePage(t, page)
	m[12] = 0xFF
	m[13] = 0xFF
	m[14] = 0xFF
	m[15] = 0x7F
	if _, err := decodePage(m); err == nil {
		t.Fatal("decodePage(forged entry count) = nil error")
	}
}

func TestIndexPageDecodeIllegalChangeType(t *testing.T) {
	// 伪造某条目的 change 2bit = 3（保留非法），必须报错。
	rows := mkSeq(8, 8)
	page, _, _, _ := mustEncode(t, rows, 4096)
	m := clonePage(t, page)
	cbOff := changeBitsOffset(page)
	if cbOff < 0 {
		t.Fatalf("cannot locate change bits region (%d)", cbOff)
	}
	m[cbOff] = m[cbOff] | 0x03 // 第 0 条目标记为 3
	if _, err := decodePage(m); err == nil {
		t.Fatal("decodePage(illegal change type 3) = nil error")
	}
}

func TestIndexPageDecodeCorruptCRC(t *testing.T) {
	rows := mkSeq(50, 10)
	page, _, _, _ := mustEncode(t, rows, 4096)
	// 在流区字节修改（CRC 会失配）。
	m := clonePage(t, page)
	m[indexPageHeaderSize] ^= 0x40
	if _, err := decodePage(m); err == nil {
		t.Fatal("decodePage(stream mutation) = nil error")
	}
}

// ---- Fence ----

func TestFenceFor(t *testing.T) {
	rows := mkSeq(100, 25)
	page, n, mn, mx := mustEncode(t, rows, 4096)
	f, err := fenceFor(page, 42)
	if err != nil {
		t.Fatalf("fenceFor: %v", err)
	}
	if f.TableID != 1 {
		t.Fatalf("fence TableID = %d, want 1", f.TableID)
	}
	if f.EntryCount != uint32(n) || f.EntryCount != uint32(len(rows)) {
		t.Fatalf("fence EntryCount = %d, want %d", f.EntryCount, len(rows))
	}
	if f.MinRowID != mn || f.MaxRowID != mx {
		t.Fatalf("fence min/max = %d/%d, want %d/%d", f.MinRowID, f.MaxRowID, mn, mx)
	}
	if f.RawSize != uint32(len(page)) {
		t.Fatalf("fence RawSize = %d, want %d", f.RawSize, len(page))
	}
	if f.StoredSize != 42 {
		t.Fatalf("fence StoredSize = %d, want 42", f.StoredSize)
	}
	if f.PageCRC32C != pageCRC32C(page) {
		t.Fatalf("fence PageCRC32C mismatch")
	}
	if f.FenceSize() != protoFenceSize {
		t.Fatalf("fence size = %d, want %d", f.FenceSize(), protoFenceSize)
	}
}

func TestFenceMarshalRoundTrip(t *testing.T) {
	f := ProtoFence{
		SnapshotID:   0,
		TableID:      7,
		MinRowID:     100,
		MaxRowID:     999,
		StoredOffset: 0x1234,
		StoredSize:   77,
		RawSize:      500,
		EntryCount:   42,
		PageCRC32C:   0xDEADBEEF,
	}
	buf := make([]byte, protoFenceSize)
	if err := f.marshalTo(buf); err != nil {
		t.Fatalf("marshalTo: %v", err)
	}
	got, err := unmarshalFence(buf)
	if err != nil {
		t.Fatalf("unmarshalFence: %v", err)
	}
	if got != f {
		t.Fatalf("fence round-trip mismatch: got=%+v want=%+v", got, f)
	}
	if _, err := unmarshalFence(buf[:protoFenceSize-1]); err == nil {
		t.Fatal("unmarshalFence(short) = nil error")
	}
}

// ---- helpers ----

// mkSeq 构造单表顺序数据：每 blkEvery 行切一个块，ordinal 为块内序号。
func mkSeq(n, blkEvery int) []ProtoRowEntry {
	rows := make([]ProtoRowEntry, n)
	for i := 0; i < n; i++ {
		rows[i] = ProtoRowEntry{
			TableID:       1,
			RowID:         uint64(i) + 1,
			BlockID:       uint64(i/blkEvery + 1),
			RecordOrdinal: uint32(i % blkEvery),
			ChangeType:    fileformat.ChangeInsert,
		}
	}
	return rows
}

// changeBitsOffset 扫描页各流长度，返回 change bits 区起点；失败返回 -1。
func changeBitsOffset(page []byte) int {
	if len(page) < indexPageHeaderSize {
		return -1
	}
	// 直接读 header 的五流长度（偏移须与 marshal 一致）。
	tableRun := int(uint32LE(page, 16))
	rowid := int(uint32LE(page, 20))
	blockRun := int(uint32LE(page, 24))
	ordinal := int(uint32LE(page, 28))
	off := indexPageHeaderSize + tableRun + rowid + blockRun + ordinal
	if off > len(page) {
		return -1
	}
	return off
}

func uint32LE(b []byte, off int) uint32 {
	return uint32(b[off]) | uint32(b[off+1])<<8 | uint32(b[off+2])<<16 | uint32(b[off+3])<<24
}

// pageCRC32C 返回页流区 CRC（与 encodePage 封装的 h.CRC32C 一致）。
func pageCRC32C(page []byte) uint32 {
	return fileformat.CRC32C(page[indexPageHeaderSize:])
}
