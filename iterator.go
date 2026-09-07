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
	curLoc   *index.RowLoc

	// buf is the iterator-managed reusable row used when Next is called with
	// a nil dst. It grows on demand and is overwritten by every Next call.
	buf Row

	// Block cursor: reuses the parsed rows directory while consecutive rows
	// fall in the same block, avoiding a per-row full-block parse.
	curBlockID uint64
	curBlk     *index.BlockLoc
	curPayload *block.RowsIndex

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
	a := &h[i].keys[h[i].pos]
	b := &h[j].keys[h[j].pos]
	if a.RowID != b.RowID {
		return a.RowID < b.RowID
	}
	return h[i].depth < h[j].depth
}
func (h rowHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *rowHeap) Push(x any)   { *h = append(*h, x.(*layerIter)) }
func (h *rowHeap) Pop() any     { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

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

// Next advances to the next visible row. The returned Row is decoded into an
// iterator-managed buffer that is reused across calls (allocated once, grown
// as needed), so a full scan allocates no per-row Row slices. The returned
// Row is valid only until the next call to Next on this iterator; values that
// must outlive it must be copied (getters of String/Bytes/Decimal return
// copies, so reading through them is always safe; retained Value structs may
// observe overwritten Decimals). It returns (nil, false) at the end; call
// Err to distinguish completion from failure. It is not safe for concurrent
// use.
func (it *Iterator) Next() (Row, bool) {
	if it.closed || it.err != nil {
		return nil, false
	}
	if it.ctx != nil {
		select {
		case <-it.ctx.Done():
			it.err = it.ctx.Err()
			return nil, false
		default:
		}
	}
	rowID, loc, ok := it.nextLoc()
	if !ok {
		return nil, false
	}
	row, err := it.rowAt(loc, it.buf)
	if err != nil {
		it.err = err
		return nil, false
	}
	it.curRowID = rowID
	it.curLoc = loc
	it.buf = row
	return row, true
}

// nextLoc advances the k-way merge and returns the next visible row location
// after applying range and tombstone filtering. It reports ok=false at the
// end of the scan or when the EndRowID bound is reached. The returned pointer
// aliases the immutable row shard.
func (it *Iterator) nextLoc() (RowID, *index.RowLoc, bool) {
	for it.heap.Len() > 0 {
		winner := heap.Pop(&it.heap).(*layerIter)
		ent := &winner.keys[winner.pos]
		rowID := ent.RowID
		loc := &ent.Loc
		// All layers currently at rowID lose; pop them and advance. The heap
		// tiebreak ensures the winner is the shallowest layer.
		for it.heap.Len() > 0 && it.heap[0].keys[it.heap[0].pos].RowID == rowID {
			l := heap.Pop(&it.heap).(*layerIter)
			l.advance(&it.heap)
		}
		winner.advance(&it.heap)
		// Range filtering.
		if it.opts.StartRowID > 0 && rowID < it.opts.StartRowID {
			continue
		}
		if it.opts.EndRowID > 0 && rowID >= it.opts.EndRowID {
			return 0, nil, false
		}
		if loc.ChangeType == fileformat.ChangeDelete {
			continue // tombstone: hide the row entirely
		}
		return rowID, loc, true
	}
	return 0, nil, false
}

func (l *layerIter) advance(h *rowHeap) {
	l.pos++
	if l.pos < len(l.keys) {
		heap.Push(h, l)
	}
}

// rowAt resolves one row into dst, reusing the parsed payload of the current
// block when the location is inside it.
func (it *Iterator) rowAt(loc *index.RowLoc, dst Row) (Row, error) {
	if err := it.locateBlock(loc); err != nil {
		return nil, err
	}
	return it.store.rowFromPayloadInto(it.curPayload, it.curBlk, loc, it.state.schemas, dst)
}

// locateBlock loads and parses the rows directory of loc's block, reusing the
// parsed payload of the current block when consecutive rows fall inside it.
func (it *Iterator) locateBlock(loc *index.RowLoc) error {
	if it.curBlockID == loc.BlockID {
		return nil
	}
	bl := it.state.view.Block(loc.BlockID)
	if bl == nil {
		return fmt.Errorf("rowpack: block %d missing from view", loc.BlockID)
	}
	blk, err := it.store.loader.Load(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return err
	}
	rp, err := block.ParseRowsDirectory(blk.Raw, bl.ItemCount)
	if err != nil {
		return err
	}
	it.curBlockID = loc.BlockID
	it.curBlk = bl
	it.curPayload = rp
	return nil
}

// RowID returns the current row's RowID.
func (it *Iterator) RowID() RowID { return it.curRowID }

// Err returns the first error encountered, or nil on clean completion.
func (it *Iterator) Err() error { return it.err }

// Close releases the iterator. It is idempotent.
func (it *Iterator) Close() error {
	it.closed = true
	return nil
}
