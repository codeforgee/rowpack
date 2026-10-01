package rowpack

import (
	"context"
	"errors"
	"testing"

	"github.com/codeforgee/rowpack/internal/fault"
	"github.com/stretchr/testify/require"
)

// TestUpdateDeleteUnknownTableRejected: the writer-level Update/Delete entry
// points resolve the table themselves and reject unknown names before any
// change-type policy applies.
func TestUpdateDeleteUnknownTableRejected(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))

	err = tx.Update(ctx, "nope", 1, Row{Uint64(1)})
	require.ErrorIs(t, err, ErrNotFound)
	err = tx.Delete(ctx, "nope", 1)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, tx.Rollback())
}

// TestCommitTornWhenTxnAppendFails: closing the data file at the txn fault
// point fails the stored-txn append after the blocks are already on disk —
// a torn commit, not an unknown outcome.
func TestCommitTornWhenTxnAppendFails(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.txn.before", func() { _ = db.data.Close() })
	defer fault.Clear()

	_, err = w.Commit(ctx)
	// A raw append failure before the sync: not wrapped into CommitError
	// (that wrapper is reserved for post-sync "outcome unknown" failures).
	require.Error(t, err)
	require.False(t, errors.As(err, new(*CommitError)), "pre-sync append failure stays unwrapped")
	_ = db.Close() // already closed by the injection

	// Reopen: nothing committed; the torn tail is dropped.
	db2, err := Open(db.basePath, Options{})
	require.NoError(t, err)
	defer func() { _ = db2.Close() }()
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Empty(t, snaps)
}

// TestCommitFailsAtFooterAppend: a failure between the txn append and the
// footer append tears the commit at the footer.
func TestCommitFailsAtFooterAppend(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.footer.before", func() { _ = db.data.Close() })
	defer fault.Clear()

	_, err = w.Commit(ctx)
	require.Error(t, err)
	_ = db.Close() // already closed by the injection

	db2, err := Open(db.basePath, Options{})
	require.NoError(t, err)
	defer func() { _ = db2.Close() }()
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Empty(t, snaps)
}
