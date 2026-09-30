package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
)

// read_batch_arms_test.go 覆盖批量读的两个「索引说这里有一行」拒绝臂:行指向视图里
// 没有的块、以及指向块里不存在的记录序号。写入器总是把索引和块一起提交,所以这些
// 视图只能白盒拼出来。

// TestReadBatchReportsClosed: like every read path, the batch read answers
// ErrClosed once the store is closed.
func TestReadBatchReportsClosed(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	require.NoError(t, db.Close())

	_, err := db.ReadBatch(ctx, 1, "t", []RowID{1})
	require.ErrorIs(t, err, ErrClosed)
}

// TestReadBatchRejectsUnknownSnapshot: like Get, the batch read answers
// ErrNotFound for a snapshot that was never committed.
func TestReadBatchRejectsUnknownSnapshot(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	_, err := db.ReadBatch(ctx, 4242, "t", []RowID{1})
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "snapshot 4242")
}

// TestReadBatchRejectsMissingBlock: the row index names a block the view has
// never seen. The batch cannot even open the block, so it fails loudly.
func TestReadBatchRejectsMissingBlock(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	publishCraftedView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 2, TableID: 1, RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    123456, ItemOrdinal: 0, // never committed
		}))
	})

	_, err := db.ReadBatch(ctx, 2, "t", []RowID{1})
	require.ErrorContains(t, err, "missing from view")
}

// TestReadBatchRejectsOrdinalBeyondPages: the first request of a block run
// points past the block's pages, so the run cannot be located.
func TestReadBatchRejectsOrdinalBeyondPages(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	st, err := db.captureState()
	require.NoError(t, err)
	loc, ok := st.view.ResolveRow(1, 1, 1)
	require.True(t, ok)

	publishCraftedView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 2, TableID: 1, RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    loc.BlockID, ItemOrdinal: 1 << 24, // real block, no such page
		}))
	})

	_, err = db.ReadBatch(ctx, 2, "t", []RowID{1})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound, "a row that cannot be located is not an absent row")
}

// TestReadBatchRejectsNeighbourOrdinal: the run's first request is fine and a
// later one in the same block points past the pages. The batch must stop where
// the block contradicts the index instead of reading a wrong record.
func TestReadBatchRejectsNeighbourOrdinal(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	st, err := db.captureState()
	require.NoError(t, err)
	loc, ok := st.view.ResolveRow(1, 1, 1)
	require.True(t, ok)

	publishCraftedView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 2, TableID: 1, RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    loc.BlockID, ItemOrdinal: loc.ItemOrdinal,
		}))
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 2, TableID: 1, RowID: 2,
			ChangeType: format.ChangeInsert,
			BlockID:    loc.BlockID, ItemOrdinal: 1 << 24, // same block, no such page
		}))
	})

	_, err = db.ReadBatch(ctx, 2, "t", []RowID{1, 2})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrNotFound)
}

// TestReadBatchSpansPages: one block is loaded once and its requests are split
// into per-page runs, each page decompressed once. The batch must come back in
// the order of ids whatever the physical page layout is.
func TestReadBatchSpansPages(t *testing.T) {
	ctx := context.Background()
	base, snap := setupPlainMultiPageStore(t, 60)
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	ids := make([]RowID, 60)
	for i := range ids {
		ids[i] = RowID(i + 1)
	}
	rows, err := db.ReadBatch(ctx, snap, "t", ids)
	require.NoError(t, err)
	require.Len(t, rows, len(ids))
	for i, row := range rows {
		got, ok := row[1].Uint64()
		require.True(t, ok)
		require.Equal(t, uint64(i+1), got, "row %d reads back in id order", i+1)
	}
}
