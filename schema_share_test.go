package rowpack

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildIndexSharesNeutralDeltaResults: a DELTA whose own layer adds no
// Table/Column records derives exactly its parent's schemas, so buildIndex
// must share the parent's per-snapshot maps instead of re-deriving (and
// re-allocating every codec.Schema and decoder) once per snapshot. A DELTA
// that does carry a schema change must get its own derivation.
func TestBuildIndexSharesNeutralDeltaResults(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{})
	require.NoError(t, err)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "n", Type: TypeUint64},
	}))
	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, tx.Insert(ctx, "t", i, Row{Uint64(i)}))
	}
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// Neutral deltas: plain inserts, no DefineTable.
	var deltas []SnapshotID
	parent := full
	for i := 0; i < 5; i++ {
		tx, err := db.Begin(ctx, parent)
		require.NoError(t, err)
		id := uint64(100 + i)
		require.NoError(t, tx.Insert(ctx, "t", id, Row{Uint64(id)}))
		snap, err := tx.Commit(ctx)
		require.NoError(t, err)
		deltas = append(deltas, snap)
		parent = snap
	}
	require.NoError(t, db.Close())

	db, err = Open(base, Options{})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	si := db.state.Load().schemas
	parentTables := si.bySnapshot[uint64(full)]
	require.NotNil(t, parentTables)
	parentAddr := si.byAddress[uint64(full)]
	for _, snap := range deltas {
		require.Equal(t,
			reflect.ValueOf(parentTables).Pointer(),
			reflect.ValueOf(si.bySnapshot[uint64(snap)]).Pointer(),
			"neutral DELTA %d must share its parent's table map", snap)
		require.Equal(t,
			reflect.ValueOf(parentAddr).Pointer(),
			reflect.ValueOf(si.byAddress[uint64(snap)]).Pointer(),
			"neutral DELTA %d must share its parent's address map", snap)
	}

	// A delta that defines another table carries Table+Column records in its
	// own layer, so it must get its own derivation (and keep the parent's
	// table visible through the chain).
	tx, err = db.Begin(ctx, parent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("u", []Column{
		{Name: "s", Type: TypeString},
	}))
	require.NoError(t, tx.Insert(ctx, "u", 999, Row{String("x")}))
	changed, err := tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(base, Options{})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	si = db.state.Load().schemas
	require.NotEqual(t,
		reflect.ValueOf(si.bySnapshot[uint64(parent)]).Pointer(),
		reflect.ValueOf(si.bySnapshot[uint64(changed)]).Pointer(),
		"a schema-adding DELTA must not share the parent's derivation")
	require.NotNil(t, si.schema(uint64(changed), 1, 1), "parent table stays visible")
	require.NotNil(t, si.schema(uint64(changed), 2, 1), "new table is derived")
}
