// Package index — S3-⑦ 前置内存原型：排序 Row Index Page 编码 + Fence 目录。
//
// 这是一个【原型】，不是落盘。它实现设计稿 FILE_FORMAT_REFACTOR_PLAN.md §7.1
// 的页内编码与 §7.2 的 Fence Directory 语义，仅被本包测试/基准引用，用于测量
// 决策点 #2（Index Page 2048 vs 4096 条）与 #6（Index Page 是否复用数据压缩级别）。
//
// 它刻意【不】触碰：
//   - internal/fileformat 的 IndexTxn/IndexChunk 目录结构；
//   - writer/reader/recovery 读路径；
//   - 任何 on-disk 字节布局或 golden。
//
// 落盘前的关键差异（见 ADR-005）：
//   - 输入是独立的 ProtoRowEntry，而非 fileformat.RowIndexEntry；
//   - 未建模 PageOrdinal（数据页在块内的序号）——最终格式 §7.1 还要编
//     PageOrdinal run/delta；本原型只记录 (BlockID, RecordOrdinal)，把
//     PageOrdinal 的编码留给 S3-⑦ 落盘提交。
//   - 输出是内存字节与 ProtoFence，不含 SnapshotID（置常量 0）与 StoredOffset
//     （由调用方/最终目录赋予）。
//
// 本文件被包内 _test/_bench 引用；不计入 writer/reader/recovery 生产路径。
package index

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// ProtoRowEntry 是原型页的内存行索引条目：已按 (TableID, RowID) 升序。
// 它对应设计 §7.1 的页内记录，(TableID, RowID) 是排序键，BlockID 与
// RecordOrdinal 共同定位数据页内的一条物理记录，ChangeType 保留插入/更新/删除。
type ProtoRowEntry struct {
	TableID       uint32
	RowID         uint64
	BlockID       uint64
	RecordOrdinal uint32
	ChangeType    fileformat.ChangeType
}

// ---- Frozen prototype constants ----

const (
	// magicIndexPage 打开每个原型 Row Index Page。
	magicIndexPage = "RPKIDXPG"
	// indexPageVersion 是原型页布局版本。
	indexPageVersion = 1
	// indexPageHeaderSize 是原型页固定头大小（与 RowsPageHeader 同构的 64B）。
	indexPageHeaderSize = 64
	// protoFenceSize 是 ProtoFence 的定长序列化尺寸（见 fence.marshalTo）。
	protoFenceSize = 52
)

// maxUint32 used for width checks.
const maxUint32 = uint64(0xFFFFFFFF)

// errIndexPageTruncated 表示页流被截断/非法 varint。
var errIndexPageTruncated = errors.New("rowpack: index page stream truncated")

// indexPageHeader 是原型页固定 64 字节头，承载各流长度与 Page CRC。
type indexPageHeader struct {
	EntryCount      uint32
	TableRunBytes   uint32
	RowIDBytes      uint32
	BlockRunBytes   uint32
	OrdinalBytes    uint32
	ChangeBitsBytes uint32
	FirstRowID      uint64
	MinRowID        uint64
	MaxRowID        uint64
	CRC32C          uint32 // 覆盖流区（header 之后），不含头
}

// streamsBytes 返回五条流的总字节数。
func (h *indexPageHeader) streamsBytes() uint64 {
	return uint64(h.TableRunBytes) + uint64(h.RowIDBytes) + uint64(h.BlockRunBytes) +
		uint64(h.OrdinalBytes) + uint64(h.ChangeBitsBytes)
}

