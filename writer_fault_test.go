package rowpack

import (
	"context"
	"os"
	"testing"

	"github.com/codeforgee/rowpack/internal/fault"
	"github.com/stretchr/testify/require"
)

// This file pins the commit pipeline's failure semantics: which failures are
// a known torn commit (outcome known, snapshot absent) versus an unknown
// outcome (the snapshot may be durable; the store latches must-reopen), and
// the rejection arms of put()/createTable() that deterministic tests can
// drive directly.

// TestCommitUnknownAfterSyncFailure closes the underlying data file at the
// sync fault point: the footer is already appended, so the snapshot MAY be
// fully durable. Commit must report CommitError{Unknown: true}, the writer
// must become failed, new writers must be refused with ErrMustReopen, and
// recovery on reopen must accept the snapshot.
func TestCommitUnknownAfterSyncFailure(t *testing.T) {
	ctx := context.Background()
	path := tmpdb(t)
	db, err := Create(path, Options{})
	require.NoError(t, err)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.sync.before", func() { _ = db.data.Close() })
	defer fault.Clear()
	_, err = tx.Commit(ctx)
	require.Error(t, err)
	var ce *CommitError
	require.ErrorAs(t, err, &ce)
	require.True(t, ce.Unknown, "sync failure after footer append is an unknown outcome")

	// The writer is failed; every further mutation and a second commit fail.
	require.ErrorIs(t, tx.Insert(ctx, "t", 2, Row{Uint64(2)}), ErrSnapshotFailed)
	_, err = tx.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotFailed)

	// New FULL writers are refused: the store latched must-reopen. (Begin
	// with Latest cannot even resolve a parent: the unknown-outcome failure
	// deliberately leaves the in-memory view without the snapshot.)
	_, err = db.Begin(ctx, NoParent)
	require.ErrorIs(t, err, ErrMustReopen)
	_, err = db.Begin(ctx, Latest)
	require.ErrorIs(t, err, ErrInvalidParent)
	_ = db.Close()

	// Recovery: the footer was written before the sync point, so the snapshot
	// is fully committed on disk.
	db2, err := Open(path, Options{})
	require.NoError(t, err)
	defer db2.Close()
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	_, err = db2.Get(ctx, snaps[0].ID, "t", 1, nil)
	require.NoError(t, err, "snapshot must be durable after unknown-outcome failure")
}

// TestCommitUnknownWhenViewRejects commits a snapshot whose ID collides with
// an already-committed one: the failure happens after the sync (durable
// bytes), so the outcome is unknown even though the view rejected the apply.
func TestCommitUnknownWhenViewRejects(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	// Forge a snapshot-ID collision with the committed snapshot 1.
	tx2.w.id = 1
	require.NoError(t, tx2.Insert(ctx, "t", 2, Row{Uint64(2)}))
	_, err = tx2.Commit(ctx)
	require.Error(t, err)
	var ce *CommitError
	require.ErrorAs(t, err, &ce)
	require.True(t, ce.Unknown)
	require.ErrorContains(t, ce, "already committed")
	_, err = db.Begin(ctx, Latest)
	require.ErrorIs(t, err, ErrMustReopen)
}

// TestCommitTornOnTrailingAppend appends a stray byte after the footer: the
// length cross-check fires as a known (torn) failure, not an unknown one.
func TestCommitTornOnTrailingAppend(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.footer.after", func() { _, _ = db.data.Append([]byte{0xAA}) })
	defer fault.Clear()
	_, err = tx.Commit(ctx)
	require.Error(t, err)
	require.ErrorContains(t, err, "snapshot end")
	var ce *CommitError
	require.NotErrorAs(t, err, &ce, "torn commit must not claim an unknown outcome")
}

// TestCommitTornOnHeaderWriteFailure closes the data file before the snapshot
// header is appended: a plain error (nothing durable), writer failed.
func TestCommitTornOnHeaderWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.header.before", func() { _ = db.data.Close() })
	defer fault.Clear()
	_, err = tx.Commit(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrClosed)
	// A torn commit does not latch must-reopen: the next writer is allowed.
	tx2, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx2.Rollback())
}

// TestCommitTornOnBlockWriteFailure closes the data file between the snapshot
// header and the first block: the block append fails with a plain error.
func TestCommitTornOnBlockWriteFailure(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.block.before", func() { _ = db.data.Close() })
	defer fault.Clear()
	_, err = tx.Commit(ctx)
	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrClosed)
}

// TestCommitCtxCanceled rejects a Commit whose context is already cancelled.
func TestCommitCtxCanceled(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = tx.Commit(cctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, tx.Rollback())
}

