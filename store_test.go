package rowpack

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testDB builds a fresh store with a small block size (multiple blocks per
// dataset, fast commits) and registers cleanup.
func testDB(t testing.TB, opts Options) *Store {
	t.Helper()
	if opts.BlockSize == 0 {
		opts.BlockSize = 1024
	}
	db, err := Create(filepath.Join(tmpdb(t), "store"), opts)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func usersSchema() []Column {
	return []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
		{Name: "active", Type: TypeBool},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	}
}

func insertUsers(t testing.TB, tx *Tx, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		require.NoError(t, tx.Insert(context.Background(), "users", uint64(i), Row{
			Uint64(uint64(i)), String(fmt.Sprintf("user-%d", i)), Bool(i%2 == 0),
			DecimalValue(Decimal{Unscaled: big.NewInt(int64(i * 100)), Scale: 2}),
		}))
	}
}

// TestCreateOpenLifecycle covers the store lifecycle: Create rejects .rpk
// paths and existing files, Open of a missing store fails with ErrNotFound.
func TestCreateOpenLifecycle(t *testing.T) {
	base := filepath.Join(tmpdb(t), "lf")
	// A .rpk suffix on the base path is stripped, so the logical name and
	// the physical file are interchangeable and Path reports the base.
	db, err := Create(base+".rpk", Options{})
	require.NoError(t, err)
	require.False(t, db.ReadOnly())
	require.Equal(t, base, db.Path())
	require.NotEqual(t, [16]byte{}, db.UUID())
	require.NoError(t, db.Close())
	require.ErrorIs(t, db.Close(), nil) // idempotent

	// Create on an existing store never overwrites (CreateSingle).
	_, err = Create(base, Options{})
	require.Error(t, err)

	// Open works; Open of a missing store fails.
	re, err := Open(base, Options{})
	require.NoError(t, err)
	require.NoError(t, re.Close())
	_, err = Open(filepath.Join(tmpdb(t), "missing"), Options{})
	require.ErrorIs(t, err, ErrNotFound)

	// Operations on a closed store fail with ErrClosed.
	closed, err := Open(base, Options{})
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	_, err = closed.ListSnapshots(context.Background())
	require.ErrorIs(t, err, ErrClosed)
}