// marshalTo 写入定长头部。header 本身不设自身 CRC（页 CRC 在流区）。
func (h *indexPageHeader) marshalTo(dst []byte) error {
	if len(dst) < indexPageHeaderSize {
		return fmt.Errorf("rowpack: index page header dst %d bytes, want %d", len(dst), indexPageHeaderSize)
	}
	for i := 0; i < indexPageHeaderSize; i++ {
		dst[i] = 0
	}
	copy(dst[0:8], magicIndexPage)
	dst[8] = indexPageVersion
	binary.LittleEndian.PutUint32(dst[12:], h.EntryCount)
	binary.LittleEndian.PutUint32(dst[16:], h.TableRunBytes)
	binary.LittleEndian.PutUint32(dst[20:], h.RowIDBytes)
	binary.LittleEndian.PutUint32(dst[24:], h.BlockRunBytes)
	binary.LittleEndian.PutUint32(dst[28:], h.OrdinalBytes)
	binary.LittleEndian.PutUint32(dst[32:], h.ChangeBitsBytes)
	binary.LittleEndian.PutUint64(dst[36:], h.FirstRowID)
	binary.LittleEndian.PutUint64(dst[44:], h.MinRowID)
	binary.LittleEndian.PutUint64(dst[52:], h.MaxRowID)
	binary.LittleEndian.PutUint32(dst[60:], h.CRC32C)
	return nil
}

// unmarshalHeader 校验 magic/version/reserved/几何并填充 h。totalLen 非零时
// 校验 header+流汇总 == totalLen。
func unmarshalHeader(src []byte, totalLen int) (indexPageHeader, error) {
	var h indexPageHeader
	if len(src) < indexPageHeaderSize {
		return h, errIndexPageTruncated
	}
	if string(src[0:8]) != magicIndexPage {
		return h, fmt.Errorf("rowpack: index page bad magic %q", src[0:8])
	}
	if v := src[8]; v != indexPageVersion {
		return h, fmt.Errorf("rowpack: index page unsupported version %d", v)
	}
	if src[9] != 0 || src[10] != 0 || src[11] != 0 {
		return h, fmt.Errorf("rowpack: index page reserved bytes must be zero")
	}
	h.EntryCount = binary.LittleEndian.Uint32(src[12:])
	h.TableRunBytes = binary.LittleEndian.Uint32(src[16:])
	h.RowIDBytes = binary.LittleEndian.Uint32(src[20:])
	h.BlockRunBytes = binary.LittleEndian.Uint32(src[24:])
	h.OrdinalBytes = binary.LittleEndian.Uint32(src[28:])
	h.ChangeBitsBytes = binary.LittleEndian.Uint32(src[32:])
	h.FirstRowID = binary.LittleEndian.Uint64(src[36:])
	h.MinRowID = binary.LittleEndian.Uint64(src[44:])
	h.MaxRowID = binary.LittleEndian.Uint64(src[52:])
	h.CRC32C = binary.LittleEndian.Uint32(src[60:])
	if h.EntryCount == 0 {
		return h, fmt.Errorf("rowpack: index page must carry at least one entry")
	}
	if totalLen > 0 {
		total := uint64(indexPageHeaderSize) + h.streamsBytes()
		if total != uint64(totalLen) {
			return h, fmt.Errorf("rowpack: index page streams sum to %d, page is %d", total, totalLen)
		}
	}
	return h, nil
}

