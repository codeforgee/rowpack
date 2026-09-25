// Package rowpack_test exercises the source-catalog pattern documented in
// docs/SOURCE_CATALOG_GUIDE_V1.md through the *public* API only, exactly as an
// upper-layer adapter would. It exists to keep the guide honest: if a claim in
// the guide stops holding, one of these tests fails.
package rowpack_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack"
	"github.com/stretchr/testify/require"
)

const (
	// catalogNamespace is the caller's own namespace for its bookkeeping tables:
	// it keeps them out of the default (source) namespace in one filter.
	catalogNamespace = "_rowpack"
)

// The catalog tables are addressed namespace-qualified, because a table
// identity is (namespace, name).
var (
	catalogObjects = rowpack.Qualify(catalogNamespace, "__rowpack_src_objects")
	catalogColumns = rowpack.Qualify(catalogNamespace, "__rowpack_src_columns")
)

// defineCatalog mirrors the guide's §2.2 column lists.
func defineCatalog(t *testing.T, tx *rowpack.Tx) {
	t.Helper()
	require.NoError(t, tx.DefineTableIn(catalogNamespace, "__rowpack_src_objects", []rowpack.Column{
		{Name: "kind", Type: rowpack.TypeString},
		{Name: "schema", Type: rowpack.TypeString},
		{Name: "name", Type: rowpack.TypeString},
		{Name: "parent", Type: rowpack.TypeString, Nullable: true},
		{Name: "revision", Type: rowpack.TypeUint64},
		{Name: "dropped", Type: rowpack.TypeBool},
		{Name: "declared", Type: rowpack.TypeBytes, Nullable: true},
		{Name: "attrs", Type: rowpack.TypeBytes, Nullable: true},
	}))
	require.NoError(t, tx.DefineTableIn(catalogNamespace, "__rowpack_src_columns", []rowpack.Column{
		{Name: "owner", Type: rowpack.TypeString},
		{Name: "name", Type: rowpack.TypeString},
		{Name: "ordinal", Type: rowpack.TypeUint32},
		{Name: "declared", Type: rowpack.TypeString},
		{Name: "data_type", Type: rowpack.TypeString},
		{Name: "s_nullable", Type: rowpack.TypeBool},
		{Name: "s_default", Type: rowpack.TypeBytes, Nullable: true},
		{Name: "s_length", Type: rowpack.TypeUint32, Nullable: true},
		{Name: "revision", Type: rowpack.TypeUint64},
		{Name: "dropped", Type: rowpack.TypeBool},
		{Name: "attrs", Type: rowpack.TypeBytes, Nullable: true},
	}))
}

func optString(s string) rowpack.Value {
	if s == "" {
		return rowpack.Null()
	}
	return rowpack.String(s)
}

func optBytes(b []byte) rowpack.Value {
	if len(b) == 0 {
		return rowpack.Null()
	}
	return rowpack.Bytes(b)
}

func optUint32(v uint32) rowpack.Value {
	if v == 0 {
		return rowpack.Null()
	}
	return rowpack.Uint32(v)
}

func objectRow(kind, schema, name, parent string, revision uint64, dropped bool, declared, attrs []byte) rowpack.Row {
	return rowpack.Row{
		rowpack.String(kind), rowpack.String(schema), rowpack.String(name),
		optString(parent), rowpack.Uint64(revision), rowpack.Bool(dropped),
		optBytes(declared), optBytes(attrs),
	}
}

func columnRow(owner, name string, ordinal uint32, declared, dataType string, nullable bool,
	def []byte, length uint32, revision uint64, dropped bool, attrs []byte) rowpack.Row {
	return rowpack.Row{
		rowpack.String(owner), rowpack.String(name), rowpack.Uint32(ordinal),
		rowpack.String(declared), rowpack.String(dataType), rowpack.Bool(nullable),
		optBytes(def), optUint32(length), rowpack.Uint64(revision), rowpack.Bool(dropped),
		optBytes(attrs),
	}
}

