package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReadLookupNotFoundArms pins the ErrNotFound contract of the read-side
// lookups for unknown snapshots and unknown tables.
func TestReadLookupNotFoundArms(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, w.Insert("t", 1, Row{Uint64(1)}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	const missing = SnapshotID(999)

	// Exists: unknown snapshot and unknown table are ErrNotFound, matching
	// Get/Blocks; a known row that is absent is (false, nil).
	_, err = db.Exists(ctx, missing, "t", 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Exists(ctx, snap, "nope", 1)
	require.ErrorIs(t, err, ErrNotFound)
	ok, err := db.Exists(ctx, snap, "t", 42)
	require.NoError(t, err)
	require.False(t, ok)

	// Blocks: unknown snapshot and unknown table.
	_, err = db.Blocks(ctx, missing, "t")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Blocks(ctx, snap, "nope")
	require.ErrorIs(t, err, ErrNotFound)

	// BlockRange (lo >= hi) and Scan: unknown snapshot.
	_, err = db.Blocks(ctx, snap, "t")
	require.NoError(t, err)
	_, err = db.Scan(ctx, missing, "t", ScanOptions{})
	require.ErrorIs(t, err, ErrNotFound)
}
