package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// row_index_page.go — S3-⑦ 落盘②：把 IndexTxn 正文的行索引从 chunk delta 编码切换为
// 排序 Row Index Page + Fence Directory（FILE_FORMAT_REFACTOR_PLAN §7.1/§7.2、
// ADR-005）。本文件把原型 page_proto.go 的 encodePage/decodePage/fenceFor 从
// ProtoRowEntry 泛化到 fileformat.RowIndexEntry（RecordOrdinal 语义 == ItemOrdinal，
// 块内序号、块边界重置），并改用已冻结的 fileformat.RowIndexPageHeader /
// fileformat.RowIndexFenceEntry。原型文件保持不变（测量工件），本文件才是生产实现。

// indexPageEntryCount 是每页最大行条目数（ADR-005 决策 #2 = 4096）。
const indexPageEntryCount = 4096

// rowIndexPageChunkKind is the chunk-sealing kind used for index pages so an
// encrypted store seals pages under the Index-domain nonce/AAD space (R11).
// It reuses the row kind because pages are the row index; pages seal under
// chunk sequences beyond every chunk, so no nonce collision occurs.
const rowIndexPageChunkKind = fileformat.IndexChunkKindRow

// errIndexPageCorrupt 表示页内容损坏。
var errIndexPageCorrupt = errors.New("rowpack: index page corrupt")

// rowIndexPageBuild is one encoded (and optionally compressed/sealed) index
// page plus its fence directory entry, produced by the builder for the txn
// body. raw is the uncompressed page (for the plaintext-body CRC); stored is
// the compressed (and, when encrypted, sealed) page bytes written to disk.
type rowIndexPageBuild struct {
	raw    []byte
	stored []byte
	fence  fileformat.RowIndexFenceEntry
}

// buildRowIndexPages sorts the builder's rows by (TableID, RowID), partitions
// them into indexPageEntryCount-entry pages, encodes/compresses (and, when
// crypto != nil, seals) each page, and returns the pages plus their fence
// entries. Fence.StoredOffset is zero here and patched by the caller once the
// body layout (chunk region + directory) is known. pageSeqBase is the first
// free chunk sequence in the surrounding txn so pages seal under distinct
// nonces (ADR-005 / R11).
func (b *Builder) buildRowIndexPages(crypto *ChunkCrypto, level int, pageSeqBase uint32) ([]rowIndexPageBuild, error) {
	n := len(b.rows)
	if n == 0 {
		return nil, nil
	}
	sortRowIndexEntries(b.rows)
	out := make([]rowIndexPageBuild, 0, (n+indexPageEntryCount-1)/indexPageEntryCount)
	for start := 0; start < n; start += indexPageEntryCount {
		end := start + indexPageEntryCount
		if end > n {
			end = n
		}
		page, _, _, _, err := encodeRowIndexPage(b.rows[start:end], indexPageEntryCount)
		if err != nil {
			return nil, err
		}
		stored, err := block.Compress(fileformat.CompressionZstd, level, page)
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
		fence, err := fenceForRowIndexPage(page, storedSize, b.snapshot.SnapshotID, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, rowIndexPageBuild{raw: page, stored: stored, fence: fence})
	}
	return out, nil
}

// sortRowIndexEntries 把行索引条目就地按 (TableID, RowID) 升序排序（决定 #1：
// 逻辑位序保留 ItemOrdinal，排序键仍是 (TableID, RowID)）。用 sort.Slice 而非
// 手写插入排序，避免恢复重建路径在完全乱序的大快照上退化到 O(n^2)。
func sortRowIndexEntries(entries []fileformat.RowIndexEntry) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.TableID != b.TableID {
			return a.TableID < b.TableID
		}
		return a.RowID < b.RowID
	})
}

