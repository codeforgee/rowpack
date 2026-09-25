package rowpack

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/metadata"
	"github.com/stretchr/testify/require"
)

// 这一组测试针对「CRC 链条自洽的页流损坏」：改完页内字节后重算页 CRC、目录项
// PageCRC32C、块头 RawCRC32C 与块头自身 CRC，使文件的完整性外壳完全合法。
// 只有这样才能穿透块/页层的 CRC 拦截，把损坏送到行解码与迭代器的错误分支上
// （RecordAt / decoderFor / DecodeInto / 目录几何不一致导致的装载失败），
// 验证「不 panic + 错误可被 errors.Is 分类（REQUIREMENTS §12）+ 资源不泄漏」。

// patchableBlock 是磁盘上一个明文 rows 块（CompressionNone、未加密）的可改写视图。
type patchableBlock struct {
	file      *os.File
	off       int64
	hdr       format.BlockHeader
	container []byte // [RowsBlockHeader][dir × N][page 0][page 1]…
	rh        format.RowsBlockHeader
	dir       []format.RowsPageDirEntry
	dirEnd    int
}

// loadPatchableRowsBlock 读取 blkOff 处的 rows 块（必须是不压缩、不加密的明文块）。
func loadPatchableRowsBlock(t *testing.T, f *os.File, blkOff int64) *patchableBlock {
	t.Helper()
	pb := &patchableBlock{file: f, off: blkOff}
	var hb [format.BlockHeaderSize]byte
	_, err := f.ReadAt(hb[:], blkOff)
	require.NoError(t, err)
	require.NoError(t, pb.hdr.Unmarshal(hb[:]))
	require.Equal(t, format.BlockKindRows, pb.hdr.BlockKind)
	require.False(t, pb.hdr.Encrypted)
	require.Equal(t, format.CompressionNone, pb.hdr.Compression)
	pb.container = make([]byte, pb.hdr.StoredSize)
	_, err = f.ReadAt(pb.container, blkOff+format.BlockHeaderSize)
	require.NoError(t, err)
	require.NoError(t, pb.rh.Unmarshal(pb.container[:format.RowsBlockHeaderSize]))
	pb.dirEnd = format.RowsBlockHeaderSize + int(pb.rh.DirectoryBytes)
	pb.dir = make([]format.RowsPageDirEntry, pb.rh.PageCount)
	for i := range pb.dir {
		pos := format.RowsBlockHeaderSize + i*format.RowsPageDirEntrySize
		require.NoError(t, pb.dir[i].Unmarshal(pb.container[pos:]))
	}
	return pb
}

// page 返回第 i 页的 stored 字节（CompressionNone 下即 raw 页）。
func (pb *patchableBlock) page(i int) []byte {
	e := &pb.dir[i]
	return pb.container[int(e.StoredOffset) : int(e.StoredOffset)+int(e.StoredSize)]
}

// pageStreams 返回第 i 页的流区（页头之后），即页 CRC 的覆盖范围。
func (pb *patchableBlock) pageStreams(i int) []byte {
	p := pb.page(i)
	return p[format.RowsPageHeaderSize:]
}

// write 重算整条 CRC 链并落盘：页头 CRC ← 页流 CRC；目录 PageCRC32C ← 页头 CRC；
// 块头 RawCRC32C ← 容器头+目录区 CRC；块头 CRC 由 MarshalTo 重算。
func (pb *patchableBlock) write(t *testing.T) {
	t.Helper()
	for i := range pb.dir {
		p := pb.page(i)
		require.Greater(t, len(p), format.RowsPageHeaderSize)
		crc := format.CRC32C(p[format.RowsPageHeaderSize:])
		binary.LittleEndian.PutUint32(p[60:], crc) // RowsPageHeader.CRC32C
		pb.dir[i].PageCRC32C = crc
		pos := format.RowsBlockHeaderSize + i*format.RowsPageDirEntrySize
		require.NoError(t, pb.dir[i].MarshalTo(pb.container[pos:]))
	}
	pb.hdr.RawCRC32C = format.CRC32C(pb.container[:pb.dirEnd])
	var hb [format.BlockHeaderSize]byte
	require.NoError(t, pb.hdr.MarshalTo(hb[:]))
	_, err := pb.file.WriteAt(hb[:], pb.off)
	require.NoError(t, err)
	_, err = pb.file.WriteAt(pb.container, pb.off+format.BlockHeaderSize)
	require.NoError(t, err)
}

