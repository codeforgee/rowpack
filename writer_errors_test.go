package rowpack

import (
	"context"
	"errors"
	"testing"

	"github.com/rowpack/rowpack/internal/fault"
)

func testSchema() Schema {
	return Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString},
	}}
}

func newEmptyStore(t *testing.T) *Store {
	t.Helper()
	db, err := Create(t.TempDir()+"/s", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// commitOneFull creates a FULL snapshot with rows 1..n in table 1.
func commitOneFull(t *testing.T, db *Store, n uint64) SnapshotID {
	t.Helper()
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= n; i++ {
		if err := w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String("x")}); err != nil {
			t.Fatal(err)
		}
	}
	info, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info.ID
}

func TestBeginSnapshotValidation(t *testing.T) {
	db := newEmptyStore(t)

	// Invalid snapshot type.
	if _, err := db.BeginSnapshot(context.Background(), SnapshotType(9), SnapshotOptions{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("bad type: %v", err)
	}
	// FULL with a parent.
	if _, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{Parent: 1}); !errors.Is(err, ErrInvalidParent) {
		t.Fatalf("FULL with parent: %v", err)
	}
	// DELTA with missing parent.
	if _, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 7}); !errors.Is(err, ErrInvalidParent) {
		t.Fatalf("DELTA missing parent: %v", err)
	}
	// DELTA with zero parent.
	if _, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{}); !errors.Is(err, ErrInvalidParent) {
		t.Fatalf("DELTA zero parent: %v", err)
	}

	// Writer busy: one active writer excludes a second.
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{}); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("second writer: %v", err)
	}
	if w.ID() != 1 {
		t.Fatalf("first snapshot ID = %d, want 1", w.ID())
	}
	if w.Parent() != 0 {
		t.Fatalf("FULL Parent = %d, want 0", w.Parent())
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginSnapshotReadOnlyAndClosed(t *testing.T) {
	base := t.TempDir() + "/ro"
	db, err := Create(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	commitOneFull(t, db, 1)
	db.Close()

	ro, err := Open(base, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if !ro.ReadOnly() || ro.Path() != base {
		t.Fatalf("ReadOnly=%v Path=%s", ro.ReadOnly(), ro.Path())
	}
	if _, err := ro.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{}); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only BeginSnapshot: %v", err)
	}

	// Closed store.
	db2 := newEmptyStore(t)
	db2.Close()
	if _, err := db2.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed BeginSnapshot: %v", err)
	}
}

func TestWriterStateTransitions(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	// All operations on a committed writer fail with ErrSnapshotCommitted.
	if err := w.Insert(context.Background(), 1, 2, 1, Row{Uint64(2), String("b")}); !errors.Is(err, ErrSnapshotCommitted) {
		t.Fatalf("put after commit: %v", err)
	}
	if err := w.DefineSchema(testSchema()); !errors.Is(err, ErrSnapshotCommitted) {
		t.Fatalf("DefineSchema after commit: %v", err)
	}
	if _, err := w.Commit(context.Background()); !errors.Is(err, ErrSnapshotCommitted) {
		t.Fatalf("double commit: %v", err)
	}
	if err := w.Abort(); !errors.Is(err, ErrSnapshotCommitted) {
		t.Fatalf("abort after commit: %v", err)
	}

	// Aborted writer: all operations fail with ErrSnapshotAborted; Abort is
	// idempotent and frees the writer slot.
	w2, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if w2.ID() != 2 || w2.Parent() != 1 {
		t.Fatalf("DELTA id=%d parent=%d", w2.ID(), w2.Parent())
	}
	if err := w2.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := w2.Abort(); err != nil {
		t.Fatalf("second abort: %v", err)
	}
	if err := w2.Insert(context.Background(), 1, 9, 1, Row{Uint64(9), String("z")}); !errors.Is(err, ErrSnapshotAborted) {
		t.Fatalf("put after abort: %v", err)
	}
	if err := w2.DefineSchema(testSchema()); !errors.Is(err, ErrSnapshotAborted) {
		t.Fatalf("DefineSchema after abort: %v", err)
	}
	// The slot is free again.
	w3, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	if err != nil {
		t.Fatalf("writer slot not freed: %v", err)
	}
	w3.Abort()
}

