package rowpack

import (
	"context"
	"fmt"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/metadata"
	"github.com/stretchr/testify/require"
)

// TestUnknownColumnTypeForwardCompat pins the forward-compatibility contract
// for the schema index: a Table whose Column record carries a canonical type
// string this engine does not understand (e.g. written by a newer version) is
// skipped from the schema index — opening the store must succeed, every other
// table must remain fully readable, and the unknown table must not appear as
// a usable table.
func TestUnknownColumnTypeForwardCompat(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{BlockSize: 512})
	require.NoError(t, err)

	// FULL: a normal table with rows.
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}))
	for i := 1; i <= 5; i++ {
		require.NoError(t, tx.Insert("users", RowID(i), Row{Uint64(uint64(i)), String(fmt.Sprintf("user-%d", i))}))
	}
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// DELTA: hand-write a "future" table whose only column has an unknown
	// canonical type string — exactly what a newer engine version would
	// produce after adding a type this binary does not know.
	tx2, err := db.Begin(ctx, full)
	require.NoError(t, err)
	const futureTableOID = uint64(500)
	const futureColumnOID = uint64(1)<<32 + 100
	futureTable := &metadata.Record{
		RecordType:  uint32(format.RecordTable),
		ObjectID:    futureTableOID,
		Revision:    1,
		Namespace:   format.NamespaceCore,
		ExternalKey: "future",
		Fields: []metadata.Field{
			{ID: metadata.TableName, WireType: format.WireString, Value: "future"},
		},
	}
	futureColumn := &metadata.Record{
		RecordType: uint32(format.RecordColumn),
		ObjectID:   futureColumnOID,
		ParentID:   futureTableOID,
		Revision:   1,
		Namespace:  format.NamespaceCore,
		Fields: []metadata.Field{
			{ID: metadata.ColColumnID, WireType: format.WireSint, Value: int64(1)},
			{ID: metadata.ColColumnName, WireType: format.WireString, Value: "payload"},
			{ID: metadata.ColColumnType, WireType: format.WireString, Value: "jsonb"},
			{ID: metadata.ColNullable, WireType: format.WireString, Value: "YES"},
			{ID: metadata.ColDataScale, WireType: format.WireSint, Value: int64(0)},
		},
	}
	require.NoError(t, tx2.w.writeMetadata(futureTable))
	require.NoError(t, tx2.w.writeMetadata(futureColumn))
	delta, err := tx2.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Reopen: the unknown-typed table must not fail the open.
	db2, err := Open(db.Path(), Options{BlockSize: 512})
	require.NoError(t, err)
	defer db2.Close()

	// The known table stays fully readable at both snapshots.
	for _, snap := range []SnapshotID{full, delta} {
		row, err := db2.Get(ctx, snap, "users", 3, nil)
		require.NoError(t, err, "snap %d", snap)
		name, _ := row[1].String()
		require.Equal(t, "user-3", name)
	}
	it, err := db2.Scan(ctx, delta, "users", ScanOptions{})
	require.NoError(t, err)
	n := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		n++
	}
	require.NoError(t, it.Err())
	it.Close()
	require.EqualValues(t, 5, n)

	// The future table has no usable schema: writes are rejected.
	tx3, err := db2.Begin(ctx, delta)
	require.NoError(t, err)
	err = tx3.Insert("future", 1, Row{String("x")})
	require.Error(t, err)
	require.NoError(t, tx3.Rollback())
	_ = tx3

	// Integrity holds.
	_, err = db2.Verify(ctx, VerifyFull)
	require.NoError(t, err)
}
