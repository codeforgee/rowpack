package rowpack

import (
	"container/heap"
	"context"
	"fmt"
	"unsafe"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
)

// ScanOptions bounds a Scan.
type ScanOptions struct {
	StartRowID RowID // inclusive; 0 = from the beginning
	EndRowID   RowID // exclusive; 0 = no upper bound
}

// IterArenaChunkSize bounds the per-iterator string arena chunks; chunks are
// append-only and swapped (never grown in place) so previously handed-out
// string views stay valid even when the arena's current chunk rotates.
const iterArenaChunkSize = 32 << 10

// strArena is the append-only string arena shared by the Scan and batch
// iterators: materialized String payloads become zero-copy views into the
// current chunk. Views keep the chunk alive via GC, so a string handed out
// earlier stays valid even after the arena rotates to a fresh chunk (chunks
// are never overwritten, only dropped).
type strArena struct {
	chunk []byte
}

// materialize copies payload into the arena and returns a view into it.
func (a *strArena) materialize(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if len(a.chunk)+len(payload) > cap(a.chunk) {
		// Rotate to a fresh chunk with at least enough room; never grow in
		// place, because views into the old chunk may still be referenced.
		c := iterArenaChunkSize
		if len(payload) > c {
			c = len(payload)
		}
		a.chunk = make([]byte, 0, c)
	}
	off := len(a.chunk)
	a.chunk = append(a.chunk, payload...)
	return unsafe.String(&a.chunk[off], len(payload))
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

	// sink materializes decoded String payloads as append-only views into
	// the arena (strArena), eliminating the per-row string copy allocation.
	// See strArena for the view-lifetime guarantee.
	sink  codec.StringSink
	arena strArena

	// Block cursor: reuses the parsed rows directory while consecutive rows
	// fall in the same block, avoiding a per-row full-block parse. The
	// directory entry slice is reused across blocks (ParseRowsDirectory's
	// entries argument) to keep a whole scan allocation-free apart from the
	// string arena.
	curBlockID uint64
	curBlk     *index.BlockLoc
	curPayload *block.RowsIndex
	dirEntries []fileformat.RowDirectoryEntry
	// curRef is the current block reference. Cache hits are cache-owned
	// (Release no-op); transient misses own a pooled scratch that is
	// returned when the cursor moves to the next block or the iterator
	// closes.
	curRef *scanRef

	err    error
	closed bool
}

// strArenaSink binds a StringSink to an append-only arena; shared by the
// Scan and batch iterators and pre-bound once at iterator creation so
// per-row decodes never allocate a method value.
func strArenaSink(a *strArena) codec.StringSink {
	return func(payload []byte) string { return a.materialize(payload) }
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
	// Pre-bind the string-arena sink once (a method value allocated per
	// expression evaluation would otherwise cost one allocation per Next).
	it.sink = strArenaSink(&it.arena)
	if len(it.layers) > 1 {
		heap.Init(&it.heap)
		for _, l := range it.layers {
			if l.pos < len(l.keys) {
				heap.Push(&it.heap, l)
			}
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
			it.releaseBlock()
			return nil, false
		default:
		}
	}
	rowID, loc, ok := it.nextLoc()
	if !ok {
		it.releaseBlock()
		return nil, false
	}
	row, err := it.rowAt(loc, it.buf)
	if err != nil {
		it.err = err
		it.releaseBlock()
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
// aliases the immutable row shard. A single-layer scan (the common FULL case)
// walks the sorted shard linearly and skips the heap machinery.
func (it *Iterator) nextLoc() (RowID, *index.RowLoc, bool) {
	if len(it.layers) == 1 {
		l := it.layers[0]
		keys := l.keys
		for l.pos < len(keys) {
			ent := &keys[l.pos]
			l.pos++
			if it.opts.StartRowID > 0 && ent.RowID < it.opts.StartRowID {
				continue
			}
			if it.opts.EndRowID > 0 && ent.RowID >= it.opts.EndRowID {
				return 0, nil, false
			}
			if ent.Loc.ChangeType == fileformat.ChangeDelete {
				continue // tombstone: hide the row entirely
			}
			return ent.RowID, &ent.Loc, true
		}
		return 0, nil, false
	}
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
	return it.store.rowFromPayloadInto(it.curPayload, it.curBlk, loc, it.state.schemas, dst, it.sink)
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
	ref, _, err := it.store.loader.LoadScan(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return err
	}
	rp, err := block.ParseRowsDirectory(ref.Raw(), bl.ItemCount, it.dirEntries)
	if err != nil {
		ref.Release()
		return err
	}
	// The new directory is built from ref's buffer; the previous block (if
	// any) is no longer referenced, so its scratch can be returned to the
	// pool before the cursor moves.
	it.releaseBlock()
	it.dirEntries = rp.Entries
	it.curBlockID = loc.BlockID
	it.curBlk = bl
	it.curPayload = rp
	it.curRef = ref
	return nil
}

// releaseBlock returns the current block's scratch (if any) to the pool.
// Callers must no longer reference curPayload's raw buffer. Idempotent.
func (it *Iterator) releaseBlock() {
	if it.curRef != nil {
		it.curRef.Release()
		it.curRef = nil
	}
}

// RowID returns the current row's RowID.
func (it *Iterator) RowID() RowID { return it.curRowID }

// Err returns the first error encountered, or nil on clean completion.
func (it *Iterator) Err() error { return it.err }

// Close releases the iterator. It is idempotent.
func (it *Iterator) Close() error {
	it.closed = true
	it.releaseBlock()
	return nil
}
