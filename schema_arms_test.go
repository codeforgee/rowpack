package rowpack

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/index"
)

// schema_arms_test.go 覆盖 schema 派生(deriveTables)与元数据记录读取的三条跳过/拒绝
// 臂:索引条目被标成删除时不读它、表对象的 ObjectID 不是合法表 ID 时拒绝、以及元数据
// 记录被标成删除/序号越出载荷时的失败。索引条目由写入器随块一起提交,删除标记只在
// 事务里删除表时出现,所以后两条用白盒视图构造。
//
// deriveTables 里两条臂不可达:「父链走到一半快照不见了」(View.Apply 只接受父快照已
// 提交的快照,链上不会有洞),以及版本排序(一张表只有一个版本——重定义时列不同是
// ErrSchemaConflict,列相同则沿用原修订号,见 TestDeriveTablesKeepsOneVersion)。

// craftView applies one delta snapshot carrying only the metadata entries add
// emits. Unlike publishCraftedView it does not publish the result: these tests
// ask the schema derivation directly.
func craftView(t *testing.T, db *Store, snapID, parent uint64, add func(b *index.Builder)) *index.View {
	t.Helper()
	st, err := db.captureState()
	require.NoError(t, err)
	b := index.NewBuilder(1)
	require.NoError(t, b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID:       snapID,
		ParentSnapshotID: parent,
		SnapshotType:     format.SnapshotDelta,
		CreatedUnixNano:  time.Now().UnixNano(),
	}))
	if add != nil {
		add(b)
	}
	_, txn, err := b.Build(index.BodyBounds{}, 0)
	require.NoError(t, err)
	view, err := st.view.Apply(txn, format.DefaultMaxSnapshotDepth)
	require.NoError(t, err)
	return view
}

// realTableLoc returns the object id and physical location of a table record
// the fixture store really committed.
func realTableLoc(t *testing.T, db *Store) (uint64, format.MetadataIndexEntry) {
	t.Helper()
	st, err := db.captureState()
	require.NoError(t, err)
	oids := st.view.MetadataByType(1, uint32(format.RecordTable))
	require.NotEmpty(t, oids)
	loc := st.view.Metadata(1, oids[0])
	require.NotNil(t, loc)
	return oids[0], format.MetadataIndexEntry{
		SnapshotID:  1,
		ObjectID:    oids[0],
		Revision:    1,
		RecordType:  uint32(format.RecordTable),
		BlockID:     loc.BlockID,
		ItemOrdinal: loc.ItemOrdinal,
		Operation:   format.OperationUpsert,
	}
}

// TestDeriveTablesSkipsDeletedMetadata: an index entry marked deleted is not a
// record to read. Derivation skips it — for both column and table objects —
// instead of failing the whole snapshot or reporting the object as unreadable.
func TestDeriveTablesSkipsDeletedMetadata(t *testing.T) {
	db := armCommittedStore(t)
	view := craftView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
			SnapshotID: 2, ObjectID: 1000, Revision: 1,
			RecordType: uint32(format.RecordColumn),
			BlockID:    123456, ItemOrdinal: 0,
			Operation: format.OperationDelete, // a column that never was
		}))
		require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
			SnapshotID: 2, ObjectID: 1001, Revision: 1,
			RecordType: uint32(format.RecordTable),
			BlockID:    123456, ItemOrdinal: 0,
			Operation: format.OperationDelete, // a table that never was
		}))
	})

	ds, err := db.deriveTables(view, 2, nil)
	require.NoError(t, err, "deleted entries are skipped, not read")
	require.NotEmpty(t, ds.tables, "the tables of the parent chain are still derived")
}

// TestDeriveTablesRejectsUnusableTableObjectID: the record is readable but its
// object id is not a table id, so there is nothing to attach the columns to.
func TestDeriveTablesRejectsUnusableTableObjectID(t *testing.T) {
	db := armCommittedStore(t)
	_, loc := realTableLoc(t, db)
	loc.SnapshotID, loc.ObjectID = 2, 0 // the real record, under an unusable id

	view := craftView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddMetadata(loc))
	})

	_, err := db.deriveTables(view, 2, nil)
	require.ErrorContains(t, err, "table object 0")
}

// TestTableRecordRejectsDeletedMetadata: an object the index says is deleted has
// no record, whatever the block still holds.
func TestTableRecordRejectsDeletedMetadata(t *testing.T) {
	db := armCommittedStore(t)
	oid, loc := realTableLoc(t, db)
	loc.SnapshotID, loc.Operation = 2, format.OperationDelete

	view := craftView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddMetadata(loc))
	})

	_, err := db.tableRecord(view, 2, oid)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "deleted")
}

// TestTableRecordRejectsOrdinalBeyondPayload: the index points past the records
// the metadata block carries, so this is corruption of the block, not an absent
// object.
func TestTableRecordRejectsOrdinalBeyondPayload(t *testing.T) {
	db := armCommittedStore(t)
	oid, loc := realTableLoc(t, db)
	loc.SnapshotID, loc.ItemOrdinal = 2, 1<<20

	view := craftView(t, db, 2, 1, func(b *index.Builder) {
		require.NoError(t, b.AddMetadata(loc))
	})

	_, err := db.tableRecord(view, 2, oid)
	requireCorruption(t, "a metadata ordinal past the payload", err)
	require.ErrorContains(t, err, "out of range")
}

// TestDeriveTablesKeepsOneVersion: a table carries one version, so the version
// sort has nothing to order. Redefining it with other columns is a conflict and
// redefining it identically keeps the revision — both verified here, which is
// why the sort arm cannot be reached: no table ever has two versions.
func TestDeriveTablesKeepsOneVersion(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t) // snapshot 1: table t with one column
	tables, err := db.Tables(ctx, 1)
	require.NoError(t, err)
	require.Len(t, tables, 1)
	require.Equal(t, uint32(1), tables[0].LatestVersion)

	tx := armTx(t, db, 1)
	require.NoError(t, tx.DefineTable("t", armCols)) // identical: allowed
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	tables, err = db.Tables(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, uint32(1), tables[0].LatestVersion)

	tx2 := armTx(t, db, snap)
	err = tx2.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}, {Name: "v", Type: TypeUint64}})
	require.ErrorIs(t, err, ErrSchemaConflict, "any other column set is a conflict, not a new version")
}