func TestPutValidation(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Zero row ID.
	if err := w.Insert(context.Background(), 1, 0, 1, Row{Uint64(0), String("a")}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero row id: %v", err)
	}
	// No schema defined yet.
	if err := w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("insert without schema: %v", err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}); err != nil {
		t.Fatal(err)
	}
	// Duplicate (table,row) in the same snapshot.
	if err := w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate row: %v", err)
	}
	// FULL snapshots only allow INSERT (use a fresh row ID so the duplicate
	// check does not mask the FULL-only check).
	if err := w.Update(context.Background(), 1, 2, 1, Row{Uint64(2), String("b")}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("FULL update: %v", err)
	}
	if err := w.Delete(context.Background(), 1, 2); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("FULL delete: %v", err)
	}
	// Wrong column count against the schema.
	if err := w.Insert(context.Background(), 1, 2, 1, Row{Uint64(2)}); err == nil {
		t.Fatal("row with missing column accepted")
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestPutStrictParentValidation(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 2)

	// Strict validation (default): DELTA changes must match the parent view.
	w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	// Update of a row missing in the parent.
	if err := w.Update(context.Background(), 1, 99, 1, Row{Uint64(99), String("x")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing row: %v", err)
	}
	// Delete of a row missing in the parent: tombstones carry no payload and
	// bypass the strict parent-existence check by design.
	if err := w.Delete(context.Background(), 1, 99); err != nil {
		t.Fatalf("tombstone without parent row: %v", err)
	}
	// Insert of a row that already exists in the parent.
	if err := w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("x")}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("insert existing row: %v", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatal(err)
	}

	// ValidationNone skips exactly the parent-view existence checks.
	db.Close()
	db2, err := Open(db.Path(), Options{Validation: ValidationNone})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	w2, err := db2.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	// Missing-row update is allowed under ValidationNone...
	if err := w2.Update(context.Background(), 1, 99, 1, Row{Uint64(99), String("x")}); err != nil {
		t.Fatalf("ValidationNone update: %v", err)
	}
	if err := w2.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyChannel(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	// Valid changes then close: Apply drains and returns nil.
	ch := make(chan Change, 4)
	for i := uint64(1); i <= 3; i++ {
		ch <- Change{Type: ChangeInsert, TableID: 1, RowID: i, SchemaVersion: 1, Row: Row{Uint64(i), String("v")}}
	}
	close(ch)
	if err := w.Apply(context.Background(), ch); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := w.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A failing change propagates out of Apply.
	w2, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	bad := make(chan Change, 1)
	bad <- Change{Type: ChangeInsert, TableID: 1, RowID: 1, SchemaVersion: 1, Row: Row{Uint64(1), String("dup")}} // exists in parent
	close(bad)
	if err := w2.Apply(context.Background(), bad); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Apply bad change: %v", err)
	}
	// Next after error: the writer stays open but put rejects; abort to clean up.
	if err := w2.Abort(); err != nil {
		t.Fatal(err)
	}

	// A cancelled context stops Apply.
	w3, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch3 := make(chan Change)
	if err := w3.Apply(ctx, ch3); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply cancelled: %v", err)
	}
	w3.Abort()
}

func TestCommitValidation(t *testing.T) {
	db := newEmptyStore(t)

	// Empty FULL snapshot without AllowEmpty is rejected.
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(context.Background()); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty FULL commit: %v", err)
	}
	// The failed writer cannot be reused.
	if _, err := w.Commit(context.Background()); !errors.Is(err, ErrSnapshotFailed) {
		t.Fatalf("commit after failure: %v", err)
	}
	// A failed commit does not publish anything.
	if snaps, _ := db.ListSnapshots(context.Background()); len(snaps) != 0 {
		t.Fatalf("failed commit published %d snapshots", len(snaps))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// AllowEmpty permits an empty FULL snapshot.
	db2, err := Create(t.TempDir()+"/s2", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	w2, err := db2.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{AllowEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	info, err := w2.Commit(context.Background())
	if err != nil {
		t.Fatalf("AllowEmpty commit: %v", err)
	}
	if info.ID != 1 || info.BlockCount != 0 || info.ChangeCount != 0 {
		t.Fatalf("empty snapshot info: %+v", info)
	}

	// Commit with a cancelled context.
	w3, err := db2.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w3.Commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled commit: %v", err)
	}
	w3.Abort()
}

func TestCommitFailedWriterState(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}); err != nil {
		t.Fatal(err)
	}
	// Simulate an I/O failure mid-commit by closing the data file at a fault
	// point: commitLocked fails, the writer enters writerFailed and the
	// writer slot stays held.
	fault.Inject("commit.data-header.before", func() { db.data.Close() })
	t.Cleanup(fault.Clear)
	if _, err := w.Commit(context.Background()); err == nil {
		t.Fatal("commit should fail after data file close")
	}
	if _, err := w.Commit(context.Background()); !errors.Is(err, ErrSnapshotFailed) {
		t.Fatalf("commit after failure: %v", err)
	}
	// The failed writer holds the slot until Close.
	if _, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{}); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("BeginSnapshot with failed writer: %v", err)
	}
	db.Close()
}