// encodeRowIndexPage 把【已按 (TableID, RowID) 升序】的 entries 编码成一个 Row Index
// Page，页头为冻结的 fileformat.RowIndexPageHeader。pageSize 是每页最大条目数；
// len(entries) 必须 <= pageSize 且非空。返回未压缩页字节、实际条目数、跨表全局
// Min/Max RowID（排序键是 (TableID, RowID)，表切换会让全局 RowID 非单调，故显式扫描）。
//
// 布局与原型 encodePage 一致：TableID run、RowID 表内非负 uvarint delta、BlockID run、
// ItemOrdinal zigzag delta、ChangeType 2bit；页 CRC 覆盖流区。
func encodeRowIndexPage(entries []fileformat.RowIndexEntry, pageSize int) (page []byte, entryCount int, minRowID, maxRowID uint64, err error) {
	if pageSize <= 0 {
		return nil, 0, 0, 0, errors.New("rowpack: page size must be positive")
	}
	if len(entries) == 0 {
		return nil, 0, 0, 0, errors.New("rowpack: cannot encode an empty index page")
	}
	if len(entries) > pageSize {
		return nil, 0, 0, 0, fmt.Errorf("rowpack: page has %d entries, exceeds page size %d", len(entries), pageSize)
	}
	// 校验 (TableID, RowID) 严格升序：TableID 非降，表内 RowID 严格递增。
	for i := 1; i < len(entries); i++ {
		if entries[i].TableID < entries[i-1].TableID {
			return nil, 0, 0, 0, fmt.Errorf("rowpack: table ids not ascending at %d", i)
		}
		if entries[i].TableID == entries[i-1].TableID && entries[i].RowID <= entries[i-1].RowID {
			return nil, 0, 0, 0, fmt.Errorf("rowpack: row ids not strictly ascending within table at %d", i)
		}
	}

	// TableID run + 逐表 RowID 分段起点。
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

	// RowID stream：按表内 run 分段，run 首条绝对、其后非负 uvarint 增量。
	rowIDBytes := make([]byte, 0)
	for _, tr := range tableRuns {
		b, e := tr.start, tr.start+tr.length
		rowIDBytes = binary.AppendUvarint(rowIDBytes, entries[b].RowID)
		for k := b + 1; k < e; k++ {
			rowIDBytes = binary.AppendUvarint(rowIDBytes, entries[k].RowID-entries[k-1].RowID)
		}
	}

	// BlockID run：连续相同 BlockID 一段，绝对 blockID + runLen。
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

	// ItemOrdinal：首条绝对，其后 zigzag delta（可负，块边界会重置）。
	ordinalBytes := make([]byte, 0)
	ordinalBytes = binary.AppendUvarint(ordinalBytes, uint64(entries[0].ItemOrdinal))
	for i := 1; i < len(entries); i++ {
		d := int64(entries[i].ItemOrdinal) - int64(entries[i-1].ItemOrdinal)
		ordinalBytes = binary.AppendUvarint(ordinalBytes, zigzag(d))
	}

	// ChangeType 2bit stream。
	changeBits := make([]byte, 0)
	for i := range entries {
		packed, err := fileformat.PackChangeType(entries[i].ChangeType)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		changeBits = appendChangeBit(changeBits, uint32(i), packed)
	}

	// 全局 Min/Max RowID（排序键跨表非单调，显式扫描）。
	minRowID, maxRowID = entries[0].RowID, entries[0].RowID
	for _, r := range entries {
		if r.RowID < minRowID {
			minRowID = r.RowID
		}
		if r.RowID > maxRowID {
			maxRowID = r.RowID
		}
	}

	h := fileformat.RowIndexPageHeader{
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

	// 组装：头 + 流；CRC 覆盖流区。
	page = make([]byte, 0, fileformat.IndexPageHeaderSize+len(tableRunBytes)+len(rowIDBytes)+len(blockRunBytes)+len(ordinalBytes)+len(changeBits))
	page = append(page, make([]byte, fileformat.IndexPageHeaderSize)...)
	page = append(page, tableRunBytes...)
	page = append(page, rowIDBytes...)
	page = append(page, blockRunBytes...)
	page = append(page, ordinalBytes...)
	page = append(page, changeBits...)
	streams := page[fileformat.IndexPageHeaderSize:]
	h.CRC32C = fileformat.CRC32C(streams)
	if err := h.MarshalTo(page); err != nil {
		return nil, 0, 0, 0, err
	}
	return page, len(entries), minRowID, maxRowID, nil
}

// walkRowIndexPage 严格解码一个 Row Index Page，把每条目通过 emit 流式吐出。
// 任何截断/伪造长度/CRC 失败/非法 changeType/排序破坏都返回错误，绝不 panic、绝不做
// 无界分配。输出顺序保证 (TableID, RowID) 升序，与 encodeRowIndexPage 输入一致。
// 与 decodeRowIndexPage 相比，它不物化整页 []RowIndexEntry，也不分配中间列数组，
// 因此 Eager 建 shard 时把条目直接喂给 rowShard 构建器（S3-⑦ 落盘② Open 峰值优化）。
func walkRowIndexPage(raw []byte, emit func(fileformat.RowIndexEntry) error) error {
	n := len(raw)
	if n < fileformat.IndexPageHeaderSize {
		return errIndexPageCorrupt
	}
	var h fileformat.RowIndexPageHeader
	if err := h.Unmarshal(raw, n); err != nil {
		return err
	}
	count := int(h.EntryCount)
	start := fileformat.IndexPageHeaderSize
	if fileformat.CRC32C(raw[start:]) != h.CRC32C {
		return fmt.Errorf("rowpack: index page CRC mismatch")
	}
	wantBits := (uint64(count) + 3) / 4
	if uint64(h.ChangeBitsBytes) != wantBits {
		return fmt.Errorf("rowpack: index page change bits %d, want %d for %d entries", h.ChangeBitsBytes, wantBits, count)
	}

	// 切流（严格边界）。
	off := start
	take := func(byteLen uint32) ([]byte, error) {
		if byteLen > uint32(n-off) {
			return nil, errIndexPageCorrupt
		}
		s := raw[off : off+int(byteLen)]
		off += int(byteLen)
		return s, nil
	}
	tableRun, err := take(h.TableRunBytes)
	if err != nil {
		return err
	}
	rowIDStream, err := take(h.RowIDBytes)
	if err != nil {
		return err
	}
	blockRun, err := take(h.BlockRunBytes)
	if err != nil {
		return err
	}
	ordinalStream, err := take(h.OrdinalBytes)
	if err != nil {
		return err
	}
	changeBits, err := take(h.ChangeBitsBytes)
	if err != nil {
		return err
	}
	if off != n {
		return fmt.Errorf("rowpack: index page has %d trailing bytes", n-off)
	}
	// 安全边界：每条目在 RowID 流至少占 1 字节，故条目数不得超过 RowID 流长度，
	// 防止伪造 EntryCount 触发无界分配。
	if uint64(count) > uint64(len(rowIDStream)) {
		return fmt.Errorf("rowpack: index page entry count %d exceeds row id stream %d", count, len(rowIDStream))
	}

	readVar := func(s []byte, pos *int) (uint64, error) {
		v, num := binary.Uvarint(s[*pos:])
		if num <= 0 {
			return 0, errIndexPageCorrupt
		}
		*pos += num
		return v, nil
	}

	// 五路流在同一条目下标 i 上锁步推进：tableRun/rowIDStream 共享 run 分段，
	// blockRun/ordinalStream/changeBits 各自独立。
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
		// 表 run：边界处读取下一段 (tableID, runLen)。
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
		}
		// RowID：run 首条绝对、其后非负 uvarint 增量。
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
		// BlockID：run 边界处读取下一段 (blockID, runLen)。
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
		}
		curBlockLeft--
		// ItemOrdinal：首条绝对、其后 zigzag delta。
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
		ct, err := fileformat.UnpackChangeType(packed)
		if err != nil {
			return fmt.Errorf("rowpack: index page entry %d: %w", ti, err)
		}
		// 排序一致性。
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
		// 全局 Min/Max RowID（跨表非单调）。
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
		if err := emit(fileformat.RowIndexEntry{
			TableID:     currentTable,
			RowID:       rowID,
			BlockID:     curBlock,
			ItemOrdinal: ordinal,
			ChangeType:  ct,
		}); err != nil {
			return err
		}
		tableRunLeft--
		ti++
	}
	// 各流必须恰好耗尽（防伪造长度）。
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
	// 交叉校验 header 导出的锚点值与解码结果一致（防单 bit 翻转构造伪页）。
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