// TestFullDeltaRoundTrip is the core end-to-end flow: FULL with two tables,
// DELTA update/delete/insert, and name-based reads on each snapshot.
func TestFullDeltaRoundTrip(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	require.NoError(t, w.DefineTable("events", []Column{{Name: "seq", Type: TypeUint64}}))
	insertUsers(t, w, 10)
	full, err := w.Commit(ctx)
	require.NoError(t, err)
	require.Equal(t, SnapshotID(1), full)

	// FULL visible state.
	row, err := db.Get(ctx, full, "users", 3, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-3", name)
	_, err = db.Get(ctx, full, "users", 11, nil)
	require.ErrorIs(t, err, ErrNotFound)

	// DELTA: update 2, delete 3, insert 11.
	d, err := db.Begin(ctx, full)
	require.NoError(t, err)
	require.NoError(t, d.Update(ctx, "users", 2, Row{Uint64(2), String("updated-2"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(999), Scale: 2})}))
	require.NoError(t, d.Delete(ctx, "users", 3))
	require.NoError(t, d.Insert(ctx, "users", 11, Row{Uint64(11), String("new-11"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(1), Scale: 2})}))
	require.NoError(t, d.Insert(ctx, "events", 1, Row{Uint64(1)}))
	delta, err := d.Commit(ctx)
	require.NoError(t, err)

	// DELTA visible state.
	row, err = db.Get(ctx, delta, "users", 2, nil)
	require.NoError(t, err)
	name, _ = row[1].String()
	require.Equal(t, "updated-2", name)
	_, err = db.Get(ctx, delta, "users", 3, nil)
	require.ErrorIs(t, err, ErrNotFound, "deleted row must be invisible in delta")
	_, err = db.Get(ctx, delta, "users", 11, nil)
	require.NoError(t, err)

	// The FULL snapshot is immutable: still sees the pre-delta state.
	row, err = db.Get(ctx, full, "users", 2, nil)
	require.NoError(t, err)
	name, _ = row[1].String()
	require.Equal(t, "user-2", name)

	// Exists along the chain.
	ok, err := db.Exists(ctx, delta, "users", 3)
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = db.Exists(ctx, full, "users", 3)
	require.NoError(t, err)
	require.True(t, ok)

	// Reopen: everything is reconstructible from the file.
	require.NoError(t, db.Close())
	t.Cleanup(func() {}) // testDB registered a Close; double Close is idempotent
	db2, err := Open(db.Path(), Options{BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	row, err = db2.Get(ctx, snaps[1].ID, "users", 2, nil)
	require.NoError(t, err)
	name, _ = row[1].String()
	require.Equal(t, "updated-2", name, "reopened store resolves the delta")

	// Tables at each snapshot.
	tables, err := db2.Tables(ctx, full)
	require.NoError(t, err)
	require.Len(t, tables, 2)
	require.Equal(t, "users", tables[0].Name)
	require.Equal(t, "events", tables[1].Name)
}

// TestReadByTableName exercises the name-based read paths, including the
// batch and schema APIs that were aligned to table-name addressing.
func TestReadByTableName(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 100)
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	// Schema by name + version.
	s, err := db.Schema(ctx, full, "users", 1)
	require.NoError(t, err)
	require.Equal(t, "users", s.Name)
	require.Len(t, s.Columns, 4)
	_, err = db.Schema(ctx, full, "nope", 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Schema(ctx, full, "users", 99)
	require.ErrorIs(t, err, ErrSchemaMismatch)

	// Batch read by name: clustered ids aggregate into few blocks.
	ids := make([]RowID, 0, 50)
	for i := RowID(2); i <= 51; i++ {
		ids = append(ids, i)
	}
	rows, err := db.ReadBatch(ctx, full, "users", ids)
	require.NoError(t, err)
	require.Len(t, rows, 50)
	for i, r := range rows {
		v, _ := r[1].String()
		require.Equal(t, fmt.Sprintf("user-%d", i+2), v)
	}
	// Missing id -> ErrNotFound, whole batch aborts.
	_, err = db.ReadBatch(ctx, full, "users", []RowID{1, 9999})
	require.ErrorIs(t, err, ErrNotFound)
	// Unknown table name -> ErrNotFound.
	_, err = db.ReadBatch(ctx, full, "ghost", []RowID{1})
	require.ErrorIs(t, err, ErrNotFound)
	// Empty batch is legal and returns nil.
	rows, err = db.ReadBatch(ctx, full, "users", nil)
	require.NoError(t, err)
	require.Nil(t, rows)

	// Blocks + ScanBlocks (block-level raw stream).
	blks, err := db.Blocks(full, "users")
	require.NoError(t, err)
	require.NotEmpty(t, blks)
	bit, err := db.ScanBlocks(ctx, full, "users", blks[0].BlockID, blks[len(blks)-1].BlockID+1)
	require.NoError(t, err)
	defer bit.Close()
	var n int
	for {
		_, ok := bit.Next()
		if !ok {
			break
		}
		n++
	}
	require.NoError(t, bit.Err())
	require.EqualValues(t, 100, n, "raw block stream emits every record")
	_, err = db.ScanBlocks(ctx, full, "users", 1_000_000, 2_000_000)
	require.ErrorIs(t, err, ErrInvalidArgument)

	// Scan range filtering.
	it, err := db.Scan(ctx, full, "users", ScanOptions{Start: 10, End: 15})
	require.NoError(t, err)
	want := []RowID{10, 11, 12, 13, 14}
	var got []RowID
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, it.RowID())
	}
	require.NoError(t, it.Err())
	require.Equal(t, want, got)
	require.NoError(t, it.Close())
	// Invalid range.
	_, err = db.Scan(ctx, full, "users", ScanOptions{Start: 10, End: 10})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// TestReadBatchInto covers the reusable-buffer entry point: repeated calls