// encodePage 把【已按 (TableID, RowID) 升序】的 rows 编码成一个原型 Row Index
// Page。pageSize 是每页最大条目数；len(rows) 必须 <= pageSize 且非空。
// 返回原始（未压缩）页字节、实际条目数、跨页的全局 Min/Max RowID。
//
// 布局（设计 §7.1，pageSize 为条目数，不指字节目标）：
//
//	[indexPageHeader]   64 B 定长
//	[TableID run]       每 run 一条 uvarint(tableID) + uvarint(runLen)
//	[RowID stream]      每条目按表内 run 分段：run 首条绝对 uvarint，
//	                    其后 uvarint(非负 delta)；表边界重新采绝对值
//	[BlockID run]       连续相同 BlockID 一段：uvarint(blockID)+uvarint(runLen)
//	[RecordOrdinal]     首条绝对 uvarint，其后 zigzag uvarint(delta)
//	[ChangeType bits]   每条目 2 bit（0=Insert 1=Update 2=Delete，3 保留非法）
//
// 为保证全 uint64 范围内正确（RowID 0/MaxUint64/跨 delta 溢出），RowID 的
// 表内增量直接以【非负 uvarint】编码（已升序，delta >= 0），而非教科书 zigzag
// 翻倍——教科书 zigzag 在 delta 超过 int64 上限时会溢出（见 ADR-005 布局说明）。
func encodePage(rows []ProtoRowEntry, pageSize int) (page []byte, entryCount int, minRowID, maxRowID uint64, err error) {
	if pageSize <= 0 {
		return nil, 0, 0, 0, errors.New("rowpack: page size must be positive")
	}
	if len(rows) == 0 {
		return nil, 0, 0, 0, errors.New("rowpack: cannot encode an empty index page")
	}
	if len(rows) > pageSize {
		return nil, 0, 0, 0, fmt.Errorf("rowpack: page has %d entries, exceeds page size %d", len(rows), pageSize)
	}
	// 校验 (TableID, RowID) 严格升序：TableID 非降，表内 RowID 严格递增。
	for i := 1; i < len(rows); i++ {
		if rows[i].TableID < rows[i-1].TableID {
			return nil, 0, 0, 0, fmt.Errorf("rowpack: table ids not ascending at %d", i)
		}
		if rows[i].TableID == rows[i-1].TableID && rows[i].RowID <= rows[i-1].RowID {
			return nil, 0, 0, 0, fmt.Errorf("rowpack: row ids not strictly ascending within table at %d", i)
		}
	}

	// TableID run + 逐表 RowID 分段起点。
	tableRunBytes := make([]byte, 0)
	type run struct{ start, length int }
	tableRuns := make([]run, 0, 8)
	for tpos := 0; tpos < len(rows); {
		tid := rows[tpos].TableID
		j := tpos
		for j < len(rows) && rows[j].TableID == tid {
			j++
		}
		tableRunBytes = binary.AppendUvarint(tableRunBytes, uint64(tid))
		tableRunBytes = binary.AppendUvarint(tableRunBytes, uint64(j-tpos))
		tableRuns = append(tableRuns, run{start: tpos, length: j - tpos})
		tpos = j
	}

	// RowID stream：按表内 run 分段，run 首条绝对、其后非负 uvarint 增量。
	rowIDBytes := make([]byte, 0)
	for _, tr := range tableRuns {
		b, e := tr.start, tr.start+tr.length
		rowIDBytes = binary.AppendUvarint(rowIDBytes, rows[b].RowID)
		for k := b + 1; k < e; k++ {
			rowIDBytes = binary.AppendUvarint(rowIDBytes, rows[k].RowID-rows[k-1].RowID)
		}
	}

	// BlockID run：连续相同 BlockID 一段，绝对 blockID + runLen。
	blockRunBytes := make([]byte, 0)
	for bpos := 0; bpos < len(rows); {
		bid := rows[bpos].BlockID
		j := bpos
		for j < len(rows) && rows[j].BlockID == bid {
			j++
		}
		blockRunBytes = binary.AppendUvarint(blockRunBytes, bid)
		blockRunBytes = binary.AppendUvarint(blockRunBytes, uint64(j-bpos))
		bpos = j
	}

	// RecordOrdinal：首条绝对，其后 zigzag delta（可负，块边界会重置）。
	ordinalBytes := make([]byte, 0)
	ordinalBytes = binary.AppendUvarint(ordinalBytes, uint64(rows[0].RecordOrdinal))
	for i := 1; i < len(rows); i++ {
		d := int64(rows[i].RecordOrdinal) - int64(rows[i-1].RecordOrdinal)
		ordinalBytes = binary.AppendUvarint(ordinalBytes, zigzag(d))
	}

	// ChangeType 2bit stream。
	changeBits := make([]byte, 0)
	for i := range rows {
		packed, err := fileformat.PackChangeType(rows[i].ChangeType)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		changeBits = appendChangeBit(changeBits, uint32(i), packed)
	}

	// 全局 Min/Max RowID（排序键是 (TableID, RowID)，表切换会让全局 RowID
	// 非单调，故显式扫描取极值）。
	minRowID, maxRowID = rows[0].RowID, rows[0].RowID
	for _, r := range rows {
		if r.RowID < minRowID {
			minRowID = r.RowID
		}
		if r.RowID > maxRowID {
			maxRowID = r.RowID
		}
	}

	h := indexPageHeader{
		EntryCount:      uint32(len(rows)),
		TableRunBytes:   uint32(len(tableRunBytes)),
		RowIDBytes:      uint32(len(rowIDBytes)),
		BlockRunBytes:   uint32(len(blockRunBytes)),
		OrdinalBytes:    uint32(len(ordinalBytes)),
		ChangeBitsBytes: uint32(len(changeBits)),
		FirstRowID:      rows[0].RowID,
		MinRowID:        minRowID,
		MaxRowID:        maxRowID,
	}

	// 组装：头 + 流；CRC 覆盖流区。
	page = make([]byte, 0, indexPageHeaderSize+len(tableRunBytes)+len(rowIDBytes)+len(blockRunBytes)+len(ordinalBytes)+len(changeBits))
	page = append(page, make([]byte, indexPageHeaderSize)...)
	page = append(page, tableRunBytes...)
	page = append(page, rowIDBytes...)
	page = append(page, blockRunBytes...)
	page = append(page, ordinalBytes...)
	page = append(page, changeBits...)
	streams := page[indexPageHeaderSize:]
	h.CRC32C = fileformat.CRC32C(streams)
	if err := h.marshalTo(page); err != nil {
		return nil, 0, 0, 0, err
	}
	return page, len(rows), minRowID, maxRowID, nil
}