// pageHeaderOf 解析第 i 页的页头（各流的长度字段用于定位流区）。
func pageHeaderOf(t *testing.T, pb *patchableBlock, i int) format.RowsPageHeader {
	t.Helper()
	p := pb.page(i)
	var h format.RowsPageHeader
	require.NoError(t, h.Unmarshal(p[:format.RowsPageHeaderSize], len(p)))
	return h
}

// blockHoldingRow 返回快照 snap 上表 table 中 rowID 所在 rows 块的文件偏移，
// 以及该块是否跨多页。
func blockHoldingRow(t *testing.T, db *Store, snap SnapshotID, table string, rowID RowID) (int64, int) {
	t.Helper()
	st, err := db.captureState()
	require.NoError(t, err)
	tid, ok := st.schemas.tableID(uint64(snap), table)
	require.True(t, ok)
	loc, ok := st.view.ResolveRow(uint64(snap), uint32(tid), uint64(rowID))
	require.True(t, ok, "row %d must resolve", rowID)
	bl := st.view.Block(loc.BlockID)
	require.NotNil(t, bl)
	require.Equal(t, format.BlockKindRows, bl.Kind)
	return int64(bl.DataOffset), len(st.view.Blocks())
}

// multiPageBlockOffset 返回第一个（按文件偏移排序）跨多页的 rows 块偏移。
func multiPageBlockOffset(t *testing.T, db *Store, snap SnapshotID, table string) int64 {
	t.Helper()
	st, err := db.captureState()
	require.NoError(t, err)
	tid, ok := st.schemas.tableID(uint64(snap), table)
	require.True(t, ok)
	var offs []int64
	for _, bl := range st.view.Blocks() {
		if bl.SnapshotID == uint64(snap) && bl.TableID == uint32(tid) && bl.Kind == format.BlockKindRows && bl.ItemCount > 4 {
			offs = append(offs, int64(bl.DataOffset))
		}
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	require.NotEmpty(t, offs, "snapshot must own multi-record rows blocks")
	return offs[0]
}

// setupPlainMultiPageStore 建一个 CompressionNone、多页多块的 store，返回 base 与快照。
func setupPlainMultiPageStore(t *testing.T, rows int) (string, SnapshotID) {
	t.Helper()
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{BlockSize: 2048, PageSize: 128, Compression: CompressionNone})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "name", Type: TypeString},
		{Name: "n", Type: TypeUint64},
	}))
	for i := 0; i < rows; i++ {
		require.NoError(t, tx.Insert("t", RowID(i+1), Row{
			String("name-" + filepath.Base(t.Name()) + "-padded-to-fill-a-page"),
			Uint64(uint64(i + 1)),
		}))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	// 每页 2 条记录、每块 24 条记录：损坏只落在 page 0 / 首个块上，因此块越多，
	// 「损坏块被跳过」与「被读到」越容易区分。
	st, err := db.captureState()
	require.NoError(t, err)
	blocks := 0
	for _, bl := range st.view.Blocks() {
		if bl.Kind == format.BlockKindRows && bl.ItemCount > 4 {
			blocks++
		}
	}
	require.GreaterOrEqual(t, blocks, 2, "need several multi-record blocks so a skipped corrupt block is observable")
	require.NoError(t, db.Close())
	return base, snap
}

// patchFirstBlock 以 CRC 自洽的方式改写快照 1 表 t 中 row 1 所在块的 page 0，
// mutate 负责改页内字节，返回是否成功落盘。
func patchFirstBlock(t *testing.T, base string, mutate func(t *testing.T, pb *patchableBlock)) {
	t.Helper()
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	off, _ := blockHoldingRow(t, db, 1, "t", 1)
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	pb := loadPatchableRowsBlock(t, f, off)
	mutate(t, pb)
	pb.write(t)
}

