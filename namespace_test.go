package rowpack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
	"github.com/stretchr/testify/require"
)

func nsCols() []Column {
	return []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}
}

func nsRow(id uint64, name string) Row {
	return Row{Uint64(id), String(name)}
}

// tableByName returns the listed table with the given name.
func tableByName(t *testing.T, tables []Table, name string) Table {
	t.Helper()
	for _, tbl := range tables {
		if tbl.Name == name {
			return tbl
		}
	}
	require.Failf(t, "table not found", "name %q in %v", name, tables)
	return Table{}
}

// tableAddresses returns each table's namespace-qualified address.
func tableAddresses(tables []Table) []string {
	out := make([]string, 0, len(tables))
	for _, tbl := range tables {
		out = append(out, tbl.Address())
	}
	return out
}

// TestNamespaceDefaultIsImplicit pins the compatibility contract: a table
// defined with DefineTable carries no Namespace record at all, so stores written
// before namespaces existed keep their exact bytes and semantics.
func TestNamespaceDefaultIsImplicit(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "implicit"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	require.NoError(t, tx.Insert("users", 1, nsRow(1, "a")))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	tables, err := db.Tables(ctx, full)
	require.NoError(t, err)
	require.Len(t, tables, 1)
	require.Equal(t, NSUser, tables[0].NS)

	row, err := db.Get(ctx, full, "users", 1, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "a", name)
}

// TestNSUserChosen covers the public surface: namespaces are a free-form
// caller label, and TablesIn filters on it.
func TestNSUserChosen(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "chosen"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	require.NoError(t, tx.DefineTableIn("catalog", "_src_columns", nsCols()))
	require.NoError(t, tx.DefineTableIn("public", "orders", nsCols()))
	require.NoError(t, tx.Insert("users", 1, nsRow(1, "a")))
	// A non-default namespace is addressed as "<namespace>.<name>".
	require.NoError(t, tx.Insert("catalog._src_columns", 1, nsRow(1, "c")))
	require.NoError(t, tx.Insert("public.orders", 1, nsRow(1, "o")))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	tables, err := db.Tables(ctx, full)
	require.NoError(t, err)
	require.Len(t, tables, 3)

	require.Equal(t, NSUser, tableByName(t, tables, "users").NS)
	require.Equal(t, "catalog", tableByName(t, tables, "_src_columns").NS)
	require.Equal(t, "public", tableByName(t, tables, "orders").NS)

	user, err := db.TablesIn(ctx, full, NSUser)
	require.NoError(t, err)
	require.Equal(t, []string{"users"}, tableAddresses(user))

	pub, err := db.TablesIn(ctx, full, "public")
	require.NoError(t, err)
	require.Equal(t, []string{"public.orders"}, tableAddresses(pub))

	// Tables in a non-default namespace are ordinary tables: readable by address.
	row, err := db.Get(ctx, full, "public.orders", 1, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "o", name)
}

// TestNamespaceEmptyRejected: the namespace is part of the table identity, so an
// empty one is rejected rather than silently meaning "default".
func TestNamespaceEmptyRejected(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "empty"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.ErrorIs(t, tx.DefineTableIn("", "t", nsCols()), ErrInvalidArgument)
	require.NoError(t, tx.Rollback())
}