// TestPutRejectedBranches drives put()'s validation arms that the public
// methods cannot reach with ordinary arguments.
func TestPutRejectedBranches(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	w := tx.w
	tid := w.tableIDs["t"]

	// Cancelled context.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	err = w.put(cctx, rowChange{typ: ChangeInsert, table: tid, rowID: 9, row: Row{Uint64(9)}})
	require.ErrorIs(t, err, context.Canceled)

	// Duplicate row inside one snapshot.
	require.NoError(t, tx.Insert(ctx, "t", 5, Row{Uint64(5)}))
	err = tx.Insert(ctx, "t", 5, Row{Uint64(6)})
	require.ErrorIs(t, err, ErrAlreadyExists)

	// FULL snapshots reject UPDATE/DELETE even before parent checks.
	err = w.put(ctx, rowChange{typ: ChangeUpdate, table: tid, rowID: 7, row: Row{Uint64(6)}})
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorContains(t, err, "FULL snapshot only allows INSERT")

	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	// Undefined schema version for a DELTA update: define (no-op cache), then
	// update the existing parent row 5 with a version the chain never had.
	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, tx2.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	tid2 := tx2.w.tableIDs["t"]
	err = tx2.w.put(ctx, rowChange{typ: ChangeUpdate, table: tid2, rowID: 5, schemaVersion: 99, row: Row{Uint64(6)}})
	require.ErrorIs(t, err, ErrSchemaMismatch)

	// Unknown internal table id surfaces through the parent-existence check.
	err = tx2.w.put(ctx, rowChange{typ: ChangeUpdate, table: TableID(9999), rowID: 5, row: Row{Uint64(6)}})
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, tx2.Rollback())
}

// TestDeltaRejectsMissingParentRows pins the DELTA parent-existence checks:
// UPDATE/DELETE of a row the parent does not have, and INSERT of a row it
// already has, are all rejected.
func TestDeltaRejectsMissingParentRows(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.ErrorIs(t, tx2.Update(ctx, "t", 42, Row{Uint64(9)}), ErrNotFound)
	require.ErrorIs(t, tx2.Delete(ctx, "t", 42), ErrNotFound)
	require.ErrorIs(t, tx2.Insert(ctx, "t", 1, Row{Uint64(2)}), ErrAlreadyExists)
	require.NoError(t, tx2.Rollback())
}

// TestDeltaConflictingRedefinitionsRejected pins the schema-comparison false
// arms: a DELTA that redefines a committed table with a different layout is a
// schema conflict, not a new version (the chain's columns must match exactly).
func TestDeltaConflictingRedefinitionsRejected(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}, {Name: "v", Type: TypeUint64}}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	variants := []struct {
		name string
		cols []Column
	}{
		{"type changed", []Column{{Name: "id", Type: TypeUint64}, {Name: "v", Type: TypeString}}},
		{"nullability changed", []Column{{Name: "id", Type: TypeUint64, Nullable: true}, {Name: "v", Type: TypeUint64}}},
		{"column count changed", []Column{{Name: "id", Type: TypeUint64}}},
	}
	for _, v := range variants {
		tx2, err := db.Begin(ctx, Latest)
		require.NoError(t, err)
		err = tx2.DefineTable("t", v.cols)
		require.ErrorIs(t, err, ErrSchemaConflict, v.name)
		require.NoError(t, tx2.Rollback(), v.name)
	}
}

// TestTableIDSpaceExhausted pins the id-space guard: when the store's table
// id counter is at its ceiling, DefineTable must fail cleanly.
func TestTableIDSpaceExhausted(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	db.maxTableID.Store(^uint32(0) - 1)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	err = tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}})
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorContains(t, err, "table id space exhausted")
	require.NoError(t, tx.Rollback())
}

// TestFullReopenRedefinesSameTable covers the FULL-checkpoint chain branch:
// after reopening, re-defining an identical table writes the snapshot's own
// schema layer even though it matches the ancestor's (latest==0 fallback).
func TestFullReopenRedefinesSameTable(t *testing.T) {
	ctx := context.Background()
	path := tmpdb(t)
	db, err := Create(path, Options{})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db2, err := Open(path, Options{})
	require.NoError(t, err)
	defer func() { _ = db2.Close() }()
	tx2, err := db2.Begin(ctx, NoParent) // FULL checkpoint over the history
	require.NoError(t, err)
	require.NoError(t, tx2.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx2.Insert(ctx, "t", 2, Row{Uint64(2)}))
	snap, err := tx2.Commit(ctx)
	require.NoError(t, err)

	// Rows from both generations are readable.
	row, err := db2.Get(ctx, snap, "t", 2, nil)
	require.NoError(t, err)
	u, ok := row[0].Uint64()
	require.True(t, ok)
	require.Equal(t, uint64(2), u)
}
