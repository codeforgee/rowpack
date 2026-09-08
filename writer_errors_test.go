package rowpack

import (
	"context"
	"testing"

	"github.com/rowpack/rowpack/internal/fault"
	"github.com/stretchr/testify/require"
)

func testSchema() Schema {
	return Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString},
	}}
}

func newEmptyStore(t *testing.T) *Store {
	t.Helper()
	db, err := Create(tmpdb(t)+"/s", Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// commitOneFull creates a FULL snapshot with rows 1..n in table 1.
func commitOneFull(t *testing.T, db *Store, n uint64) SnapshotID {
	t.Helper()
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	for i := uint64(1); i <= n; i++ {
		require.NoError(t, w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String("x")}))
	}
	info, err := w.Commit(context.Background())
	require.NoError(t, err)
	return info.ID
}

func TestBeginSnapshotValidation(t *testing.T) {
	db := newEmptyStore(t)

	// Invalid snapshot type.
	_, err := db.BeginSnapshot(context.Background(), SnapshotType(9), SnapshotOptions{})
	require.ErrorIs(t, err, ErrInvalidArgument, "bad type: %v", err)
	// FULL with a parent.
	_, err = db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{Parent: 1})
	require.ErrorIs(t, err, ErrInvalidParent, "FULL with parent: %v", err)
	// DELTA with missing parent.
	_, err = db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 7})
	require.ErrorIs(t, err, ErrInvalidParent, "DELTA missing parent: %v", err)
	// DELTA with zero parent.
	_, err = db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{})
	require.ErrorIs(t, err, ErrInvalidParent, "DELTA zero parent: %v", err)

	// Writer busy: one active writer excludes a second.
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	_, err = db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.ErrorIs(t, err, ErrWriterBusy, "second writer: %v", err)
	require.Equal(t, uint64(1), w.ID(), "first snapshot ID = %d, want 1", w.ID())
	require.Equal(t, SnapshotID(0), w.Parent(), "FULL Parent = %d, want 0", w.Parent())
	require.NoError(t, w.Abort())
}

func TestBeginSnapshotReadOnlyAndClosed(t *testing.T) {
	base := tmpdb(t) + "/ro"
	db, err := Create(base, Options{})
	require.NoError(t, err)
	commitOneFull(t, db, 1)
	db.Close()

	ro, err := Open(base, Options{ReadOnly: true})
	require.NoError(t, err)
	defer ro.Close()
	require.True(t, ro.ReadOnly())
	require.Equal(t, base, ro.Path())
	_, err = ro.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.ErrorIs(t, err, ErrReadOnly, "read-only BeginSnapshot: %v", err)

	// Closed store.
	db2 := newEmptyStore(t)
	db2.Close()
	_, err = db2.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.ErrorIs(t, err, ErrClosed, "closed BeginSnapshot: %v", err)
}

func TestWriterStateTransitions(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	require.NoError(t, w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}))
	_, err = w.Commit(context.Background())
	require.NoError(t, err)
	// All operations on a committed writer fail with ErrSnapshotCommitted.
	err = w.Insert(context.Background(), 1, 2, 1, Row{Uint64(2), String("b")})
	require.ErrorIs(t, err, ErrSnapshotCommitted, "put after commit: %v", err)
	err = w.DefineSchema(testSchema())
	require.ErrorIs(t, err, ErrSnapshotCommitted, "DefineSchema after commit: %v", err)
	_, err = w.Commit(context.Background())
	require.ErrorIs(t, err, ErrSnapshotCommitted, "double commit: %v", err)
	err = w.Abort()
	require.ErrorIs(t, err, ErrSnapshotCommitted, "abort after commit: %v", err)

	// Aborted writer: all operations fail with ErrSnapshotAborted; Abort is
	// idempotent and frees the writer slot.
	w2, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, err)
	require.Equal(t, uint64(2), w2.ID())
	require.Equal(t, SnapshotID(1), w2.Parent())
	require.NoError(t, w2.Abort())
	require.NoError(t, w2.Abort(), "second abort: %v", err)
	err = w2.Insert(context.Background(), 1, 9, 1, Row{Uint64(9), String("z")})
	require.ErrorIs(t, err, ErrSnapshotAborted, "put after abort: %v", err)
	err = w2.DefineSchema(testSchema())
	require.ErrorIs(t, err, ErrSnapshotAborted, "DefineSchema after abort: %v", err)
	// The slot is free again.
	w3, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, err, "writer slot not freed: %v", err)
	w3.Abort()
}

func TestPutValidation(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	// Zero row ID.
	err = w.Insert(context.Background(), 1, 0, 1, Row{Uint64(0), String("a")})
	require.ErrorIs(t, err, ErrInvalidArgument, "zero row id: %v", err)
	// No schema defined yet.
	err = w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")})
	require.ErrorIs(t, err, ErrSchemaMismatch, "insert without schema: %v", err)
	require.NoError(t, w.DefineSchema(testSchema()))
	require.NoError(t, w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}))
	// Duplicate (table,row) in the same snapshot.
	err = w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")})
	require.ErrorIs(t, err, ErrAlreadyExists, "duplicate row: %v", err)
	// FULL snapshots only allow INSERT (use a fresh row ID so the duplicate
	// check does not mask the FULL-only check).
	err = w.Update(context.Background(), 1, 2, 1, Row{Uint64(2), String("b")})
	require.ErrorIs(t, err, ErrInvalidArgument, "FULL update: %v", err)
	err = w.Delete(context.Background(), 1, 2)
	require.ErrorIs(t, err, ErrInvalidArgument, "FULL delete: %v", err)
	// Wrong column count against the schema.
	err = w.Insert(context.Background(), 1, 2, 1, Row{Uint64(2)})
	require.Error(t, err, "row with missing column accepted")
	require.NoError(t, w.Abort())
}

