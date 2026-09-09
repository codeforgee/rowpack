package rowpack

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"
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

func insertUsers(t testing.TB, w *Writer, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		require.NoError(t, w.Insert(context.Background(), "users", uint64(i), Row{
			Uint64(uint64(i)), String(fmt.Sprintf("user-%d", i)), Bool(i%2 == 0),
			DecimalValue(Decimal{Unscaled: big.NewInt(int64(i * 100)), Scale: 2}),
		}))
	}
}

// TestCreateOpenLifecycle covers the store lifecycle: Create rejects .rpk
// paths and existing files, Open of a missing store fails with ErrNotFound.
func TestCreateOpenLifecycle(t *testing.T) {
	base := filepath.Join(tmpdb(t), "lf")
	// Extension suffixes on the base path are rejected (R21).
	_, err := Create(base+".rpk", Options{})
	require.ErrorIs(t, err, ErrInvalidPath)

	db, err := Create(base, Options{})
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

	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	require.NoError(t, w.CreateTable("events", []Column{{Name: "seq", Type: TypeUint64}}))
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
	d, err := db.BeginDelta(ctx, full)
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
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
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
	blks, err := db.Blocks(ctx, full, "users")
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
	require.Equal(t, 100, n, "raw block stream emits every record")
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

// TestWriterErrors covers the error sentinels on the write path.
func TestWriterErrors(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()

	w, _ := db.BeginFull(ctx)
	// Argument validation.
	require.ErrorIs(t, w.Insert(ctx, "users", 0, Row{Uint64(0)}), ErrInvalidArgument)
	require.ErrorIs(t, w.Delete(ctx, "users", 0), ErrInvalidArgument)
	require.ErrorIs(t, w.CreateTable("", nil), ErrInvalidArgument)
	require.Error(t, w.CreateTable("x", []Column{{Name: "a", Type: TypeUint64}, {Name: "a", Type: TypeUint64}}), "duplicate column names must be rejected")

	// FULL snapshots reject UPDATE/DELETE.
	require.NoError(t, w.CreateTable("users", usersSchema()))
	require.ErrorIs(t, w.Update(ctx, "users", 1, Row{}), ErrInvalidArgument)
	require.ErrorIs(t, w.Delete(ctx, "users", 1), ErrInvalidArgument)

	// Unknown table.
	require.ErrorIs(t, w.Insert(ctx, "ghost", 1, Row{Uint64(1)}), ErrNotFound)

	// Duplicate row within the snapshot.
	require.NoError(t, w.Insert(ctx, "users", 1, Row{Uint64(1), String("a"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(1), Scale: 2})}))
	require.ErrorIs(t, w.Insert(ctx, "users", 1, Row{}), ErrAlreadyExists)

	// CreateTable idempotence vs conflict.
	require.NoError(t, w.CreateTable("users", usersSchema()), "identical redefinition is a no-op")
	require.ErrorIs(t, w.CreateTable("users", []Column{{Name: "id", Type: TypeUint64}}), ErrSchemaConflict)
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	// After commit, the writer rejects further use.
	require.ErrorIs(t, w.Insert(ctx, "users", 2, Row{}), ErrSnapshotCommitted)
	_, err = w.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotCommitted)
	require.ErrorIs(t, w.Abort(), ErrSnapshotCommitted)

	// Abort is idempotent and frees the writer slot.
	w2, _ := db.BeginFull(ctx)
	require.NoError(t, w2.Abort())
	require.NoError(t, w2.Abort())

	// Empty FULL cannot be committed; a failed writer must be aborted to
	// release the single-writer slot (like a rolled-back transaction).
	wEmpty, _ := db.BeginFull(ctx)
	_, err = wEmpty.Commit(ctx)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.NoError(t, wEmpty.Abort())

	// BeginDelta requires a committed parent.
	_, err = db.BeginDelta(ctx, 0)
	require.ErrorIs(t, err, ErrInvalidParent)
	_, err = db.BeginDelta(ctx, 9999)
	require.ErrorIs(t, err, ErrInvalidParent)

	// Single active writer.
	w3, _ := db.BeginFull(ctx)
	require.NotNil(t, w3)
	_, err = db.BeginFull(ctx)
	require.ErrorIs(t, err, ErrWriterBusy)
	_, err = db.BeginDelta(ctx, full)
	require.ErrorIs(t, err, ErrWriterBusy)
	require.NoError(t, w3.Abort())

	// Strict parent check: DELTA insert of an existing row, update of a
	// missing row.
	d, err := db.BeginDelta(ctx, full)
	require.NoError(t, err)
	validRow := Row{Uint64(1), String("x"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2})}
	require.ErrorIs(t, d.Insert(ctx, "users", 1, validRow), ErrAlreadyExists, "strict: row exists in parent")
	require.ErrorIs(t, d.Update(ctx, "users", 500, validRow), ErrNotFound, "strict: row missing in parent")
	require.ErrorIs(t, d.Delete(ctx, "users", 500), ErrNotFound)
	require.NoError(t, d.Abort())

	// Read-only store rejects writes.
	ro, err := Open(db.Path(), Options{ReadOnly: true, BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { ro.Close() })
	_, err = ro.BeginFull(ctx)
	require.ErrorIs(t, err, ErrReadOnly)
}

// TestValidationNone skips the strict parent existence check.
func TestValidationNone(t *testing.T) {
	db := testDB(t, Options{Validation: ValidationNone})
	ctx := context.Background()
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	insertUsers(t, w, 5)
	full, _ := w.Commit(ctx)
	d, _ := db.BeginDelta(ctx, full)
	// Strict would reject; ValidationNone allows the stale-write pattern.
	require.NoError(t, d.Insert(ctx, "users", 1, Row{Uint64(1), String("dup"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2})}))
	require.NoError(t, d.Update(ctx, "users", 500, Row{Uint64(500), String("x"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2})}))
	_, err := d.Commit(ctx)
	require.NoError(t, err)
}

// TestSchemaVersioning verifies FULL checkpoint semantics: a FULL on top of
// an existing chain rewrites its own metadata layer (R7).
func TestSchemaVersioning(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	insertUsers(t, w, 3)
	full1, _ := w.Commit(ctx)

	d, _ := db.BeginDelta(ctx, full1)
	require.NoError(t, d.Insert(ctx, "users", 4, Row{Uint64(4), String("u4"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(4), Scale: 2})}))
	delta, _ := d.Commit(ctx)

	// New FULL checkpoint: same schema is re-emitted for its own layer.
	w2, _ := db.BeginFull(ctx)
	require.NoError(t, w2.CreateTable("users", usersSchema()))
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
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	insertUsers(t, w, 10)
	full, _ := w.Commit(ctx)
	d, _ := db.BeginDelta(ctx, full)
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
	require.Equal(t, 2026, back.Year())
	require.Equal(t, 8, int(back.Month()))
	require.Equal(t, 9, back.Day())

	tod, err := NewTimeOfDay(13, 45, 30, 123456789)
	require.NoError(t, err)
	require.Equal(t, 13, tod.Hour())
	require.Equal(t, 45, tod.Minute())
	require.Equal(t, 30, tod.Second())
	require.Equal(t, 123456789, tod.Nanosecond())
	_, err = NewTimeOfDay(24, 0, 0, 0)
	require.Error(t, err)
	_, err = NewTimeOfDay(0, 0, 0, -1)
	require.Error(t, err)

	// DateTime accessor round trip through Value.
	v := DateTime(now)
	got, _ := v.DateTimeValue()
	require.True(t, got.Equal(now.UTC()), "datetime value keeps precision")
}