// TestNamespaceSameNameCoexists pins the core requirement: the table identity is
// (namespace, name), so the same name in two namespaces is not a conflict, and
// each one is addressed as "<namespace>.<name>".
func TestNamespaceSameNameCoexists(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "coexist"), Options{})
	require.NoError(t, err)
	defer db.Close()

	// Same name, two non-default namespaces, in one transaction.
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("public", "users", nsCols()))
	require.NoError(t, tx.DefineTableIn("audit", "users", nsCols()))
	require.NoError(t, tx.Insert("public.users", 1, nsRow(1, "pub")))
	require.NoError(t, tx.Insert("audit.users", 1, nsRow(1, "aud")))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// Two distinct tables.
	tables, err := db.Tables(ctx, full)
	require.NoError(t, err)
	require.Len(t, tables, 2)
	require.NotEqual(t, tables[0].ID, tables[1].ID)
	require.Equal(t, []string{"public.users", "audit.users"}, tableAddresses(tables))

	// The bare name belongs to the default namespace, which has no such table.
	_, err = db.Get(ctx, full, "users", 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "a bare name addresses the default namespace only")

	// Each namespace keeps its own rows.
	for _, tc := range []struct{ addr, want string }{
		{"public.users", "pub"},
		{"audit.users", "aud"},
	} {
		row, err := db.Get(ctx, full, tc.addr, 1, nil)
		require.NoError(t, err)
		got, _ := row[1].String()
		require.Equal(t, tc.want, got, tc.addr)
	}

	// The same name may also coexist with a default-namespace table.
	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	tables, err = db.Tables(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, []string{"public.users", "audit.users", "users"}, tableAddresses(tables))
	row, err := db.Get(ctx, snap, "users", 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "the default-namespace table has no rows yet")
	_ = row

	// The same *address* with different columns is still a conflict.
	tx, err = db.Begin(ctx, snap)
	require.NoError(t, err)
	require.ErrorIs(t, tx.DefineTableIn("public", "users", []Column{{Name: "id", Type: TypeUint64}}), ErrSchemaConflict)
	require.NoError(t, tx.Rollback())

	// Redefining an existing address with the same columns stays a no-op.
	tx, err = db.Begin(ctx, snap)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("public", "users", nsCols()))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
}

// TestNamespaceAddressHelpers pins the address grammar.
func TestNamespaceAddressHelpers(t *testing.T) {
	require.Equal(t, "users", Qualify(NSUser, "users"))
	require.Equal(t, "users", Qualify("", "users"))
	require.Equal(t, "public.users", Qualify("public", "users"))

	ns, name := SplitAddress("users")
	require.Equal(t, NSUser, ns)
	require.Equal(t, "users", name)

	ns, name = SplitAddress("public.users")
	require.Equal(t, "public", ns)
	require.Equal(t, "users", name)
}

// TestNamespaceSeparatorIsNotForbidden: no character is forbidden in an ns or a
// name. A name may contain the separator because addresses resolve at the first
// one, and so may an ns.
func TestNamespaceSeparatorIsNotForbidden(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "sep"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	// A dotted name in the default ns, and a dotted name in a named ns.
	require.NoError(t, tx.DefineTable("a.b", nsCols()))
	require.NoError(t, tx.DefineTableIn("public", "order.items", nsCols()))
	// A dotted ns.
	require.NoError(t, tx.DefineTableIn("a.b", "c", nsCols()))
	require.NoError(t, tx.Insert("a.b", 1, nsRow(1, "default-ns a.b")))
	require.NoError(t, tx.Insert("public.order.items", 1, nsRow(1, "public dotted")))
	require.NoError(t, tx.Insert("a.b.c", 1, nsRow(1, "dotted ns")))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	tables, err := db.Tables(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, []string{"a.b", "public.order.items", "a.b.c"}, tableAddresses(tables))

	// Each address resolves to its own table.
	for _, tc := range []struct{ addr, want string }{
		{"a.b", "default-ns a.b"},
		{"public.order.items", "public dotted"},
		{"a.b.c", "dotted ns"},
	} {
		row, err := db.Get(ctx, snap, tc.addr, 1, nil)
		require.NoError(t, err)
		got, _ := row[1].String()
		require.Equal(t, tc.want, got, tc.addr)
	}

	// Table.NS / Table.Name are the authoritative decomposition; SplitAddress is
	// a convenience that only holds when the ns itself has no separator.
	for _, tbl := range tables {
		switch tbl.Address() {
		case "a.b":
			require.Equal(t, NSUser, tbl.NS)
			require.Equal(t, "a.b", tbl.Name)
		case "public.order.items":
			require.Equal(t, "public", tbl.NS)
			require.Equal(t, "order.items", tbl.Name)
		case "a.b.c":
			require.Equal(t, "a.b", tbl.NS)
			require.Equal(t, "c", tbl.Name)
		}
	}
}