func TestPutStrictParentValidation(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 2)

	// Strict validation (default): DELTA changes must match the parent view.
	w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	// Update of a row missing in the parent.
	err = w.Update(context.Background(), 1, 99, 1, Row{Uint64(99), String("x")})
	require.ErrorIs(t, err, ErrNotFound, "update missing row: %v", err)
	// Delete of a row missing in the parent: tombstones carry no payload and
	// bypass the strict parent-existence check by design.
	require.NoError(t, w.Delete(context.Background(), 1, 99), "tombstone without parent row: %v", err)
	// Insert of a row that already exists in the parent.
	err = w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("x")})
	require.ErrorIs(t, err, ErrAlreadyExists, "insert existing row: %v", err)
	require.NoError(t, w.Abort())

	// ValidationNone skips exactly the parent-view existence checks.
	db.Close()
	db2, err := Open(db.Path(), Options{Validation: ValidationNone})
	require.NoError(t, err)
	defer db2.Close()
	w2, err := db2.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	require.NoError(t, err)
	require.NoError(t, w2.DefineSchema(testSchema()))
	// Missing-row update is allowed under ValidationNone...
	require.NoError(t, w2.Update(context.Background(), 1, 99, 1, Row{Uint64(99), String("x")}), "ValidationNone update: %v", err)
	require.NoError(t, w2.Abort())
}

func TestApplyChannel(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	// Valid changes then close: Apply drains and returns nil.
	ch := make(chan Change, 4)
	for i := uint64(1); i <= 3; i++ {
		ch <- Change{Type: ChangeInsert, TableID: 1, RowID: i, SchemaVersion: 1, Row: Row{Uint64(i), String("v")}}
	}
	close(ch)
	require.NoError(t, w.Apply(context.Background(), ch), "Apply: %v", err)
	_, err = w.Commit(context.Background())
	require.NoError(t, err)

	// A failing change propagates out of Apply.
	w2, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, err)
	require.NoError(t, w2.DefineSchema(testSchema()))
	bad := make(chan Change, 1)
	bad <- Change{Type: ChangeInsert, TableID: 1, RowID: 1, SchemaVersion: 1, Row: Row{Uint64(1), String("dup")}} // exists in parent
	close(bad)
	err = w2.Apply(context.Background(), bad)
	require.ErrorIs(t, err, ErrAlreadyExists, "Apply bad change: %v", err)
	// Next after error: the writer stays open but put rejects; abort to clean up.
	require.NoError(t, w2.Abort())

	// A cancelled context stops Apply.
	w3, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch3 := make(chan Change)
	err = w3.Apply(ctx, ch3)
	require.ErrorIs(t, err, context.Canceled, "Apply cancelled: %v", err)
	w3.Abort()
}

func TestCommitValidation(t *testing.T) {
	db := newEmptyStore(t)

	// Empty FULL snapshot without AllowEmpty is rejected.
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	_, err = w.Commit(context.Background())
	require.ErrorIs(t, err, ErrInvalidArgument, "empty FULL commit: %v", err)
	// The failed writer cannot be reused.
	_, err = w.Commit(context.Background())
	require.ErrorIs(t, err, ErrSnapshotFailed, "commit after failure: %v", err)
	// A failed commit does not publish anything.
	snaps, _ := db.ListSnapshots(context.Background())
	require.Len(t, snaps, 0, "failed commit published %d snapshots", len(snaps))
	require.NoError(t, db.Close())

	// AllowEmpty permits an empty FULL snapshot.
	db2, err := Create(tmpdb(t)+"/s2", Options{})
	require.NoError(t, err)
	defer db2.Close()
	w2, err := db2.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{AllowEmpty: true})
	require.NoError(t, err)
	info, err := w2.Commit(context.Background())
	require.NoError(t, err, "AllowEmpty commit: %v", err)
	require.Equal(t, uint64(1), info.ID)
	require.Zero(t, info.BlockCount)
	require.Zero(t, info.ChangeCount)

	// Commit with a cancelled context.
	w3, err := db2.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = w3.Commit(ctx)
	require.ErrorIs(t, err, context.Canceled, "cancelled commit: %v", err)
	w3.Abort()
}

func TestCommitFailedWriterState(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	require.NoError(t, w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}))
	// Simulate an I/O failure mid-commit by closing the data file at a fault
	// point: commitLocked fails, the writer enters writerFailed and the
	// writer slot stays held.
	fault.Inject("commit.header.before", func() { db.data.Close() })
	t.Cleanup(fault.Clear)
	_, err = w.Commit(context.Background())
	require.Error(t, err, "commit should fail after data file close")
	_, err = w.Commit(context.Background())
	require.ErrorIs(t, err, ErrSnapshotFailed, "commit after failure: %v", err)
	// The failed writer holds the slot until Close.
	_, err = db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.ErrorIs(t, err, ErrWriterBusy, "BeginSnapshot with failed writer: %v", err)
	db.Close()
}
