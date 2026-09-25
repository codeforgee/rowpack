package rowpack

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/index"
)

// read_batch_page_arms_test.go 覆盖批量读在页内失败的三条臂:页头自相矛盾时整页取不
// 出来、记录的 change type 位是个非法值、以及页目录多报了一条记录(序号指向页里不
// 存在的记录)。三者都发生在页内容层面,块级 CRC 由补丁器重算,因此只能由页解码器自
// 己发现。
//
// 剩下的 collectChunk「同一页里出现两个 schema 版本」臂是构造上不可达的:一个块只
// 属于一次提交的一张表,同页记录必然同版本;同一事务内用不同的列重定义表会被
// ErrSchemaConflict 挡住。batchBuffer 的 Lock/Unlock 是给 go vet 看的空实现,永远
// 不会被调用。

// TestReadBatchRejectsUndecodablePage: the page header claims no record at all.
// The block and the page both pass their CRCs, so the failure belongs to the
// page decoder: the run cannot be read from a page that cannot be parsed.
func TestReadBatchRejectsUndecodablePage(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		p := pb.page(0)
		require.NotZero(t, binary.LittleEndian.Uint32(p[12:]), "page 0 carries records")
		binary.LittleEndian.PutUint32(p[12:], 0) // RowsPageHeader.EntryCount
	})

	_, _, _, batchErr, _, _ := readAllEntries(t, base)
	requireCorruption(t, "ReadBatch of a page whose header contradicts itself", batchErr)
}

// TestReadBatchRejectsIllegalChangeType: the 2-bit packed change type of a
// record is 3, which no writer produces. The record cannot be interpreted, so
// the batch read fails on it instead of decoding a wrong change type.
func TestReadBatchRejectsIllegalChangeType(t *testing.T) {
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		rh := pageHeaderOf(t, pb, 0)
		streams := pb.pageStreams(0)
		cbStart := int(rh.RowIDsBytes) + int(rh.OffsetsBytes) + int(rh.SchemaRLEBytes)
		require.Less(t, cbStart, len(streams))
		streams[cbStart] |= 3 // record 0: Insert(00) -> 11, an illegal packed value
		require.Equal(t, byte(3), streams[cbStart]&3)
	})

	_, _, _, batchErr, _, _ := readAllEntries(t, base)
	requireCorruption(t, "ReadBatch of a record with an illegal packed change type", batchErr)
	require.ErrorContains(t, batchErr, "change type")
}

// TestReadBatchRejectsPhantomOrdinal: the page directory promises one record
// more than the page carries. The directory stays self-consistent (contiguous
// first ordinals, matching total), so the block loads; only when the batch asks
// for that ordinal does the page refuse it.
func TestReadBatchRejectsPhantomOrdinal(t *testing.T) {
	ctx := context.Background()
	base, snap := setupPlainMultiPageStore(t, 60)

	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	st, err := db.captureState()
	require.NoError(t, err)
	tid, ok := st.schemas.tableID(uint64(snap), "t")
	require.True(t, ok)
	loc, ok := st.view.ResolveRow(uint64(snap), uint32(tid), 1)
	require.True(t, ok)
	require.NoError(t, db.Close())

	var (
		real    uint32 // an ordinal the last page really carries
		phantom uint32 // the ordinal the directory promises but the page lacks
	)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		last := len(pb.dir) - 1
		require.Greater(t, last, 0, "the fixture block spans several pages")
		real = pb.dir[last].FirstRecordOrdinal
		phantom = pb.dir[last].FirstRecordOrdinal + pb.dir[last].RecordCount
		pb.dir[last].RecordCount++
		pb.rh.TotalRecords++
		pb.hdr.ItemCount++ // the block header must agree with the container
		require.NoError(t, pb.rh.MarshalTo(pb.container[:format.RowsBlockHeaderSize]))
	})

	db2, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	publishCraftedView(t, db2, snap+1, uint64(snap), func(b *index.Builder) {
		// Two requests inside the same page: the first one decodes, the second
		// asks for the record the directory invented.
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: uint64(snap + 1), TableID: uint32(tid), RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    loc.BlockID, ItemOrdinal: real,
		}))
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: uint64(snap + 1), TableID: uint32(tid), RowID: 2,
			ChangeType: format.ChangeInsert,
			BlockID:    loc.BlockID, ItemOrdinal: phantom,
		}))
	})

	_, err = db2.ReadBatch(ctx, snap+1, "t", []RowID{1, 2})
	requireCorruption(t, "ReadBatch of an ordinal the page does not carry", err)
}
