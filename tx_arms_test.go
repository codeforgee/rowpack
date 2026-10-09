package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// tx_arms_test.go 覆盖事务入口的两条臂:Latest 这一路要先取已发布状态,store 已关闭
// 时必须把读锁放掉再回答 ErrClosed(不能顺手吞掉锁);以及 Apply 遇到不认识的变更类型
// 时拒绝,而不是当成插入。SealTable 的三种结局(未知表、无待封行、已提交)也在本文件。

// TestBeginLatestOnClosedStore: resolving Latest needs the published state, so
// this branch takes the read lock itself — a closed store must release it
// before answering ErrClosed.
func TestBeginLatestOnClosedStore(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	require.NoError(t, db.Close())

	_, err := db.Begin(ctx, Latest)
	require.ErrorIs(t, err, ErrClosed)

	// The lock was released: a second call must not block.
	_, err = db.Begin(ctx, Latest)
	require.ErrorIs(t, err, ErrClosed)
}

// TestApplyRejectsUnknownChangeType: an unknown change type is a caller bug,
// not an insert.
func TestApplyRejectsUnknownChangeType(t *testing.T) {
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))

	err := tx.Apply(context.Background(), Change{Type: ChangeType(99), Table: "t", RowID: 1, Row: Row{Uint64(1)}})
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorContains(t, err, "change type")
}

// TestSealTableUnknownEmptyAndCommitted: SealTable's three outcomes — an
// unknown table is ErrNotFound, sealing with no pending rows is a no-op, and a
// committed transaction refuses further sealing.
func TestSealTableUnknownEmptyAndCommitted(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))

	// Unknown table.
	require.ErrorIs(t, w.SealTable("nope"), ErrNotFound)
	// No pending rows: a no-op.
	require.NoError(t, w.SealTable("t"))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1)}))
	require.NoError(t, w.SealTable("t"))
	_, err = w.Commit(ctx)
	require.NoError(t, err)

	// The committed transaction rejects further sealing.
	require.ErrorIs(t, w.SealTable("t"), ErrSnapshotCommitted)
}
