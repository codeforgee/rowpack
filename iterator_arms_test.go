package rowpack

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
)

// iterator_arms_test.go 覆盖迭代器的四条臂:扫描结束后再 Next、字节值大于 arena 的
// chunk 时 chunk 必须按值放大、以及索引指到视图里没有的块/没有的序号——后两条只能
// 白盒构造。
//
// ScanBlocks 分支里「blockIDs 里的块在视图里找不到」那条臂是构造上不可达的:
// blockIDs 来自 view.Blocks(),而 view.Block(id) 读的是同一张表。

// TestIteratorStopsAtTheEnd: a closed iterator keeps answering (nil, false)
// instead of restarting or panicking.
func TestIteratorStopsAtTheEnd(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	it, err := db.Scan(ctx, 1, "t", ScanOptions{})
	require.NoError(t, err)
	row, ok := it.Next()
	require.True(t, ok)
	require.NotNil(t, row)
	require.NoError(t, it.Close())

	row, ok = it.Next()
	require.False(t, ok, "a closed iterator stays at the end")
	require.Nil(t, row)
	require.NoError(t, it.Err())
}

// TestIteratorStopsAfterFailure: once the iterator has failed it stays failed,
// so a caller that keeps pulling sees the same end and the same error.
func TestIteratorStopsAfterFailure(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	publishCraftedView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 2, TableID: 1, RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    123456, ItemOrdinal: 0, // never committed
		}))
	})

	it, err := db.Scan(ctx, 2, "t", ScanOptions{})
	require.NoError(t, err)
	_, ok := it.Next()
	require.False(t, ok, "the row cannot be located, so the scan ends")
	require.ErrorContains(t, it.Err(), "missing from view")

	_, ok = it.Next()
	require.False(t, ok, "a failed iterator does not resume")
	require.ErrorContains(t, it.Err(), "missing from view")
}

// TestIteratorRejectsOrdinalBeyondPages: the index points past the block's
// pages. The scan reports corruption of this snapshot's table rather than
// decoding some other record.
func TestIteratorRejectsOrdinalBeyondPages(t *testing.T) {
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

	it, err := db.Scan(ctx, 2, "t", ScanOptions{})
	require.NoError(t, err)
	_, ok = it.Next()
	require.False(t, ok)
	requireCorruption(t, "Scan of an ordinal the block does not carry", it.Err())
}

// TestIteratorArenaGrowsForLargeBytes: a bytes value wider than one arena chunk
// (32 KiB) cannot share the current chunk, so the arena allocates a chunk that
// fits the value instead of truncating it.
func TestIteratorArenaGrowsForLargeBytes(t *testing.T) {
	ctx := context.Background()
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "blob", Type: TypeBytes}}))

	big := bytes.Repeat([]byte{0xAB}, 40<<10) // > iterArenaChunkSize
	require.NoError(t, tx.Insert("t", 1, Row{Bytes(big)}))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	it, err := db.Scan(ctx, snap, "t", ScanOptions{})
	require.NoError(t, err)
	// The iterator holds the store's read lock: without Close the store's Close
	// waits for the leaked iterator's GC finalizer.
	defer func() { require.NoError(t, it.Close()) }()
	row, ok := it.Next()
	require.True(t, ok)
	require.NoError(t, it.Err())
	got, ok := row[0].Bytes()
	require.True(t, ok)
	require.Equal(t, len(big), len(got), "the whole blob survives the arena chunk rotation")
	require.True(t, bytes.Equal(big, got))
}