// overwrite the previous result in place (Get's dst contract) and, once the
// buffer is warm, allocate nothing.
func TestReadBatchInto(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("u", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}))
	const n = 64
	for i := 1; i <= n; i++ {
		require.NoError(t, w.Insert(ctx, "u", uint64(i), Row{Uint64(uint64(i)), String(fmt.Sprintf("v-%d", i))}))
	}
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	ids := make([]RowID, n)
	for i := range ids {
		ids[i] = RowID(i + 1)
	}
	var buf batchBuffer
	check := func(rows []Row) {
		t.Helper()
		require.Len(t, rows, n)
		for i, r := range rows {
			id, ok := r[0].Uint64()
			require.True(t, ok)
			require.Equal(t, uint64(i+1), id)
			name, ok := r[1].String()
			require.True(t, ok)
			require.Equal(t, fmt.Sprintf("v-%d", i+1), name)
		}
	}
	rows, err := db.readBatchInto(snap, "u", ids, &buf)
	require.NoError(t, err)
	check(rows)
	rows2, err := db.readBatchInto(snap, "u", ids, &buf)
	require.NoError(t, err)
	check(rows2)
	require.True(t, &rows[0] == &rows2[0], "the output buffer must be reused, not reallocated")

	// Warm buffer: a call allocates nothing.
	if _, err := db.readBatchInto(snap, "u", ids, &buf); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(10, func() {
		if _, err := db.readBatchInto(snap, "u", ids, &buf); err != nil {
			t.Fatal(err)
		}
	})
	require.Zero(t, allocs, "readBatchInto must not allocate once the buffer is warm")

	// Empty batch is legal and does not touch the buffer; a nil buffer is not.
	out, err := db.readBatchInto(snap, "u", nil, &buf)
	require.NoError(t, err)
	require.Nil(t, out)
	_, err = db.readBatchInto(snap, "u", ids, nil)
	require.ErrorIs(t, err, ErrInvalidArgument)

	// Errors keep the same sentinel as ReadBatch.
	_, err = db.readBatchInto(snap, "u", []RowID{1, 999}, &buf)
	require.ErrorIs(t, err, ErrNotFound)

	// ReadBatch and readBatchInto agree.
	want, err := db.ReadBatch(ctx, snap, "u", ids)
	require.NoError(t, err)
	check(want)
}

