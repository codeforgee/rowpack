package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWriterZeroRowIDRejected pins the zero-row-id guard on every mutation
// entry point (row id 0 is reserved as "no row" in the row index).
func TestWriterZeroRowIDRejected(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))

	require.ErrorIs(t, tx.Insert("t", 0, Row{Uint64(1)}), ErrInvalidArgument)
	require.ErrorContains(t, tx.Insert("t", 0, Row{Uint64(1)}), "row id is zero")
	require.ErrorIs(t, tx.Update("t", 0, Row{Uint64(1)}), ErrInvalidArgument)
	require.ErrorIs(t, tx.Delete("t", 0), ErrInvalidArgument)

	// The internal put path re-checks: belt and braces against future callers.
	err = tx.w.put(ctx, rowChange{typ: ChangeInsert, table: tx.w.tableIDs["t"], rowID: 0})
	require.ErrorIs(t, err, ErrInvalidArgument)

	require.NoError(t, tx.Rollback())
}

// TestWriterTerminalStates pins the once-only semantics of Commit/Rollback:
// a committed writer can only be observed, never rolled back or re-committed.
func TestWriterTerminalStates(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	// Commit after commit.
	_, err = tx.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotCommitted)
	// Rollback after commit surfaces the same terminal-state error.
	require.ErrorIs(t, tx.Rollback(), ErrSnapshotCommitted)
}

// TestCommitAfterClose pins the failure mode of committing a writer whose
// store was closed underneath it: ErrClosed, writer marked failed.
func TestCommitAfterClose(t *testing.T) {
	ctx := context.Background()
	base := tmpdb(t)
	db, err := Create(base, Options{})
	require.NoError(t, err)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))

	// Close aborts every pending writer, so the pending commit reports the
	// aborted state, not the closed store; the state sticks afterwards.
	require.NoError(t, db.Close())
	_, err = tx.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotAborted)
	_, err = tx.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotAborted, "aborted state is terminal")
}