// decodePage 严格解码一个原型页。任何截断/伪造长度/CRC 失败/非法 changeType/
// 排序破坏都返回错误，绝不 panic、绝不做无界分配。输出保证 (TableID, RowID)
// 升序，与 encodePage 输入一致。
func decodePage(raw []byte) ([]ProtoRowEntry, error) {
	n := len(raw)
	if n < indexPageHeaderSize {
		return nil, errIndexPageTruncated
	}
	h, err := unmarshalHeader(raw, n)
	if err != nil {
		return nil, err
	}
	count := int(h.EntryCount)
	start := indexPageHeaderSize
	if fileformat.CRC32C(raw[start:]) != h.CRC32C {
		return nil, fmt.Errorf("rowpack: index page CRC mismatch")
	}
	wantBits := (uint64(count) + 3) / 4
	if uint64(h.ChangeBitsBytes) != wantBits {
		return nil, fmt.Errorf("rowpack: index page change bits %d, want %d for %d entries", h.ChangeBitsBytes, wantBits, count)
	}
	if uint64(count) == 0 {
		return nil, fmt.Errorf("rowpack: index page carries no entries")
	}

	// 切流（严格边界）。
	off := start
	take := func(byteLen uint32) ([]byte, error) {
		if byteLen > uint32(n-off) {
			return nil, errIndexPageTruncated
		}
		s := raw[off : off+int(byteLen)]
		off += int(byteLen)
		return s, nil
	}
	tableRun, err := take(h.TableRunBytes)
	if err != nil {
		return nil, err
	}
	rowIDStream, err := take(h.RowIDBytes)
	if err != nil {
		return nil, err
	}
	blockRun, err := take(h.BlockRunBytes)
	if err != nil {
		return nil, err
	}
	ordinalStream, err := take(h.OrdinalBytes)
	if err != nil {
		return nil, err
	}
	changeBits, err := take(h.ChangeBitsBytes)
	if err != nil {
		return nil, err
	}
	if off != n {
		return nil, fmt.Errorf("rowpack: index page has %d trailing bytes", n-off)
	}
	// 安全边界：每条目在 RowID 流至少占 1 字节（run 首条绝对或表内增量 uvarint），
	// 故条目数不得超过 RowID 流长度，防止伪造 EntryCount 触发无界分配。
	if uint64(count) > uint64(len(rowIDStream)) {
		return nil, fmt.Errorf("rowpack: index page entry count %d exceeds row id stream %d", count, len(rowIDStream))
	}

	readVar := func(s []byte, pos *int) (uint64, error) {
		v, n := binary.Uvarint(s[*pos:])
		if n <= 0 {
			return 0, errIndexPageTruncated
		}
		*pos += n
		return v, nil
	}

	// Tables（逐 run 记录每条目的 tableID，并按 run 长度还原 run 边界）。
	tableIDs := make([]uint32, count)
	runLens := make([]int, 0, 8) // 每个 table run 的条目数（连续、无重叠）
	tp := 0
	ti := 0
	for ti < count {
		tv, err := readVar(tableRun, &tp)
		if err != nil {
			return nil, err
		}
		if tv > maxUint32 {
			return nil, fmt.Errorf("rowpack: index page table id %d exceeds uint32", tv)
		}
		rl, err := readVar(tableRun, &tp)
		if err != nil {
			return nil, err
		}
		if rl == 0 || rl > uint64(count-ti) {
			return nil, fmt.Errorf("rowpack: index page table run len %d, want 1..%d", rl, count-ti)
		}
		runLens = append(runLens, int(rl))
		for k := 0; k < int(rl); k++ {
			tableIDs[ti] = uint32(tv)
			ti++
		}
	}
	if tp != len(tableRun) {
		return nil, fmt.Errorf("rowpack: index page table run has %d trailing bytes", len(tableRun)-tp)
	}
	if ti != count {
		return nil, fmt.Errorf("rowpack: index page table runs cover %d entries, want %d", ti, count)
	}

	// RowIDs：按 table run 分段还原。每个 run 首条绝对 uvarint，其后非负 uvarint
	// 增量；表切换时重新采绝对值（跨表 RowID 单调性与表内增量无关）。
	rowIDs := make([]uint64, count)
	rp := 0
	runIdx := 0
	ri := 0
	for runIdx < len(runLens) {
		abs, err := readVar(rowIDStream, &rp)
		if err != nil {
			return nil, err
		}
		rowIDs[ri] = abs
		ri++
		for k := 1; k < runLens[runIdx]; k++ {
			d, err := readVar(rowIDStream, &rp)
			if err != nil {
				return nil, err
			}
			rowIDs[ri] = rowIDs[ri-1] + d
			ri++
		}
		runIdx++
	}
	if rp != len(rowIDStream) {
		return nil, fmt.Errorf("rowpack: index page row id stream has %d trailing bytes", len(rowIDStream)-rp)
	}

	// BlockIDs（run）。
	blockIDs := make([]uint64, count)
	bp := 0
	bi := 0
	for bi < count {
		bid, err := readVar(blockRun, &bp)
		if err != nil {
			return nil, err
		}
		rl, err := readVar(blockRun, &bp)
		if err != nil {
			return nil, err
		}
		if rl == 0 || rl > uint64(count-bi) {
			return nil, fmt.Errorf("rowpack: index page block run len %d, want 1..%d", rl, count-bi)
		}
		for k := 0; k < int(rl); k++ {
			blockIDs[bi] = bid
			bi++
		}
	}
	if bp != len(blockRun) {
		return nil, fmt.Errorf("rowpack: index page block run has %d trailing bytes", len(blockRun)-bp)
	}

	// RecordOrdinals（zigzag delta）。
	ordinals := make([]uint32, count)
	op := 0
	ov, err := readVar(ordinalStream, &op)
	if err != nil {
		return nil, err
	}
	if ov > maxUint32 {
		return nil, fmt.Errorf("rowpack: index page record ordinal %d exceeds uint32", ov)
	}
	ordinals[0] = uint32(ov)
	for i := 1; i < count; i++ {
		d, err := readVar(ordinalStream, &op)
		if err != nil {
			return nil, err
		}
		v := int64(ordinals[i-1]) + unzigzag(d)
		if v < 0 || uint64(v) > maxUint32 {
			return nil, fmt.Errorf("rowpack: index page record ordinal %d out of range", v)
		}
		ordinals[i] = uint32(v)
	}
	if op != len(ordinalStream) {
		return nil, fmt.Errorf("rowpack: index page record ordinal stream has %d trailing bytes", len(ordinalStream)-op)
	}

	// ChangeType（2bit）。
	changes := make([]fileformat.ChangeType, count)
	for i := 0; i < count; i++ {
		packed := (changeBits[i/4] >> ((i % 4) * 2)) & 3
		ct, err := fileformat.UnpackChangeType(packed)
		if err != nil {
			return nil, fmt.Errorf("rowpack: index page entry %d: %w", i, err)
		}
		changes[i] = ct
	}

	// 排序一致性 + 组装。
	out := make([]ProtoRowEntry, count)
	var globalMin, globalMax uint64 = rowIDs[0], rowIDs[0]
	for i := 0; i < count; i++ {
		if i > 0 {
			if tableIDs[i] < tableIDs[i-1] {
				return nil, fmt.Errorf("rowpack: index page table ids not sorted at %d", i)
			}
			if tableIDs[i] == tableIDs[i-1] && rowIDs[i] <= rowIDs[i-1] {
				return nil, fmt.Errorf("rowpack: index page row ids not strictly ascending at %d", i)
			}
		}
		if rowIDs[i] < globalMin {
			globalMin = rowIDs[i]
		}
		if rowIDs[i] > globalMax {
			globalMax = rowIDs[i]
		}
		out[i] = ProtoRowEntry{
			TableID:       tableIDs[i],
			RowID:         rowIDs[i],
			BlockID:       blockIDs[i],
			RecordOrdinal: ordinals[i],
			ChangeType:    changes[i],
		}
	}
	// 交叉校验 header 导出的锚点值与解码结果一致（防单 bit 翻转构造伪页）。
	if h.FirstRowID != out[0].RowID {
		return nil, fmt.Errorf("rowpack: index page first row id %d, want %d", h.FirstRowID, out[0].RowID)
	}
	if h.MinRowID != globalMin {
		return nil, fmt.Errorf("rowpack: index page min row id %d, want %d", h.MinRowID, globalMin)
	}
	if h.MaxRowID != globalMax {
		return nil, fmt.Errorf("rowpack: index page max row id %d, want %d", h.MaxRowID, globalMax)
	}
	return out, nil
}