// TestNamespaceAddressCollision: two different (ns, name) pairs can produce the
// same address. That is a real conflict, reported on the second definition --
// not a character restriction on the first.
func TestNamespaceAddressCollision(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "collide"), Options{})
	require.NoError(t, err)
	defer db.Close()

	// ns "a" + name "b" collides with NSUser + name "a.b".
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("a.b", nsCols()))
	require.ErrorIs(t, tx.DefineTableIn("a", "b", nsCols()), ErrSchemaConflict)
	require.NoError(t, tx.Rollback())

	// The other order reports it too, in one transaction and across a DELTA.
	tx, err = db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("a", "b", nsCols()))
	require.ErrorIs(t, tx.DefineTable("a.b", nsCols()), ErrSchemaConflict)
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.ErrorIs(t, tx.DefineTable("a.b", nsCols()), ErrSchemaConflict)
	require.NoError(t, tx.Rollback())

	// Redefining the same (ns, name) pair still resolves to the same table.
	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("a", "b", nsCols()))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
}

// TestNamespaceAcrossChain: a namespace declared by an ancestor is inherited,
// not redeclared, and survives a reopen.
func TestNamespaceAcrossChain(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "chain")

	db, err := Create(base, Options{})
	require.NoError(t, err)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("catalog", "_src_objects", nsCols()))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// A DELTA that adds a second table to the same namespace: the namespace
	// travels on each Table record, so no shared record has to be inherited.
	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("catalog", "_src_columns", nsCols()))
	delta, err := tx.Commit(ctx)
	require.NoError(t, err)

	tables, err := db.Tables(ctx, delta)
	require.NoError(t, err)
	require.Len(t, tables, 2)
	for _, tbl := range tables {
		require.Equal(t, "catalog", tbl.NS)
	}
	require.NoError(t, db.Close())

	// Reopen: namespaces are re-derived from the metadata records.
	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer db2.Close()
	tables, err = db2.Tables(ctx, delta)
	require.NoError(t, err)
	require.Len(t, tables, 2)
	for _, tbl := range tables {
		require.Equal(t, "catalog", tbl.NS)
	}
}

// TestNamespaceFullCheckpointRewritesOwnLayer: a FULL snapshot's metadata is not
// visible through any ancestor, so the checkpoint must carry its own namespace
// record and keep resolving the table's namespace.
func TestNamespaceFullCheckpointRewritesOwnLayer(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "checkpoint"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("catalog", "_src_columns", nsCols()))
	require.NoError(t, tx.Insert("catalog._src_columns", 1, nsRow(1, "c")))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	tx, err = db.Begin(ctx, NoParent) // FULL checkpoint, no parent
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("catalog", "_src_columns", nsCols()))
	require.NoError(t, tx.Insert("catalog._src_columns", 1, nsRow(1, "c")))
	check, err := tx.Commit(ctx)
	require.NoError(t, err)

	tables, err := db.Tables(ctx, check)
	require.NoError(t, err)
	require.Len(t, tables, 1)
	require.Equal(t, "catalog", tables[0].NS)

	// The pre-checkpoint snapshot still reports the same namespace.
	tables, err = db.Tables(ctx, full)
	require.NoError(t, err)
	require.Len(t, tables, 1)
	require.Equal(t, "catalog", tables[0].NS)
}

