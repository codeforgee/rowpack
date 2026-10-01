package rowpack

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/metadata"
)

// read_arms_test.go 覆盖读路径的「读不出来」臂:store 已关闭时每条读路径都必须回答
// ErrClosed(而不是去解引用已不存在的 publishedState)、快照不存在时的 ErrNotFound
// (TablesIn 要原样传出 Tables 的错),以及索引与块互相矛盾时读路径的拒绝臂。后一类
// 只能白盒构造:写入器总是把索引和块一起提交,不会造出互相矛盾的视图。
//
// 剩下两条是构造上不可达的:captureState 的 state==nil 臂被 checkOpen 挡在前面
// (Close 先置 closed),tableRecord 走完整条父链仍找不到对象则需要索引里存在
// OperationDelete 的元数据项,而写入路径只产生 OperationUpsert。

// TestReadPathsReportClosed: Close leaves no published state behind, so every
// read path is one captureState away from a nil dereference. All of them must
// answer ErrClosed.
func TestReadPathsReportClosed(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	require.NoError(t, db.Close())

	_, err := db.ListSnapshots(ctx)
	require.ErrorIs(t, err, ErrClosed)

	_, err = db.Get(ctx, 1, "t", 1, nil)
	require.ErrorIs(t, err, ErrClosed)

	_, err = db.Exists(ctx, 1, "t", 1)
	require.ErrorIs(t, err, ErrClosed)

	_, err = db.Schema(ctx, 1, "t", 0)
	require.ErrorIs(t, err, ErrClosed)

	_, err = db.Tables(ctx, 1)
	require.ErrorIs(t, err, ErrClosed)

	_, err = db.TablesIn(ctx, 1, NSUser)
	require.ErrorIs(t, err, ErrClosed)
}

// TestReadPathsRejectUnknownSnapshot: an unknown snapshot is ErrNotFound on
// every addressing API, and TablesIn reports the Tables error unchanged.
func TestReadPathsRejectUnknownSnapshot(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	const unknown SnapshotID = 4242

	_, err := db.Schema(ctx, unknown, "t", 0)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "snapshot 4242")

	_, err = db.Tables(ctx, unknown)
	require.ErrorIs(t, err, ErrNotFound)

	_, err = db.TablesIn(ctx, unknown, NSUser)
	require.ErrorIs(t, err, ErrNotFound, "TablesIn is a filter over Tables, so it reports Tables' error")
}

// publishCraftedView（testutil_test.go，DELTA 简写）构造并发布一个只含 add 产出
// 条目的快照：写入器总是把索引与块一起提交，索引与块不一致的情形只能在这里装配。

// TestGetRejectsMissingBlock: the row index names a block the view has never
// seen. The row cannot be read and there is nothing to decode, so this is a
// loud failure — not ErrNotFound, which would mean "the row is not here".
func TestGetRejectsMissingBlock(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	publishCraftedView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 2, TableID: 1, RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    123456, ItemOrdinal: 0, // never committed
		}))
	})

	_, err := db.Get(ctx, 2, "t", 1, nil)
	require.ErrorContains(t, err, "missing from view")
	require.NotErrorIs(t, err, ErrNotFound)
}

// TestGetRejectsTombstoneRecord: the index claims an upsert while the record in
// the block is a tombstone. The disagreement is corruption of this snapshot's
// table, not an absent row.
func TestGetRejectsTombstoneRecord(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	// Commit a real delete: snapshot 2 holds the tombstone record.
	tx, err := db.Begin(ctx, 1)
	require.NoError(t, err)
	require.NoError(t, tx.Delete(ctx, "t", 1))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	st, err := db.captureState()
	require.NoError(t, err)
	loc, ok := st.view.ResolveRow(2, 1, 1)
	require.True(t, ok)
	require.Equal(t, format.ChangeDelete, loc.ChangeType, "snapshot 2 resolves the row to a tombstone")

	// Re-point the same record as an upsert.
	publishCraftedView(t, db, 3, 2, func(b *index.Builder) {
		require.NoError(t, b.AddRow(format.RowIndexEntry{
			SnapshotID: 3, TableID: 1, RowID: 1,
			ChangeType: format.ChangeInsert,
			BlockID:    loc.BlockID, ItemOrdinal: loc.ItemOrdinal,
		}))
	})

	_, err = db.Get(ctx, 3, "t", 1, nil)
	requireCorruption(t, "Get of a row whose block record is a tombstone", err)
	require.ErrorContains(t, err, "tombstone")
}

// TestTablesSkipsUndecodableTable: a table object can be listed by the index
// while its metadata record cannot be read (the block never reached the view).
// Tables lists what it can decode and skips the rest — one unreadable table
// must not hide the tables that are readable, and must not fail the listing.
//
// Reached white-box: a view carrying such an entry cannot be produced by any
// writer, since the index and the blocks are committed together.
func TestTablesSkipsUndecodableTable(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	st, err := db.captureState()
	require.NoError(t, err)

	const (
		parentID  = 1
		newSnapID = 2
		ghostTID  = 77
	)
	b := index.NewBuilder(1)
	require.NoError(t, b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID:       newSnapID,
		ParentSnapshotID: parentID,
		SnapshotType:     format.SnapshotDelta,
		CreatedUnixNano:  time.Now().UnixNano(),
	}))
	require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
		SnapshotID: newSnapID, ObjectID: metadata.ObjectID(ghostTID),
		Revision: 1, RecordType: uint32(format.RecordTable),
		BlockID: 123456, ItemOrdinal: 0, // a block the view has never heard of
	}))
	_, txn, err := b.Build(index.BodyBounds{}, 0)
	require.NoError(t, err)
	view, err := st.view.Apply(txn, format.DefaultMaxSnapshotDepth)
	require.NoError(t, err)
	db.state.Store(&publishedState{view: view, schemas: st.schemas})

	tables, err := db.Tables(ctx, newSnapID)
	require.NoError(t, err)
	require.Len(t, tables, 1, "the undecodable table is skipped, the readable one is still listed")
	require.Equal(t, "t", tables[0].Name)
}