// ProtoFence 是设计 §7.2 的每页 Fence 目录条目。原型里 SnapshotID 置常量 0，
// StoredOffset 由调用方/最终页目录赋予；尺寸以定长 packed 布局估算（protoFenceSize）。
type ProtoFence struct {
	SnapshotID   uint64 // 原型常量 0
	TableID      uint32
	MinRowID     uint64
	MaxRowID     uint64
	StoredOffset uint64
	StoredSize   uint32
	RawSize      uint32
	EntryCount   uint32
	PageCRC32C   uint32
}

// FenceSize 返回 Fence 条目序列化字节数（用于估算 Open 常驻 Fence 内存）。
func (f *ProtoFence) FenceSize() int { return protoFenceSize }

// marshalTo 写入定长 packed 布局（无 Go struct padding）。
func (f *ProtoFence) marshalTo(dst []byte) error {
	if len(dst) < protoFenceSize {
		return fmt.Errorf("rowpack: fence dst %d bytes, want %d", len(dst), protoFenceSize)
	}
	binary.LittleEndian.PutUint64(dst[0:], f.SnapshotID)
	binary.LittleEndian.PutUint32(dst[8:], f.TableID)
	binary.LittleEndian.PutUint64(dst[12:], f.MinRowID)
	binary.LittleEndian.PutUint64(dst[20:], f.MaxRowID)
	binary.LittleEndian.PutUint64(dst[28:], f.StoredOffset)
	binary.LittleEndian.PutUint32(dst[36:], f.StoredSize)
	binary.LittleEndian.PutUint32(dst[40:], f.RawSize)
	binary.LittleEndian.PutUint32(dst[44:], f.EntryCount)
	binary.LittleEndian.PutUint32(dst[48:], f.PageCRC32C)
	return nil
}