// TestMetadataIDHighWaterMarkSurvivesReopen is the regression guard for the
// recovery ID high-water mark: recovery must resume the allocator above every
// metadata ObjectID in the file, not just the ones owned by Table/Column
// records. Otherwise the next write re-issues an ID that another record already
// owns, and because the metadata index is keyed by (SnapshotID, ObjectID) the
// two records alias each other in the new snapshot's layer.
func TestMetadataIDHighWaterMarkSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "ids")

	db, err := Create(base, Options{})
	require.NoError(t, err)

	// Snapshot 1: a table, so its column objects occupy low non-table IDs.
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// Snapshot 2: a foreign record type carrying a much higher ObjectID. The
	// engine preserves unknown record types, so its ID must still bound the
	// allocator after a reopen.
	const foreignOID = uint64(1) << 40
	tx, err = db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, tx.w.writeMetadata(&metadata.Record{
		RecordType: 99, // unknown to the engine
		ObjectID:   foreignOID,
		Revision:   1,
		Namespace:  fileformat.NamespaceCore,
		Fields: []metadata.Field{
			{ID: 1, WireType: fileformat.WireString, Value: "kept verbatim"},
		},
	}))
	snap2, err := tx.Commit(ctx)
	require.NoError(t, err)

	require.Equal(t, foreignOID, maxMetadataObject(db.state.Load().view, uint64(snap2)))
	require.NoError(t, db.Close())

	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer db2.Close()

	// Snapshot 3, after reopen: new Column records must not reuse an ID that the
	// foreign record already owns.
	tx, err = db2.Begin(ctx, snap2)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTableIn("public", "orders", nsCols()))
	snap3, err := tx.Commit(ctx)
	require.NoError(t, err)

	for _, id := range db2.state.Load().view.MetadataByType(uint64(snap3), uint32(fileformat.RecordColumn)) {
		require.Greater(t, id, foreignOID, "ObjectID was re-issued after reopen")
	}

	// The foreign record survived the round-trip and did not break the store.
	tables, err := db2.Tables(ctx, snap3)
	require.NoError(t, err)
	require.Equal(t, []string{"users", "public.orders"}, tableAddresses(tables))
}

func maxMetadataObject(view *index.View, snap uint64) uint64 {
	var highest uint64
	for _, oid := range view.MetadataObjects(snap) {
		if oid > highest {
			highest = oid
		}
	}
	return highest
}

// TestNamespaceDuplicateAddressReportedByVerify: two Table records may claim one
// address (only a writer outside the engine can produce that). Lookup must stay
// deterministic instead of depending on map order, and Verify must report it.
func TestNamespaceDuplicateAddressReportedByVerify(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "dupaddr")
	db, err := Create(base, Options{})
	require.NoError(t, err)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("a.b", nsCols()))

	// Forge the records a foreign writer would produce for ns "a" + name "b":
	// the same address as the table above, with its own ObjectIDs.
	const forgedTableID = 100
	require.NoError(t, tx.w.writeMetadata(&metadata.Record{
		RecordType: uint32(fileformat.RecordTable), ObjectID: forgedTableID, Revision: 1,
		Namespace: fileformat.NamespaceCore, ExternalKey: "b",
		Fields: []metadata.Field{
			{ID: metadata.TableName, WireType: fileformat.WireString, Value: "b"},
			{ID: metadata.TableNS, WireType: fileformat.WireString, Value: "a"},
		},
	}))
	require.NoError(t, tx.w.writeMetadata(&metadata.Record{
		RecordType: uint32(fileformat.RecordColumn), ObjectID: 1 << 40, ParentID: forgedTableID, Revision: 1,
		Namespace: fileformat.NamespaceCore,
		Fields: []metadata.Field{
			{ID: metadata.ColColumnID, WireType: fileformat.WireSint, Value: int64(1)},
			{ID: metadata.ColColumnName, WireType: fileformat.WireString, Value: "id"},
			{ID: metadata.ColColumnType, WireType: fileformat.WireString, Value: "uint64"},
			{ID: metadata.ColNullable, WireType: fileformat.WireString, Value: "NO"},
			{ID: metadata.ColDataScale, WireType: fileformat.WireSint, Value: int64(0)},
		},
	}))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer db2.Close()

	tables, err := db2.Tables(ctx, snap)
	require.NoError(t, err)
	require.Len(t, tables, 2)
	require.Equal(t, []string{"a.b", "a.b"}, tableAddresses(tables))

	// Lookup resolves to the lowest TableID deterministically, not by map order.
	_, err = db2.Get(ctx, snap, "a.b", 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "neither table has rows")

	_, err = db2.Verify(ctx, VerifyQuick)
	require.ErrorIs(t, err, ErrCorruptIndex)
	require.Contains(t, err.Error(), "claimed by tables")
}