// columnRowID is the guide's §3.2 layout.
func columnRowID(tableSeq rowpack.RowID, colSeq uint32) rowpack.RowID {
	return tableSeq<<16 | rowpack.RowID(colSeq)
}

// TestCatalogGuide_SystemNamespaceIsolated pins rule R1: catalog tables live in
// sys, source tables in user, and one call separates them.
func TestCatalogGuide_SystemNamespaceIsolated(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	defineCatalog(t, tx)
	require.NoError(t, tx.DefineTable("users", []rowpack.Column{
		{Name: "id", Type: rowpack.TypeUint64},
		{Name: "name", Type: rowpack.TypeString, Nullable: true},
	}))
	require.NoError(t, tx.Insert("users", 1, rowpack.Row{rowpack.Uint64(1), rowpack.String("a")}))
	// tableSeq = 1 for users in the catalog, colSeq 1 and 2.
	require.NoError(t, tx.Insert(catalogObjects, 1,
		objectRow("table", "public", "users", "", 1, false, []byte("CREATE TABLE users(...)"), nil)))
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 1),
		columnRow("users", "id", 1, "bigint", "int64", false, nil, 8, 1, false, nil)))
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 2),
		columnRow("users", "name", 2, "varchar(64)", "string", true, []byte("'anonymous'"), 64, 1, false, nil)))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	all, err := db.Tables(ctx, snap)
	require.NoError(t, err)
	require.Len(t, all, 3)

	mine, other := splitByNamespace(all, catalogNamespace)
	require.Len(t, mine, 2, "both catalog tables")
	require.Len(t, other, 1, "only the source table")
	require.Equal(t, "users", other[0].Name)
	require.Equal(t, rowpack.NSUser, other[0].NS)

	// TablesIn is the same filter as a one-liner.
	defaultNS, err := db.TablesIn(ctx, snap, rowpack.NSUser)
	require.NoError(t, err)
	require.Len(t, defaultNS, 1)
}

// splitByNamespace is the filter a source-mirroring caller uses: catalog tables
// live in one namespace, source tables in the default one.
func splitByNamespace(tables []rowpack.Table, ns string) (mine, other []rowpack.Table) {
	for _, tbl := range tables {
		if tbl.NS == ns {
			mine = append(mine, tbl)
			continue
		}
		other = append(other, tbl)
	}
	return mine, other
}

// TestCatalogGuide_RangeScanByTableSeq pins the §3.2 claim: with
// tableSeq<<16|colSeq, one table's columns are contiguous and reachable with a
// bounded range scan instead of a full scan.
func TestCatalogGuide_RangeScanByTableSeq(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	defineCatalog(t, tx)
	// users => tableSeq 1 (3 columns), orders => tableSeq 2 (2 columns).
	require.NoError(t, tx.Insert(catalogObjects, 1, objectRow("table", "public", "users", "", 1, false, nil, nil)))
	require.NoError(t, tx.Insert(catalogObjects, 2, objectRow("table", "public", "orders", "", 1, false, nil, nil)))
	for i, name := range []string{"id", "email", "created_at"} {
		require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, uint32(i+1)),
			columnRow("users", name, uint32(i+1), "bigint", "int64", false, nil, 8, 1, false, nil)))
	}
	for i, name := range []string{"id", "total"} {
		require.NoError(t, tx.Insert(catalogColumns, columnRowID(2, uint32(i+1)),
			columnRow("orders", name, uint32(i+1), "bigint", "int64", false, nil, 8, 1, false, nil)))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	colNames := func(tableSeq rowpack.RowID) []string {
		it, err := db.Scan(ctx, snap, catalogColumns, rowpack.ScanOptions{
			Start: tableSeq << 16,
			End:   (tableSeq + 1) << 16,
		})
		require.NoError(t, err)
		defer it.Close()
		var out []string
		for {
			row, ok := it.Next()
			if !ok {
				break
			}
			name, _ := row[1].String() // 访问器返回副本，可跨 Next 留存
			out = append(out, name)
			require.Equal(t, row, row, "keep linters quiet about unused row")
		}
		require.NoError(t, it.Err())
		return out
	}

	require.Equal(t, []string{"id", "email", "created_at"}, colNames(1))
	require.Equal(t, []string{"id", "total"}, colNames(2))
}

