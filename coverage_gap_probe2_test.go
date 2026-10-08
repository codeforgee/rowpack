package rowpack

// Round-2 probes: verify.go chain/scope checks and Stats uncovered arms.

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// NOTE: the "FULL snapshot has a parent" and "DELTA parent missing" arms in
// Verify are unreachable through any supported path: View.beginApply rejects
// both at construction, and the crafted-view helpers funnel through Apply.
// They stay as defense in depth for views built by future recovery paths.

// A chain deeper than the store's configured limit is corruption, even though
// the view itself applied under the format default.
func TestVerifyRejectsChainBeyondDepthLimit(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	publishCraftedView(t, db, 2, 1, nil)
	publishCraftedView(t, db, 3, 2, nil)
	// Shrink the store's limit after the fact; Limits are reader policy.
	db.opts.Limits.MaxSnapshotDepth = 1

	_, err := db.Verify(ctx, VerifyQuick, VerifyScope{})
	require.Error(t, err)
	var ce *CorruptionError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, ErrCorruptIndex, ce.Kind)
	require.Equal(t, "snapshot chain too deep", ce.Reason)
}

// Scope filters: a snapshot-filtered verify skips other snapshots' blocks, and
// a table-filtered verify skips blocks of other tables. Both must still
// succeed on a healthy store.
func TestVerifyScopeFilters(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("kept", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.DefineTable("skipped", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.Insert(ctx, "kept", 1, Row{Int64(1)}))
	require.NoError(t, w.Insert(ctx, "skipped", 1, Row{Int64(1)}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	// Whole store: both tables checked.
	full, err := db.Verify(ctx, VerifyFull, VerifyScope{})
	require.NoError(t, err)
	blocksAll := full.BlocksChecked

	// Table filter drops the other table's rows blocks.
	byTable, err := db.Verify(ctx, VerifyFull, VerifyScope{Tables: []string{Qualify(NSUser, "kept")}})
	require.NoError(t, err)
	require.Less(t, byTable.BlocksChecked, blocksAll)

	// Snapshot filter on the only snapshot still checks it.
	bySnap, err := db.Verify(ctx, VerifyFull, VerifyScope{Snapshot: snap})
	require.NoError(t, err)
	require.Equal(t, blocksAll, bySnap.BlocksChecked)

	// A verify of a tiny store must still report a positive duration.
	require.Greater(t, full.Duration, time.Duration(0))
}

// A healthy store must not trigger the duplicate-address arm.
func TestVerifyAcceptsDistinctAddresses(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	_, err := db.Verify(ctx, VerifyQuick, VerifyScope{})
	require.NoError(t, err)
}