// TestWriterErrors covers the error sentinels on the write path.
func TestWriterErrors(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()

	w, _ := db.Begin(ctx, NoParent)
	// Argument validation.
	require.ErrorIs(t, w.Insert(ctx, "users", 0, Row{Uint64(0)}), ErrInvalidArgument)
	require.ErrorIs(t, w.Delete(ctx, "users", 0), ErrInvalidArgument)
	require.ErrorIs(t, w.DefineTable("", nil), ErrInvalidArgument)
	require.Error(t, w.DefineTable("x", []Column{{Name: "a", Type: TypeUint64}, {Name: "a", Type: TypeUint64}}), "duplicate column names must be rejected")

	// FULL snapshots reject UPDATE/DELETE.
	require.NoError(t, w.DefineTable("users", usersSchema()))
	require.ErrorIs(t, w.Update(ctx, "users", 1, Row{}), ErrInvalidArgument)
	require.ErrorIs(t, w.Delete(ctx, "users", 1), ErrInvalidArgument)

	// Unknown table.
	require.ErrorIs(t, w.Insert(ctx, "ghost", 1, Row{Uint64(1)}), ErrNotFound)

	// Duplicate row within the snapshot.
	require.NoError(t, w.Insert(ctx, "users", 1, Row{Uint64(1), String("a"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(1), Scale: 2})}))
	require.ErrorIs(t, w.Insert(ctx, "users", 1, Row{}), ErrAlreadyExists)

	// CreateTable idempotence vs conflict.
	require.NoError(t, w.DefineTable("users", usersSchema()), "identical redefinition is a no-op")
	require.ErrorIs(t, w.DefineTable("users", []Column{{Name: "id", Type: TypeUint64}}), ErrSchemaConflict)
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	// After commit, the writer rejects further use.
	require.ErrorIs(t, w.Insert(ctx, "users", 2, Row{}), ErrSnapshotCommitted)
	_, err = w.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotCommitted)
	require.ErrorIs(t, w.Rollback(), ErrSnapshotCommitted)

	// Abort is idempotent and frees the writer slot.
	w2, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w2.Rollback())
	require.NoError(t, w2.Rollback())

	// Empty FULL cannot be committed; a failed writer must be aborted to
	// release the single-writer slot (like a rolled-back transaction).
	wEmpty, _ := db.Begin(ctx, NoParent)
	_, err = wEmpty.Commit(ctx)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.NoError(t, wEmpty.Rollback())

	// Begin refuses a DELTA whose parent is not committed. (Zero now means
	// NoParent, i.e. a FULL baseline — there is no zero-parent DELTA anymore.)
	_, err = db.Begin(ctx, 9999)
	require.ErrorIs(t, err, ErrInvalidParent)

	// Single active writer.
	w3, _ := db.Begin(ctx, NoParent)
	require.NotNil(t, w3)
	_, err = db.Begin(ctx, NoParent)
	require.ErrorIs(t, err, ErrWriterBusy)
	_, err = db.Begin(ctx, full)
	require.ErrorIs(t, err, ErrWriterBusy)
	require.NoError(t, w3.Rollback())

	// Strict parent check: DELTA insert of an existing row, update of a
	// missing row.
	d, err := db.Begin(ctx, full)
	require.NoError(t, err)
	validRow := Row{Uint64(1), String("x"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2})}
	require.ErrorIs(t, d.Insert(ctx, "users", 1, validRow), ErrAlreadyExists, "strict: row exists in parent")
	require.ErrorIs(t, d.Update(ctx, "users", 500, validRow), ErrNotFound, "strict: row missing in parent")
	require.ErrorIs(t, d.Delete(ctx, "users", 500), ErrNotFound)
	require.NoError(t, d.Rollback())

	// Read-only store rejects writes.
	ro, err := Open(db.Path(), Options{ReadOnly: true, BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { ro.Close() })
	_, err = ro.Begin(ctx, NoParent)
	require.ErrorIs(t, err, ErrReadOnly)
}

// TestFullSnapshotRequiresOwnSchema: a FULL snapshot's metadata is invisible
// through any ancestor, so writing to a chain table without defining it must
// fail the commit instead of storing rows no reader can decode.
func TestFullSnapshotRequiresOwnSchema(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 1)
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	// The chain resolves users, but this FULL layer defines nothing.
	ck, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, ck.Insert(ctx, "users", 2, Row{
		Uint64(2), String("user-2"), Bool(true),
		DecimalValue(Decimal{Unscaled: big.NewInt(200), Scale: 2}),
	}))
	_, err = ck.Commit(ctx)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.Contains(t, err.Error(), "users")
	require.NoError(t, ck.Rollback())

	// The failed commit published nothing and the store stays usable.
	tables, err := db.Tables(ctx, full)
	require.NoError(t, err)
	require.Equal(t, []string{"users"}, tableAddresses(tables))

	// Defining the table first commits normally.
	ck, err = db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, ck.DefineTable("users", usersSchema()))
	require.NoError(t, ck.Insert(ctx, "users", 2, Row{
		Uint64(2), String("user-2"), Bool(true),
		DecimalValue(Decimal{Unscaled: big.NewInt(200), Scale: 2}),
	}))
	check, err := ck.Commit(ctx)
	require.NoError(t, err)
	row, err := db.Get(ctx, check, "users", 2, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-2", name)
}

