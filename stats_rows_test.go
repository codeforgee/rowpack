package rowpack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Stats().LogicalRows 的口径是「最新快照下可见的逻辑行数」（REQUIREMENTS 的只读
// 统计项、GO_API_DESIGN §7 的「逻辑行计数」）。它必须覆盖在该快照上可见的**全部**
// 表，而不是本事务自己写过行的表：DELTA 只碰一张表时，其余表的祖先行仍然可见；
// 空 DELTA（无任何行变更）也不该把整个 store 报成 0 行。
func TestStatsLogicalRowsCoversAllVisibleTables(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "stats")
	db, err := Create(base, Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("a", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.DefineTableIn("meta", "b", []Column{{Name: "id", Type: TypeUint64}}))
	for i := RowID(1); i <= 10; i++ {
		require.NoError(t, tx.Insert("a", i, Row{Uint64(uint64(i))}))
	}
	for i := RowID(1); i <= 5; i++ {
		require.NoError(t, tx.Insert("meta.b", i, Row{Uint64(uint64(i))}))
	}
	s1, err := tx.Commit(ctx)
	require.NoError(t, err)
	st := db.Stats()
	require.Equal(t, uint64(2), st.Tables)
	require.Equal(t, uint64(15), st.LogicalRows, "FULL: 10 + 5 visible rows")

	// DELTA 只更新 a 表：b 表的 5 行仍可见。
	tx2, err := db.Begin(ctx, s1)
	require.NoError(t, err)
	for i := RowID(1); i <= 3; i++ {
		require.NoError(t, tx2.Update("a", i, Row{Uint64(uint64(i) * 100)}))
	}
	s2, err := tx2.Commit(ctx)
	require.NoError(t, err)
	st = db.Stats()
	require.Equal(t, uint64(2), st.Tables, "the untouched table is still visible")
	require.Equal(t, uint64(15), st.LogicalRows, "updates do not change the visible row count")

	// DELTA 只删 b 表两行。
	tx3, err := db.Begin(ctx, s2)
	require.NoError(t, err)
	require.NoError(t, tx3.Delete("meta.b", 4))
	require.NoError(t, tx3.Delete("meta.b", 5))
	s3, err := tx3.Commit(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(13), db.Stats().LogicalRows)

	// 空 DELTA：什么都没写，最新快照的可见行数不得塌成 0。
	tx4, err := db.Begin(ctx, s3)
	require.NoError(t, err)
	s4, err := tx4.Commit(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(4), s4)
	require.Equal(t, uint64(2), db.Stats().Tables)
	require.Equal(t, uint64(13), db.Stats().LogicalRows,
		"an empty tail snapshot must still report the inherited rows")

	// 重开一致（Stats 与视图来源无关），且 RowTables（本事务自己写过行的表）与
	// Stats 的口径差异正是本用例钉住的点。
	require.NoError(t, db.Close())
	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer db2.Close()
	require.Equal(t, uint64(13), db2.Stats().LogicalRows)
	db2.readMu.RLock()
	ownRowTables := db2.state.Load().view.RowTables(uint64(s4))
	db2.readMu.RUnlock()
	require.Empty(t, ownRowTables, "the empty delta owns no row entries (why the old sum was 0)")
}