// readAllEntries 把四类读入口都跑一遍；返回各自的 error（迭代器错误取 Err()），
// 以及成功读到的行数。Close 必须是 no-op 失败即视为读锁泄漏（require 在 cleanup 中）。
func readAllEntries(t *testing.T, base string) (rows int, getErr, scanErr, batchErr, blocksErr, verifyErr error) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close(), "Close must not hang: failing iterators release readMu")
	})

	_, getErr = db.Get(ctx, 1, "t", 1, nil)
	_, batchErr = db.ReadBatch(ctx, 1, "t", []RowID{1, 2, 3, 4})

	collect := func(it *Iterator, err error) (int, error) {
		if err != nil {
			return 0, err
		}
		defer it.Close()
		n := 0
		for {
			if _, ok := it.Next(); !ok {
				break
			}
			n++
			require.Less(t, n, 100000, "runaway iteration")
		}
		return n, it.Err()
	}
	sit, err := db.Scan(ctx, 1, "t", ScanOptions{})
	rows, scanErr = collect(sit, err)

	blks, err := db.Blocks(ctx, 1, "t")
	require.NoError(t, err)
	require.NotEmpty(t, blks)
	bit, err := db.ScanBlocks(ctx, 1, "t", blks[0].BlockID, blks[len(blks)-1].BlockID+1)
	_, blocksErr = collect(bit, err)

	_, verifyErr = db.Verify(ctx, VerifyFull)
	return
}

// requireCorruption 断言 err 是结构化的、可按 ErrCorruptData 匹配的完整性失败。
func requireCorruption(t *testing.T, what string, err error) {
	t.Helper()
	require.Error(t, err, "%s must fail", what)
	require.ErrorIs(t, err, ErrCorruptData, "%s: corrupt row data must classify via errors.Is (§12)", what)
	var cerr *CorruptionError
	require.True(t, errors.As(err, &cerr), "%s: must be a structured CorruptionError, got %T: %v", what, err, err)
	require.Equal(t, ErrCorruptData, cerr.Kind, "%s", what)
}

// TestCorruptPageRewriteIsLossless 是补丁器自身的回归：只做「重算 CRC 链」的原样
// 重写，块必须仍然完整可读（四类入口全部成功、行数不变）。
func TestCorruptPageRewriteIsLossless(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {})
	rows, getErr, scanErr, batchErr, blocksErr, verifyErr := readAllEntries(t, base)
	require.NoError(t, getErr)
	require.NoError(t, batchErr)
	require.NoError(t, blocksErr)
	require.NoError(t, verifyErr)
	require.NoError(t, scanErr)
	require.EqualValues(t, 60, rows, "every row must still be readable")
}

// TestCorruptSchemaVersionInPage: page 0 的 schema RLE 首 run 版本改成未知版本
// （CRC 链重算后自洽）。四类读入口与 Verify 都必须报结构化损坏，同时保留
// ErrSchemaMismatch 的 Cause 链。
func TestCorruptSchemaVersionInPage(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		rh := pageHeaderOf(t, pb, 0)
		streams := pb.pageStreams(0)
		rleStart := int(rh.RowIDsBytes) + int(rh.OffsetsBytes)
		require.Less(t, rleStart, len(streams))
		require.Equal(t, byte(1), streams[rleStart], "first run must be schema version 1")
		streams[rleStart] = 99 // 未知版本，仍是单字节 uvarint，流长度不变
	})

	rows, getErr, scanErr, batchErr, blocksErr, verifyErr := readAllEntries(t, base)
	require.ErrorIs(t, getErr, ErrSchemaMismatch, "the underlying cause stays on the chain")
	requireCorruption(t, "Get", getErr)
	requireCorruption(t, "Scan", scanErr)
	requireCorruption(t, "ReadBatch", batchErr)
	requireCorruption(t, "ScanBlocks", blocksErr)
	requireCorruption(t, "Verify", verifyErr)
	require.Less(t, rows, 60, "a scan that survives the corrupt block silently drops rows")
}

