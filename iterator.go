package rowpack

import (
	"container/heap"
	"context"
	"fmt"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
)

// ScanOptions bounds a Scan.
type ScanOptions struct {
	StartRowID RowID // inclusive; 0 = from the beginning
	EndRowID   RowID // exclusive; 0 = no upper bound
}

// Iterator streams the logically visible rows of a table at a snapshot in
// strictly ascending RowID order. Rows overridden by descendants and
// tombstones are filtered out. It captures the immutable index view at
// creation, so concurrent commits do not affect it.
type Iterator struct {
	store    *Store
	state    *publishedState
	ctx      context.Context
	snapshot SnapshotID
	table    TableID
	opts     ScanOptions

	layers []*layerIter
	heap   rowHeap

	curRowID RowID
	curRow   Row
	curLoc   *index.RowLoc

	// Block cursor: reuses the parsed payload while consecutive rows fall in
	// the same block, avoiding a per-row full-block parse.
	curBlockID uint64
	curBlk     *index.BlockLoc
	curPayload *block.RowsPayload

	err    error
	closed bool
}

// layerIter walks one snapshot layer's sorted incremental row index.
type layerIter struct {
	keys  []index.RowKeyLoc
	pos   int
	depth int // 0 = target snapshot; larger = ancestor
}

// rowHeap is a min-heap over the current head of each layer, keyed by RowID
// with depth as tiebreaker (shallowest wins).
type rowHeap []*layerIter

func (h rowHeap) Len() int { return len(h) }
func (h rowHeap) Less(i, j int) bool {
	a, b := h[i].head(), h[j].head()
	if a.RowID != b.RowID {
		return a.RowID < b.RowID
	}
	return h[i].depth < h[j].depth
}
func (h rowHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *rowHeap) Push(x any)   { *h = append(*h, x.(*layerIter)) }
func (h *rowHeap) Pop() any     { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

func (l *layerIter) head() index.RowKeyLoc { return l.keys[l.pos] }

// Scan opens an iterator over the visible rows of table at snapshot.
func (s *Store) Scan(ctx context.Context, snapshot SnapshotID, table TableID, opts ScanOptions) (*Iterator, error) {
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	if opts.StartRowID > 0 && opts.EndRowID > 0 && opts.StartRowID >= opts.EndRowID {
		return nil, fmt.Errorf("%w: scan start %d >= end %d", ErrInvalidArgument, opts.StartRowID, opts.EndRowID)
	}
	it := &Iterator{store: s, state: st, ctx: ctx, snapshot: snapshot, table: table, opts: opts}
	// Build the parent chain layers (target snapshot first).
	cur := snapshot
	for depth := 0; ; depth++ {
		keys := view.RowKeys(cur, table)
		if len(keys) > 0 {
			it.layers = append(it.layers, &layerIter{keys: keys, depth: depth})
		}
		sm := view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			break
		}
		cur = sm.Parent
	}
	heap.Init(&it.heap)
	for _, l := range it.layers {
		if l.pos < len(l.keys) {
			heap.Push(&it.heap, l)
		}
	}
	return it, nil
}

// Next advances to the next visible row. It returns false at the end; call
// Err to distinguish completion from failure. It is not safe for concurrent
// use.
func (it *Iterator) Next() bool {
	if it.closed || it.err != nil {
		return false
	}
	if it.ctx != nil {
		select {
		case <-it.ctx.Done():
			it.err = it.ctx.Err()
			return false
		default:
		}
	}
	for it.heap.Len() > 0 {
		winner := heap.Pop(&it.heap).(*layerIter)
		rowID := winner.head().RowID
		loc := winner.head().Loc
		// All layers currently at rowID lose; pop them and advance. The heap
		// tiebreak ensures the winner is the shallowest layer.
		for it.heap.Len() > 0 && it.heap[0].head().RowID == rowID {
			l := heap.Pop(&it.heap).(*layerIter)
			l.advance(&it.heap)
		}
		winner.advance(&it.heap)
		// Range filtering.
		if it.opts.StartRowID > 0 && rowID < it.opts.StartRowID {
			continue
		}
		if it.opts.EndRowID > 0 && rowID >= it.opts.EndRowID {
			return false
		}
		if loc == nil || loc.ChangeType == fileformat.ChangeDelete {
			continue // tombstone: hide the row entirely
		}
		row, err := it.rowAt(loc)
		if err != nil {
			it.err = err
			return false
		}
		it.curRowID = rowID
		it.curRow = row
		it.curLoc = loc
		return true
	}
	return false
}
func (l *layerIter) advance(h *rowHeap) {
	l.pos++
	if l.pos < len(l.keys) {
		heap.Push(h, l)
	}
}

// rowAt resolves one row, reusing the parsed payload of the current block
// when the location is inside it.
func (it *Iterator) rowAt(loc *index.RowLoc) (Row, error) {
	if it.curBlockID != loc.BlockID {
		bl := it.state.view.Block(loc.BlockID)
		if bl == nil {
			return nil, fmt.Errorf("rowpack: block %d missing from view", loc.BlockID)
		}
		blk, err := it.store.loader.Load(int64(bl.DataOffset), bl.BlockID)
		if err != nil {
			return nil, err
		}
		rp, err := block.ParseRowsPayload(blk.Raw, bl.ItemCount)
		if err != nil {
			return nil, err
		}
		it.curBlockID = loc.BlockID
		it.curBlk = bl
		it.curPayload = rp
	}
	return it.store.rowFromPayload(it.curPayload, it.curBlk, loc, it.state.schemas)
}

// RowID returns the current row's RowID.
func (it *Iterator) RowID() RowID { return it.curRowID }

// Row returns the current row. The Row remains valid after subsequent Next
// calls; it is owned by the caller.
func (it *Iterator) Row() Row { return it.curRow }

// Err returns the first error encountered, or nil on clean completion.
func (it *Iterator) Err() error { return it.err }

// Close releases the iterator. It is idempotent.
func (it *Iterator) Close() error {
	it.closed = true
	return nil
}
