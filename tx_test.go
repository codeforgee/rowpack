package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTxFullAndDeltaBatch(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	full, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, full.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, full.ApplyBatch(ctx, []Change{
		{Type: ChangeInsert, Table: "t", RowID: 1, Row: Row{Uint64(10)}},
		{Type: ChangeInsert, Table: "t", RowID: 2, Row: Row{Uint64(20)}},
	}))
	first, err := full.Commit(ctx)
	require.NoError(t, err)

	delta, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.Equal(t, first, delta.Parent())
	require.NoError(t, delta.ApplyBatch(ctx, []Change{
		{Type: ChangeUpdate, Table: "t", RowID: 1, Row: Row{Uint64(11)}},
		{Type: ChangeDelete, Table: "t", RowID: 2},
		{Type: ChangeInsert, Table: "t", RowID: 3, Row: Row{Uint64(30)}},
	}))
	second, err := delta.Commit(ctx)
	require.NoError(t, err)

	row, err := db.Get(ctx, second, "t", 1, nil)
	require.NoError(t, err)
	v, ok := row[0].Uint64()
	require.True(t, ok)
	require.Equal(t, uint64(11), v)
	_, err = db.Get(ctx, second, "t", 2, nil)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Get(ctx, second, "t", 3, nil)
	require.NoError(t, err)
}

func TestBeginLatestOnEmptyStore(t *testing.T) {
	db := testDB(t, Options{})
	_, err := db.Begin(context.Background(), Latest)
	require.ErrorIs(t, err, ErrInvalidParent)
}

func TestApplyRejectsDeletePayload(t *testing.T) {
	db := testDB(t, Options{})
	tx, err := db.Begin(context.Background(), NoParent)
	require.NoError(t, err)
	defer tx.Rollback()
	err = tx.Apply(context.Background(), Change{Type: ChangeDelete, Table: "t", RowID: 1, Row: Row{Uint64(1)}})
	require.ErrorIs(t, err, ErrInvalidArgument)
}