// TestCorruptNullBitmapInPage: tuples 区首字节（page 0 record 0 的 null bitmap）
// 改成 0xFF，未使用的高位必须被解码器拒绝并归类为数据损坏。
func TestCorruptNullBitmapInPage(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		rh := pageHeaderOf(t, pb, 0)
		streams := pb.pageStreams(0)
		tuplesStart := int(rh.RowIDsBytes) + int(rh.OffsetsBytes) + int(rh.SchemaRLEBytes) + int(rh.ChangeBitsBytes)
		require.Less(t, tuplesStart, len(streams))
		require.Equal(t, byte(0), streams[tuplesStart], "record 0 has no NULL")
		streams[tuplesStart] = 0xFF
	})

	rows, getErr, scanErr, batchErr, blocksErr, verifyErr := readAllEntries(t, base)
	requireCorruption(t, "Get", getErr)
	requireCorruption(t, "Scan", scanErr)
	requireCorruption(t, "ReadBatch", batchErr)
	requireCorruption(t, "ScanBlocks", blocksErr)
	requireCorruption(t, "Verify", verifyErr)
	require.Less(t, rows, 60)
}

// TestCorruptDeleteRecordCarriesBody: 把一个带 body 的 INSERT 记录的 change bit
// 改成 DELETE（packed 2 合法，只有 3 是损坏标记）。页能过 CRC，但 RecordAt 必须
// 报「delete record carries a body」并归类为数据损坏。
func TestCorruptDeleteRecordCarriesBody(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		rh := pageHeaderOf(t, pb, 0)
		streams := pb.pageStreams(0)
		cbStart := int(rh.RowIDsBytes) + int(rh.OffsetsBytes) + int(rh.SchemaRLEBytes)
		require.Less(t, cbStart, len(streams))
		streams[cbStart] |= 2 // record 0: Insert(00) → Delete(10)
	})

	_, getErr, scanErr, batchErr, blocksErr, verifyErr := readAllEntries(t, base)
	requireCorruption(t, "Get", getErr)
	requireCorruption(t, "Scan", scanErr)
	requireCorruption(t, "ReadBatch", batchErr)
	requireCorruption(t, "ScanBlocks", blocksErr)
	requireCorruption(t, "Verify", verifyErr)
}

// TestCorruptPageDirGeometryFailsLoad: 多页块的目录项 RecordCount 与下一页的
// FirstRecordOrdinal 不一致（容器头/目录区 CRC 重算后自洽）。块的装载必须失败，
// 四类读入口返回分类错误而不是崩溃。
func TestCorruptPageDirGeometryFailsLoad(t *testing.T) {
	base, snap := setupPlainMultiPageStore(t, 400)
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	off := multiPageBlockOffset(t, db, snap, "t")
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	pb := loadPatchableRowsBlock(t, f, off)
	require.GreaterOrEqual(t, len(pb.dir), 2, "block must span pages")
	pb.dir[0].RecordCount++ // page 1 的 FirstRecordOrdinal 不再衔接
	pb.write(t)
	require.NoError(t, f.Close())

	_, getErr, scanErr, batchErr, blocksErr, verifyErr := readAllEntries(t, base)
	// 按文件偏移排序的首个多页块就是承载 row 1 的块，四类入口全部经过它。
	requireCorruption(t, "Get", getErr)
	requireCorruption(t, "ReadBatch", batchErr)
	requireCorruption(t, "Scan", scanErr)
	requireCorruption(t, "ScanBlocks", blocksErr)
	requireCorruption(t, "Verify", verifyErr)
}

// metaBlock 是磁盘上一个不压缩、不加密 Metadata 块的可改写视图。
type metaBlock struct {
	file    *os.File
	off     int64
	hdr     format.BlockHeader
	payload []byte // [PayloadHeader][dir × N][record bytes]
}

func loadPatchableMetaBlock(t *testing.T, f *os.File, blkOff int64) *metaBlock {
	t.Helper()
	mb := &metaBlock{file: f, off: blkOff}
	var hb [format.BlockHeaderSize]byte
	_, err := f.ReadAt(hb[:], blkOff)
	require.NoError(t, err)
	require.NoError(t, mb.hdr.Unmarshal(hb[:]))
	require.Equal(t, format.BlockKindMetadata, mb.hdr.BlockKind)
	require.False(t, mb.hdr.Encrypted)
	require.Equal(t, format.CompressionNone, mb.hdr.Compression)
	mb.payload = make([]byte, mb.hdr.StoredSize)
	_, err = f.ReadAt(mb.payload, blkOff+format.BlockHeaderSize)
	require.NoError(t, err)
	return mb
}