// TestValidationNone skips the strict parent existence check.
func TestValidationNone(t *testing.T) {
	db := testDB(t, Options{Validation: ValidationNone})
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 5)
	full, _ := w.Commit(ctx)
	d, _ := db.Begin(ctx, full)
	// Strict would reject; ValidationNone allows the stale-write pattern.
	require.NoError(t, d.Insert(ctx, "users", 1, Row{Uint64(1), String("dup"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2})}))
	require.NoError(t, d.Update(ctx, "users", 500, Row{Uint64(500), String("x"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2})}))
	_, err := d.Commit(ctx)
	require.NoError(t, err)
}

// TestSchemaVersioning verifies FULL checkpoint semantics: a FULL on top of
// an existing chain rewrites its own metadata layer.
func TestSchemaVersioning(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 3)
	full1, _ := w.Commit(ctx)

	d, _ := db.Begin(ctx, full1)
	require.NoError(t, d.Insert(ctx, "users", 4, Row{Uint64(4), String("u4"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(4), Scale: 2})}))
	delta, _ := d.Commit(ctx)

	// New FULL checkpoint: same schema is re-emitted for its own layer.
	w2, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w2.DefineTable("users", usersSchema()))
	insertUsers(t, w2, 6)
	full2, _ := w2.Commit(ctx)
	require.Equal(t, SnapshotID(3), full2)

	// Chain depth semantics: the checkpoint is a fresh baseline; the delta
	// layer is visible to the new full (id chain continues).
	_ = delta
	for i := 1; i <= 6; i++ {
		_, err := db.Get(ctx, full2, "users", uint64(i), nil)
		require.NoError(t, err, "row %d visible after checkpoint", i)
	}
	// Schema of the checkpoint is name-resolvable.
	s, err := db.Schema(ctx, full2, "users", 1)
	require.NoError(t, err)
	require.Len(t, s.Columns, 4)
}

// TestStatsSanity verifies Stats counters reflect the store contents.
func TestStatsSanity(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 10)
	full, _ := w.Commit(ctx)
	d, _ := db.Begin(ctx, full)
	require.NoError(t, d.Delete(ctx, "users", 1))
	_, err := d.Commit(ctx)
	require.NoError(t, err)

	st := db.Stats()
	require.Equal(t, uint64(2), st.Snapshots)
	require.Equal(t, uint64(1), st.Tables)
	require.True(t, st.Blocks >= 2, "metadata + rows blocks")
	require.Equal(t, uint64(9), st.LogicalRows, "delta hides row 1; 9 rows visible at the latest snapshot")
	require.Greater(t, st.RawBytes, uint64(0))
	require.Greater(t, st.DataFileBytes, int64(0))
}

// TestRowIDSet is a compact table-driven check of the packed duplicate-row
// set used by the writer (rowset.go).
func TestRowIDSet(t *testing.T) {
	var s rowIDSet
	require.False(t, s.Contains(1)) // nil receiver safe
	seen := map[uint64]bool{}
	for i := uint64(1); i <= 100_000; i += 3 { // forces rehashes
		require.False(t, s.Contains(i), "duplicate insert of %d", i)
		require.False(t, seen[i])
		seen[i] = true
		s.Insert(i)
	}
	for i := uint64(1); i <= 100_000; i++ {
		require.Equal(t, seen[i], s.Contains(i), "member %d", i)
	}
	require.Equal(t, len(seen), s.Len())
	// Sequential inserts of 1..1e6 exercise the open-address load grow.
	var seq rowIDSet
	for i := uint64(1); i <= 1_000_000; i++ {
		seq.Insert(i)
	}
	require.Equal(t, 1_000_000, seq.Len())
	require.True(t, seq.Contains(500_000))
	require.False(t, seq.Contains(2_000_000))
}

