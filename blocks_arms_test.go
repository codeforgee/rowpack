package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
)

// blocks_arms_test.go 覆盖目录入口(blocks.go)的拒绝臂:已关闭、快照/表不存在、块
// 区间为空,以及「块存在但没有一条行落在索引里」——后者必须报空区间,而不是把
// MinRowID 留在 ^0 上让调用方算出一个天文数字的区间。
//
// ScanBlocks 里 blocksByTable 的错误臂是构造上不可达的:该函数只做索引内的遍历,
// 任何情况下都返回 nil 错误。

// TestBlocksReportClosed: the block catalogue is a read path like any other.
func TestBlocksReportClosed(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	require.NoError(t, db.Close())

	_, err := db.Blocks(1, "t")
	require.ErrorIs(t, err, ErrClosed)

	_, err = db.ScanBlocks(ctx, 1, "t", 0, 1)
	require.ErrorIs(t, err, ErrClosed)
}

// TestBlocksRejectUnknownAddress: an unknown snapshot or an unknown table is
// ErrNotFound on both block entries.
func TestBlocksRejectUnknownAddress(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	// Blocks 的「未知快照 / 未知表」两臂由 read_gaps_test.go
	// TestReadLookupNotFoundArms 覆盖（同一 ErrNotFound 判定），此处只补 ScanBlocks。
	_, err := db.ScanBlocks(ctx, 4242, "t", 0, 1)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = db.ScanBlocks(ctx, 1, "nope", 0, 1)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestScanBlocksRejectsEmptyRange: an empty block range is a caller mistake,
// not an empty result.
func TestScanBlocksRejectsEmptyRange(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	_, err := db.ScanBlocks(ctx, 1, "t", 3, 3)
	require.ErrorIs(t, err, ErrInvalidArgument)

	_, err = db.ScanBlocks(ctx, 1, "t", 9, 2)
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// TestBlocksEmptyRangeForUnindexedBlock: a rows block of the snapshot that no
// indexed row points at keeps the empty range [0,0): MinRowID must not stay at
// its ^0 sentinel.
//
// Reached white-box: the writer never commits a rows block without its rows.
func TestBlocksEmptyRangeForUnindexedBlock(t *testing.T) {
	db := armCommittedStore(t)
	publishCraftedView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddBlock(format.BlockIndexEntry{
			BlockID:     123456,
			SnapshotID:  2,
			TableID:     1,
			BlockKind:   format.BlockKindRows,
			Compression: format.CompressionNone,
			DataOffset:  1 << 20,
			RawSize:     64,
			StoredSize:  64,
			ItemCount:   0, // no row entry points here
		}))
	})

	blocks, err := db.Blocks(2, "t")
	require.NoError(t, err)
	require.Len(t, blocks, 1)
	require.Equal(t, uint64(123456), blocks[0].BlockID)
	require.Zero(t, blocks[0].MinRowID, "an unindexed block reports the empty range")
	require.Zero(t, blocks[0].MaxRowID)
}