// write 重算块级 CRC 链并落盘（Record/目录项 CRC 由调用方自行维护）。
func (mb *metaBlock) write(t *testing.T) {
	t.Helper()
	mb.hdr.RawCRC32C = format.CRC32C(mb.payload)
	var hb [format.BlockHeaderSize]byte
	require.NoError(t, mb.hdr.MarshalTo(hb[:]))
	_, err := mb.file.WriteAt(hb[:], mb.off)
	require.NoError(t, err)
	_, err = mb.file.WriteAt(mb.payload, mb.off+format.BlockHeaderSize)
	require.NoError(t, err)
}

// firstMetadataBlock 返回快照 snap 之后 view 中第一个（按文件偏移）Metadata 块。
func firstMetadataBlock(t *testing.T, db *Store) int64 {
	t.Helper()
	st, err := db.captureState()
	require.NoError(t, err)
	var offs []int64
	for _, bl := range st.view.Blocks() {
		if bl.Kind == format.BlockKindMetadata {
			offs = append(offs, int64(bl.DataOffset))
		}
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	require.NotEmpty(t, offs, "a store with tables owns metadata blocks")
	return offs[0]
}

// TestCorruptMetadataRecordIsClassified: 改一条目录记录的 fieldsLen，同时重算记录
// 内层 CRC、目录项 recordCRC 与块头 CRC——文件的 CRC 外壳完全合法，只有 TLV 自述
// 长度不一致。目录是打开时构建 schema 索引的必读对象，因此 Open 必须以分类后的
// 结构化损坏错误失败（REQUIREMENTS §12：不得靠字符串判断类别）。
func TestCorruptMetadataRecordIsClassified(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	moff := firstMetadataBlock(t, db)
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	mb := loadPatchableMetaBlock(t, f, moff)
	var ph metadata.PayloadHeader
	require.NoError(t, ph.Unmarshal(mb.payload[:metadata.PayloadHeaderSize]))
	require.Greater(t, ph.ItemCount, uint32(0))
	dirBase := metadata.PayloadHeaderSize
	recBase := dirBase + int(ph.DirectoryBytes)
	e0 := metadata.DirectoryEntry{}
	require.NoError(t, e0.Unmarshal(mb.payload[dirBase:dirBase+metadata.DirectoryEntrySize]))
	require.NotEqual(t, format.OperationDelete, e0.Operation)
	body := mb.payload[recBase+int(e0.RecordOffset) : recBase+int(e0.RecordOffset)+int(e0.RecordLength)]
	require.Greater(t, len(body), metadata.RecordEnvelopeHeaderSize+8)
	// fieldsLen（头里最后一个 u32）+1：长度声明与记录实际字段区不符。
	fl := binary.LittleEndian.Uint32(body[44:])
	binary.LittleEndian.PutUint32(body[44:], fl+1)
	// 重算记录内层 CRC（覆盖 body[:len-4]）与目录项 recordCRC（覆盖整个 body）。
	binary.LittleEndian.PutUint32(body[len(body)-4:], format.CRC32C(body[:len(body)-4]))
	e0.SetRecordCRC(format.CRC32C(body))
	require.NoError(t, e0.MarshalTo(mb.payload[dirBase:dirBase+metadata.DirectoryEntrySize]))
	mb.write(t)
	require.NoError(t, f.Close())

	_, err = Open(base, Options{Compression: CompressionNone})
	requireCorruption(t, "Open with a corrupt catalog record", err)
	var cerr *CorruptionError
	require.True(t, errors.As(err, &cerr))
	require.Contains(t, cerr.Error(), "fields length", "the underlying TLV diagnosis stays in the message")
	require.Equal(t, uint64(1), uint64(cerr.SnapshotID), "the failing record is attributed to its snapshot")
	require.NotZero(t, cerr.BlockID, "the failing record is attributed to its block")
	require.NotZero(t, cerr.Offset)
}