// TestCatalogGuide_TombstoneAcrossDeltas pins §5/§6.3: a DROP is an UPDATE to
// dropped=true, keeps the RowID, and never needs ErrNotFound handling.
func TestCatalogGuide_TombstoneAcrossDeltas(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	defineCatalog(t, tx)
	require.NoError(t, tx.Insert(catalogObjects, 1, objectRow("table", "public", "users", "", 1, false, nil, nil)))
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 1),
		columnRow("users", "id", 1, "bigint", "int64", false, nil, 8, 1, false, nil)))
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 2),
		columnRow("users", "legacy", 2, "text", "string", true, nil, 0, 1, false, nil)))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// DELTA: drop the column, then add a new one — colSeq must advance past the
	// dropped one, never reuse it.
	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, tx.Update(catalogColumns, columnRowID(1, 2),
		columnRow("users", "legacy", 2, "text", "string", true, nil, 0, 2, true, nil)))
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 3),
		columnRow("users", "email", 3, "varchar(255)", "string", false, nil, 255, 1, false, nil)))
	delta, err := tx.Commit(ctx)
	require.NoError(t, err)

	// UPDATE on a row that does not exist in the parent is rejected:
	// the catalog has no upsert, which is why the parent must be loaded first.
	tx, err = db.Begin(ctx, delta)
	require.NoError(t, err)
	require.ErrorIs(t, tx.Update(catalogObjects, 999,
		objectRow("table", "public", "ghost", "", 1, false, nil, nil)), rowpack.ErrNotFound)
	require.NoError(t, tx.Rollback())

	// The same-snapshot RowID uniqueness rule the guide warns about in §6.4.
	tx, err = db.Begin(ctx, delta)
	require.NoError(t, err)
	require.NoError(t, tx.Insert(catalogObjects, 7, objectRow("table", "public", "a", "", 1, false, nil, nil)))
	require.ErrorIs(t, tx.Insert(catalogObjects, 7, objectRow("table", "public", "b", "", 1, false, nil, nil)),
		rowpack.ErrAlreadyExists)
	require.NoError(t, tx.Rollback())

	// Readers see the tombstone: three rows, one of them dropped.
	it, err := db.Scan(ctx, delta, catalogColumns, rowpack.ScanOptions{})
	require.NoError(t, err)
	defer it.Close()
	var live, dropped int
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		if d, _ := row[9].Bool(); d {
			dropped++
			require.Equal(t, rowpack.RowID(columnRowID(1, 2)), it.RowID(), "tombstone keeps its RowID")
		} else {
			live++
		}
	}
	require.NoError(t, it.Err())
	require.EqualValues(t, 2, live)
	require.EqualValues(t, 1, dropped)
}

// TestCatalogGuide_NullableContract pins §2.2 note 1: the engine enforces
// Nullable at write time, so attributes that may be absent must be declared
// Nullable.
func TestCatalogGuide_NullableContract(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn(catalogNamespace, "strict", []rowpack.Column{
		{Name: "a", Type: rowpack.TypeString},                 // NOT NULL
		{Name: "b", Type: rowpack.TypeString, Nullable: true}, // NULL allowed
	}))
	strict := rowpack.Qualify(catalogNamespace, "strict")
	require.ErrorIs(t, tx.Insert(strict, 1, rowpack.Row{rowpack.Null(), rowpack.String("x")}),
		rowpack.ErrSchemaMismatch, "writing NULL into a NOT NULL column must fail")
	require.NoError(t, tx.Insert(strict, 1, rowpack.Row{rowpack.String("x"), rowpack.Null()}))
	require.NoError(t, tx.Rollback())
}