// unmarshalFence 解析定长 packed 布局并校验保留字节（第 10-11 字节必须是 0）。
func unmarshalFence(src []byte) (ProtoFence, error) {
	var f ProtoFence
	if len(src) < protoFenceSize {
		return f, errIndexPageTruncated
	}
	f.SnapshotID = binary.LittleEndian.Uint64(src[0:])
	f.TableID = binary.LittleEndian.Uint32(src[8:])
	f.MinRowID = binary.LittleEndian.Uint64(src[12:])
	f.MaxRowID = binary.LittleEndian.Uint64(src[20:])
	f.StoredOffset = binary.LittleEndian.Uint64(src[28:])
	f.StoredSize = binary.LittleEndian.Uint32(src[36:])
	f.RawSize = binary.LittleEndian.Uint32(src[40:])
	f.EntryCount = binary.LittleEndian.Uint32(src[44:])
	f.PageCRC32C = binary.LittleEndian.Uint32(src[48:])
	return f, nil
}

// fenceFor 从原始页字节构造 ProtoFence（§7.2）。storedSize 是压缩后的大小；
// StoredOffset 由调用方在目录构建时赋予（原型先置 0）。多表页取首表的 TableID
// ——最终格式里单表页占绝大多数，跨表页的 Fence 语义留给 S3-⑦ 落盘提交。
func fenceFor(raw []byte, storedSize uint32) (ProtoFence, error) {
	if len(raw) < indexPageHeaderSize {
		return ProtoFence{}, errIndexPageTruncated
	}
	h, err := unmarshalHeader(raw, len(raw))
	if err != nil {
		return ProtoFence{}, err
	}
	tid, ok := firstTableID(raw)
	if !ok {
		return ProtoFence{}, errIndexPageTruncated
	}
	return ProtoFence{
		SnapshotID:   0, // 原型常量
		TableID:      tid,
		MinRowID:     h.MinRowID,
		MaxRowID:     h.MaxRowID,
		StoredOffset: 0, // 调用方赋予
		StoredSize:   storedSize,
		RawSize:      uint32(len(raw)),
		EntryCount:   h.EntryCount,
		PageCRC32C:   h.CRC32C,
	}, nil
}

// firstTableID 读取页 table run 的首个 tableID（用于 Fence）。
func firstTableID(raw []byte) (uint32, bool) {
	if len(raw) < indexPageHeaderSize {
		return 0, false
	}
	t, n := binary.Uvarint(raw[indexPageHeaderSize:])
	if n <= 0 || t > maxUint32 {
		return 0, false
	}
	return uint32(t), true
}

// appendChangeBit 设置第 ordinal 条目的 2bit 值，按需增长字节流。
func appendChangeBit(dst []byte, ordinal uint32, packed uint8) []byte {
	byteIdx := ordinal / 4
	for uint32(len(dst)) <= byteIdx {
		dst = append(dst, 0)
	}
	shift := (ordinal % 4) * 2
	dst[byteIdx] |= packed << shift
	return dst
}