// TestDefineTableIdempotentAfterWriteInDelta writes to a chain table first
// (tableForWrite caches the address without a txn schema) and only then calls
// DefineTable: the re-definition stays a no-op, and the DELTA still resolves
// the table and both rows through the chain.
func TestDefineTableIdempotentAfterWriteInDelta(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "define-after-write"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	require.NoError(t, tx.Insert("users", 1, nsRow(1, "a")))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	d, err := db.Begin(ctx, full)
	require.NoError(t, err)
	defer d.Rollback()
	require.NoError(t, d.Insert("users", 2, nsRow(2, "b")))
	require.NoError(t, d.DefineTable("users", nsCols()),
		"DefineTable after a write must stay an idempotent no-op")
	delta, err := d.Commit(ctx)
	require.NoError(t, err)

	// The DELTA carries no schema of its own; the chain resolves the table and
	// both rows.
	tables, err := db.Tables(ctx, delta)
	require.NoError(t, err)
	require.Equal(t, []string{"users"}, tableAddresses(tables))
	row, err := db.Get(ctx, delta, "users", 1, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "a", name)
	row, err = db.Get(ctx, delta, "users", 2, nil)
	require.NoError(t, err)
	name, _ = row[1].String()
	require.Equal(t, "b", name)
}

// TestDefineTableTwiceInDelta calls DefineTable twice on a chain table inside
// one DELTA: both resolve idempotently through the chain without writing
// metadata.
func TestDefineTableTwiceInDelta(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "define-twice"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	d, err := db.Begin(ctx, full)
	require.NoError(t, err)
	defer d.Rollback()
	require.NoError(t, d.DefineTable("users", nsCols()))
	require.NoError(t, d.DefineTable("users", nsCols()),
		"second DefineTable with identical columns must be a no-op")
	delta, err := d.Commit(ctx)
	require.NoError(t, err)

	// No txn schema was written; the chain still resolves the table. It has no
	// rows yet.
	tables, err := db.Tables(ctx, delta)
	require.NoError(t, err)
	require.Equal(t, []string{"users"}, tableAddresses(tables))
	_, err = db.Get(ctx, delta, "users", 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "the table has no rows yet")
}

// TestDefineTableIdempotentAfterWriteInFull is the same order in a FULL
// checkpoint, whose metadata is not visible through any ancestor: the
// re-definition must still write the snapshot's own layer, or the table and the
// row written before it would be unreachable.
func TestDefineTableIdempotentAfterWriteInFull(t *testing.T) {
	ctx := context.Background()
	db, err := Create(filepath.Join(tmpdb(t), "define-after-write-full"), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("users", nsCols()))
	require.NoError(t, tx.Insert("users", 1, nsRow(1, "a")))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	ck, err := db.Begin(ctx, NoParent) // FULL checkpoint, no parent
	require.NoError(t, err)
	defer ck.Rollback()
	require.NoError(t, ck.Insert("users", 2, nsRow(2, "b")))
	require.NoError(t, ck.DefineTable("users", nsCols()),
		"a FULL re-definition must write the checkpoint's own metadata layer")
	check, err := ck.Commit(ctx)
	require.NoError(t, err)

	tables, err := db.Tables(ctx, check)
	require.NoError(t, err)
	require.Equal(t, []string{"users"}, tableAddresses(tables))
	row, err := db.Get(ctx, check, "users", 2, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "b", name)
	// A complete baseline: the previous snapshot's row is not inherited.
	_, err = db.Get(ctx, check, "users", 1, nil)
	require.ErrorIs(t, err, ErrNotFound)
}
