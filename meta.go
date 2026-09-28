package rowpack

import (
	"context"
	"fmt"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/index"
)

// Meta returns the meta block published with the snapshot: the opaque value
// its transaction passed to Tx.SetMeta, byte for byte. It is the engine-side
// counterpart of the snapshot's own data — one block per snapshot, never
// merged, never parsed.
//
// Resolution follows the parent chain: the nearest layer at or above snap
// that set a meta decides, so a DELTA that sets nothing inherits its
// parent's value and each read sees exactly one value. No snapshot in the
// chain carries one when the result is nil, which is reported as (nil, nil):
// "no meta" is a valid state, not an error. An unknown snapshot is
// ErrNotFound, like every other snapshot-taking read.
//
// The returned slice is owned by the caller: it is a copy, so mutating it
// never disturbs the decoded block cache and a later read is unaffected. The
// bytes pass CRC, size and AEAD validation before they are returned, so a
// damaged block surfaces as CorruptionError instead.
//
// ctx is accepted for signature consistency; cancellation is not observed
// mid-read (same as Get and ReadBatch).
func (s *Store) Meta(ctx context.Context, snap SnapshotID) ([]byte, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if st.view.Snapshot(snap) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snap)
	}
	loc := metaBlockLoc(st.view, snap)
	if loc == nil {
		return nil, nil
	}
	blk, err := s.loader.Load(int64(loc.DataOffset), loc.BlockID)
	if err != nil {
		return nil, err
	}
	// The loader serves shared, cache-resident blocks; never hand one to a
	// caller that could write to it.
	out := make([]byte, len(blk.Raw))
	copy(out, blk.Raw)
	return out, nil
}

// metaBlockLoc resolves a snapshot's meta block along its parent chain, the
// same nearest-ancestor rule every other parent-chain read uses: a snapshot's
// own value shadows every ancestor's. A layer can hold at most one (the writer
// appends one block per commit); should a crafted view carry several, the
// highest BlockID decides, so the answer stays a function of the committed
// bytes rather than of Go map iteration order.
func metaBlockLoc(view *index.View, snap SnapshotID) *index.BlockLoc {
	// chain lists the snapshots allowed to answer, nearest first.
	depthOf := make(map[uint64]int, 8)
	chain := make([]uint64, 0, 8)
	for cur := uint64(snap); ; {
		depthOf[cur] = len(chain)
		chain = append(chain, cur)
		sm := view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			break
		}
		cur = sm.Parent
	}
	// One traversal of the block list, not one per layer: a deep chain stays
	// O(blocks + depth) instead of O(blocks × depth).
	var best *index.BlockLoc
	bestDepth := len(chain)
	for _, bl := range view.Blocks() {
		if bl.Kind != format.BlockKindSnapshotMeta {
			continue
		}
		d, ok := depthOf[bl.SnapshotID]
		if !ok {
			continue
		}
		if best == nil || d < bestDepth || (d == bestDepth && bl.BlockID > best.BlockID) {
			best, bestDepth = bl, d
		}
	}
	return best
}