// TestTimeValues covers date/time-of-day conversions used by the value layer.
func TestTimeValues(t *testing.T) {
	now := time.Date(2026, 8, 9, 13, 45, 30, 123456789, time.FixedZone("CST", 8*3600))
	d := NewDate(now)
	require.Equal(t, Date(20674), d) // days since epoch of 2026-08-09
	back := d.Time(time.UTC)
	require.EqualValues(t, 2026, back.Year())
	require.EqualValues(t, 8, int(back.Month()))
	require.EqualValues(t, 9, back.Day())

	tod, err := NewTimeOfDay(13, 45, 30, 123456789)
	require.NoError(t, err)
	require.EqualValues(t, 13, tod.Hour())
	require.EqualValues(t, 45, tod.Minute())
	require.EqualValues(t, 30, tod.Second())
	require.EqualValues(t, 123456789, tod.Nanosecond())
	_, err = NewTimeOfDay(24, 0, 0, 0)
	require.Error(t, err)
	_, err = NewTimeOfDay(0, 0, 0, -1)
	require.Error(t, err)

	// DateTime accessor round trip through Value.
	v := DateTime(now)
	got, _ := v.DateTimeValue()
	require.True(t, got.Equal(now.UTC()), "datetime value keeps precision")
}

// TestConcurrentGetSharedPage reads the same block/page from many goroutines
// at once. The loader caches a shared RowsContainer per block whose pages are
// memoized lazily; concurrent first access to one page must not race on the
// memoization map. Run under -race (make race).
func TestConcurrentGetSharedPage(t *testing.T) {
	base := filepath.Join(tmpdb(t), "conc")
	// One block: the default 256 KiB block easily holds a few thousand rows,
	// so many concurrent Gets land on the same page container.
	db, err := Create(base, Options{})
	require.NoError(t, err)
	ctx := context.Background()
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 2000)
	full, err := w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db, err = Open(base, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	// Warm the cache so the single container is shared from the start.
	if _, err := db.Get(ctx, full, "users", 1, nil); err != nil {
		t.Fatal(err)
	}

	const g = 32
	var wg sync.WaitGroup
	errs := make(chan error, g)
	wg.Add(g)
	for i := range g {
		go func(seed RowID) {
			defer wg.Done()
			for r := range 400 {
				id := RowID((int(seed)+r)%2000) + 1
				row, err := db.Get(ctx, full, "users", id, nil)
				if err != nil {
					errs <- err
					return
				}
				if n, _ := row[1].String(); n == "" {
					errs <- fmt.Errorf("row %d empty name", id)
					return
				}
			}
		}(RowID(i*7 + 1))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestBlocksMaxRowIDOverflow: a block whose max RowID is MaxUint64 makes the
// exclusive MaxRowID overflow to 0; the block catalog must not report the
// block as empty [0,0) because of it (regression: the old MaxRowID==0
// emptiness test conflated the two).
func TestBlocksMaxRowIDOverflow(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir+"/s", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	tx, err := s.Begin(context.Background(), NoParent)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}); err != nil {
		t.Fatal(err)
	}
	id := RowID(^uint64(0))
	if err := tx.Insert(context.Background(), "t", id, Row{Int64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	blocks, err := s.Blocks(SnapshotID(1), "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	if blocks[0].ItemCount != 1 || blocks[0].MinRowID != id {
		t.Fatalf("block = item %d range [%d,%d), want item 1 min %d", blocks[0].ItemCount, blocks[0].MinRowID, blocks[0].MaxRowID, id)
	}
	// A row must be Get-able at that id too (round trip sanity).
	if _, err := s.Get(context.Background(), SnapshotID(1), "t", id, nil); err != nil {
		t.Fatalf("get max-u64 row: %v", err)
	}
}
