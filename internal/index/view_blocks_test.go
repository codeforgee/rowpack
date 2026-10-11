package index

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// applyBlocksTxn commits one snapshot with the given block ids on top of v.
func applyBlocksTxn(t *testing.T, v *View, id, parent uint64, typ format.SnapshotType, blockIDs []uint64) *View {
	t.Helper()
	b := NewBuilder(id)
	b.SetRowDedup(false)
	require.NoError(t, b.SetSnapshot(snapEntry(id, parent, typ)))
	for _, bid := range blockIDs {
		require.NoError(t, b.AddBlock(blockEntry(bid, id, 1)))
	}
	txn := &Txn{Snapshot: *b.snapshot, Metadata: b.metadata, Blocks: b.blocks, Rows: b.rows}
	nv, err := v.Apply(txn, 64)
	require.NoError(t, err)
	return nv
}

// TestViewBlocksDeltaVisibility pins the base+delta block table: blocks stay
// resolvable across later commits — including commits that add no blocks,
// whose delta must still carry the parent's recent blocks forward.
func TestViewBlocksDeltaVisibility(t *testing.T) {
	v := EmptyView()
	v = applyBlocksTxn(t, v, 1, 0, format.SnapshotFull, []uint64{10, 11, 12})
	v = applyBlocksTxn(t, v, 2, 1, format.SnapshotDelta, nil) // empty commit
	v = applyBlocksTxn(t, v, 3, 2, format.SnapshotDelta, []uint64{13, 14})

	for _, bid := range []uint64{10, 11, 12, 13, 14} {
		require.NotNil(t, v.Block(bid), "block %d must be visible after later commits", bid)
	}
	got := v.Blocks()
	require.Len(t, got, 5)
	require.Equal(t, 5, v.blocksN)
	// BlockLoc identity: Block and Blocks agree on the pointer.
	for _, bl := range got {
		require.Same(t, bl, v.Block(bl.BlockID))
	}
}

// TestViewBlocksFoldback drives the delta past the fold-back threshold: the
// table folds into a fresh base mid-chain and every block stays resolvable,
// with Blocks() still complete and duplicate-free.
func TestViewBlocksFoldback(t *testing.T) {
	v := EmptyView()
	next := uint64(1)
	parent := uint64(0)
	var all []uint64
	for round := 0; round < 3; round++ {
		var ids []uint64
		for range 600 {
			ids = append(ids, next)
			all = append(all, next)
			next++
		}
		typ := format.SnapshotDelta
		if round == 0 {
			typ = format.SnapshotFull
		}
		v = applyBlocksTxn(t, v, uint64(round+1), parent, typ, ids)
		if round == 0 {
			parent = 1
		} else {
			parent = uint64(round + 1)
		}
		// Invariants after every commit.
		require.Len(t, v.Blocks(), int(v.blocksN))
		for _, bid := range all {
			require.NotNil(t, v.Block(bid))
		}
	}
	require.Equal(t, 1800, v.blocksN)
	// The fold-back must have run: the base carries the bulk of the table.
	require.Greater(t, len(v.blocksBase), 0)
	seen := make(map[uint64]bool, len(all))
	for _, bl := range v.Blocks() {
		require.False(t, seen[bl.BlockID], "block %d listed twice", bl.BlockID)
		seen[bl.BlockID] = true
	}
	require.Len(t, seen, 1800)
}