// TestCatalogGuide_DeltaChangeStream pins §7: catalog changes are ordinary row
// changes, so the block-level change stream already answers "what changed".
func TestCatalogGuide_DeltaChangeStream(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	defineCatalog(t, tx)
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 1),
		columnRow("users", "id", 1, "bigint", "int64", false, nil, 8, 1, false, nil)))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, tx.Insert(catalogColumns, columnRowID(1, 2),
		columnRow("users", "email", 2, "varchar(255)", "string", false, nil, 255, 1, false, nil)))
	require.NoError(t, tx.Update(catalogColumns, columnRowID(1, 1),
		columnRow("users", "id", 1, "bigint unsigned", "uint64", false, nil, 8, 2, false, nil)))
	delta, err := tx.Commit(ctx)
	require.NoError(t, err)

	blocks, err := db.Blocks(ctx, delta, catalogColumns)
	require.NoError(t, err)
	require.NotEmpty(t, blocks)

	it, err := db.ScanBlocks(ctx, delta, catalogColumns, blocks[0].BlockID, blocks[len(blocks)-1].BlockID+1)
	require.NoError(t, err)
	defer it.Close()

	got := map[rowpack.ChangeType][]rowpack.RowID{}
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		require.NotNil(t, row, "catalog uses tombstones, never DELETE records")
		got[it.ChangeType()] = append(got[it.ChangeType()], it.RowID())
	}
	require.NoError(t, it.Err())
	require.Equal(t, []rowpack.RowID{columnRowID(1, 1)}, got[rowpack.ChangeUpdate])
	require.Equal(t, []rowpack.RowID{columnRowID(1, 2)}, got[rowpack.ChangeInsert])
}

// TestCatalogGuide_ParentMustExistBeforeCatalog pins the guide's "first run"
// branch: scanning a catalog table that a snapshot does not define yet reports
// ErrNotFound rather than failing the store, and a later DELTA can introduce it.
func TestCatalogGuide_ParentMustExistBeforeCatalog(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	// A store whose first snapshot predates the catalog tables.
	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", []rowpack.Column{{Name: "id", Type: rowpack.TypeUint64}}))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	_, err = db.Scan(ctx, full, catalogObjects, rowpack.ScanOptions{})
	require.ErrorIs(t, err, rowpack.ErrNotFound, "guide §4.2 must treat this as an empty parent catalog")

	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	defineCatalog(t, tx)
	delta, err := tx.Commit(ctx)
	require.NoError(t, err)

	// The catalog now exists. Every iterator must be closed: a leaked one keeps
	// the store's read lock and makes Close block until the GC finalizer runs.
	it, err := db.Scan(ctx, delta, catalogObjects, rowpack.ScanOptions{})
	require.NoError(t, err)
	require.NoError(t, it.Close())
}

// TestCatalogGuide_DefineBeforeWrite pins the guide's checklist item "every
// written table is defined in the same snapshot": a DELTA may write a chain
// table as is, while a FULL snapshot must define it, or Commit reports
// ErrInvalidArgument instead of publishing rows no reader can decode.
func TestCatalogGuide_DefineBeforeWrite(t *testing.T) {
	ctx := context.Background()
	db, err := rowpack.Create(filepath.Join(t.TempDir(), "s"), rowpack.Options{})
	require.NoError(t, err)
	defer db.Close()

	userCols := []rowpack.Column{{Name: "id", Type: rowpack.TypeUint64}}
	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", userCols))
	require.NoError(t, tx.Insert("users", 1, rowpack.Row{rowpack.Uint64(1)}))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// DELTA: the chain already defines users, so writing before the (no-op)
	// re-definition is fine.
	d, err := db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, d.Insert("users", 2, rowpack.Row{rowpack.Uint64(2)}))
	require.NoError(t, d.DefineTable("users", userCols))
	delta, err := d.Commit(ctx)
	require.NoError(t, err)
	row, err := db.Get(ctx, delta, "users", 2, nil)
	require.NoError(t, err)
	require.Len(t, row, 1)

	// FULL: its metadata is not visible through any ancestor, so it has to
	// define every table it writes.
	ck, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	require.NoError(t, ck.Insert("users", 3, rowpack.Row{rowpack.Uint64(3)}))
	_, err = ck.Commit(ctx)
	require.ErrorIs(t, err, rowpack.ErrInvalidArgument)
	require.NoError(t, ck.Rollback())
}