// decodeRowIndexPage 严格解码一个 Row Index Page，物化整页 []RowIndexEntry。
// 大多数生产路径应当使用 walkRowIndexPage 流式吐出；本函数保留给需要整页切片的
// 调用方（单元测试 / 非流式读取），并在结尾做全部 header 锚点交叉校验。
func decodeRowIndexPage(raw []byte) ([]fileformat.RowIndexEntry, error) {
	count := 0
	if len(raw) >= fileformat.IndexPageHeaderSize {
		var h fileformat.RowIndexPageHeader
		// 仅在大致可读时用 header 预分配；任何解析错误交给 walker 报告。
		if err := h.Unmarshal(raw, len(raw)); err == nil {
			count = int(h.EntryCount)
		}
	}
	out := make([]fileformat.RowIndexEntry, 0, count)
	if err := walkRowIndexPage(raw, func(e fileformat.RowIndexEntry) error {
		out = append(out, e)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// fenceForRowIndexPage 从原始（未压缩）页字节构造 RowIndexFenceEntry（§7.2）。
// storedSize 是压缩/密封后的大小；storedOffset 是页在 txn 正文内的偏移；
// snapshotID 是该 txn 的 SnapshotID。多表页取首表的 TableID（单表页占绝大多数）。
// PageCRC32C 与页内 CRC 一致（流区 CRC）。
func fenceForRowIndexPage(raw []byte, storedSize uint32, snapshotID uint64, storedOffset uint64) (fileformat.RowIndexFenceEntry, error) {
	if len(raw) < fileformat.IndexPageHeaderSize {
		return fileformat.RowIndexFenceEntry{}, errIndexPageCorrupt
	}
	var h fileformat.RowIndexPageHeader
	if err := h.Unmarshal(raw, len(raw)); err != nil {
		return fileformat.RowIndexFenceEntry{}, err
	}
	tid, ok := firstTableIDOfPage(raw)
	if !ok {
		return fileformat.RowIndexFenceEntry{}, errIndexPageCorrupt
	}
	return fileformat.RowIndexFenceEntry{
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

// firstTableIDOfPage 读取页 table run 的首个 tableID（用于 Fence）。
func firstTableIDOfPage(raw []byte) (uint32, bool) {
	if len(raw) < fileformat.IndexPageHeaderSize {
		return 0, false
	}
	t, n := binary.Uvarint(raw[fileformat.IndexPageHeaderSize:])
	if n <= 0 || t > maxUint32 {
		return 0, false
	}
	return uint32(t), true
}
