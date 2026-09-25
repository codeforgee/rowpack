package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// tx_arms_test.go 覆盖事务入口的两条臂:Latest 这一路要先取已发布状态,store 已关闭
// 时必须把读锁放掉再回答 ErrClosed(不能顺手吞掉锁);以及 Apply 遇到不认识的变更类型
// 时拒绝,而不是当成插入。

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

	err := tx.Apply(Change{Type: ChangeType(99), Table: "t", RowID: 1, Row: Row{Uint64(1)}})
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorContains(t, err, "change type")
}
