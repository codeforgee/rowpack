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
	Start RowID // inclusive; 0 = from the beginning
	End   RowID // exclusive; 0 = no upper bound
}

// IterArenaChunkSize bounds the per-iterator string arena chunks; chunks are
// append-only and swapped (never grown in place) so previously handed-out
// string views stay valid even when the arena's current chunk rotates.
// measured (M1 Pro, scan 100k×7): 16 KiB / 32 KiB / 256 KiB all within noise
// (~14.3 ms) — the cost is mallocgc zeroing + memmove of the materialized
// bytes themselves, invariant to chunk size; chunks cannot be recycled
// across iterators because handed-out string views alias them.
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

// scanMode selects the iterator traversal strategy.
type scanMode uint8

const (
	scanModeMerge  scanMode = iota // parent-chain merged visible rows
	scanModeBlocks                 // raw per-block change stream
)

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
	mode     scanMode

	layers []*layerIter
	heap   rowHeap

	curRowID RowID
	curType  fileformat.ChangeType // current record's change kind

	// buf is the iterator-managed reusable row used when Next is called with
	// a nil dst. It grows on demand and is overwritten by every Next call.
	buf Row

	// sink materializes decoded String/Bytes payloads as append-only views
	// into the arena (strArena), eliminating per-row payload copy
	// allocations. See strArena for the view-lifetime guarantee.
	sink  *codec.Sink
	arena strArena

	// Page container cursor: keeps the current block's page container and the
	// currently decompressed page, so consecutive rows inside one page reuse
	// the same decompression instead of re-decoding the page per row.
	curBlockID   uint64
	curBlk       *index.BlockLoc
	curContainer *block.RowsContainer
	curPage      *block.RowsPage
	curPageIdx   int    // index into curContainer.Dir; -1 = none loaded
	curPageRel   func() // returns the current page's pooled scratch
	curPageNext  int    // block-scan: next record ordinal within page

	// curSchema memoizes the codec schema resolved for the current block
	// cursor keyed by the record's SchemaVersion: the alternative is a
	// two-level map walk per record, while records within a block
	// overwhelmingly share one version. Invalidated whenever the block cursor
	// moves.
	curSchemaVer uint32
	curSchema    *codec.Schema
	curSchemaOK  bool

	// Block-scan mode (scanModeBlocks): the raw per-block change stream.
	blockIDs []uint64 // blocks to visit, ascending
	blockPos int      // index into blockIDs
	err      error
	closed   bool
	readHeld bool
}

// strArenaSink binds an append-only arena as the decode Sink: String and
// Bytes payloads become zero-copy views into arena chunks (chunks rotate but
// are never overwritten), eliminating per-row payload copies on scans. Bound
// once at iterator creation so per-row decodes never allocate a sink.
func strArenaSink(a *strArena) *codec.Sink {
	return &codec.Sink{
		String: func(payload []byte) string { return a.materialize(payload) },
		Bytes:  func(payload []byte) []byte { return a.materializeBytes(payload) },
	}
}

// materializeBytes copies payload into the arena and returns a full-slice
// view into it (cap == len, so callers cannot append past the view and
// clobber neighbouring materializations). Views stay valid across chunk
// rotation for the same lifetime reason as materialize.
func (a *strArena) materializeBytes(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	if len(a.chunk)+len(payload) > cap(a.chunk) {
		c := iterArenaChunkSize
		if len(payload) > c {
			c = len(payload)
		}
		a.chunk = make([]byte, 0, c)
	}
	off := len(a.chunk)
	a.chunk = append(a.chunk, payload...)
	return a.chunk[off:len(a.chunk):len(a.chunk)]
}

// layerIter walks one snapshot layer's sorted incremental row index. It holds
// a RowKeyIter over the compact shard (no materialized []RowKeyLoc, so a scan
// does not allocate a transient 24 B/row copy per layer).
type layerIter struct {
	keys  *index.RowKeyIter
	depth int // 0 = target snapshot; larger = ancestor
}

// rowHeap is a min-heap over the current head of each layer, keyed by RowID
// with depth as tiebreaker (shallowest wins).
type rowHeap []*layerIter

func (h rowHeap) Len() int { return len(h) }
func (h rowHeap) Less(i, j int) bool {
	if h[i].keys.RowID() != h[j].keys.RowID() {
		return h[i].keys.RowID() < h[j].keys.RowID()
	}
	return h[i].depth < h[j].depth
}
func (h rowHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *rowHeap) Push(x any)   { *h = append(*h, x.(*layerIter)) }
func (h *rowHeap) Pop() any     { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// Scan opens an iterator over the visible rows of the named table at
// snapshot: the parent-chain merged view with tombstones filtered out.
func (s *Store) Scan(ctx context.Context, snapshot SnapshotID, table string, opts ScanOptions) (*Iterator, error) {
	s.readMu.RLock()
	keepLock := false
	defer func() {
		if !keepLock {
			s.readMu.RUnlock()
		}
	}()
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if st.view.Snapshot(uint64(snapshot)) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableIDByName(uint64(snapshot), table)
	if !ok {
		return nil, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	view := st.view
	if opts.Start > 0 && opts.End > 0 && opts.Start >= opts.End {
		return nil, fmt.Errorf("%w: scan start %d >= end %d", ErrInvalidArgument, opts.Start, opts.End)
	}
	it := &Iterator{store: s, state: st, ctx: ctx, snapshot: snapshot, table: tid, opts: opts, mode: scanModeMerge, readHeld: true}
	// Build the parent chain layers (target snapshot first).
	cur := snapshot
	for depth := 0; ; depth++ {
		keys := view.RowIter(cur, uint32(tid))
		if keys != nil && keys.Len() > 0 {
			if opts.Start > 0 {
				keys.Seek(uint64(opts.Start))
			}
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
			if !l.keys.Done() {
				heap.Push(&it.heap, l)
			}
		}
	}
	keepLock = true
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
	if it.mode == scanModeBlocks {
		row, ok := it.nextBlockRecord()
		if !ok {
			it.finish()
		}
		return row, ok
	}
	rowID, loc, ok := it.nextLoc()
	if !ok {
		it.finish()
		return nil, false
	}
	row, err := it.rowAt(loc, it.buf)
	if err != nil {
		it.err = err
		it.finish()
		return nil, false
	}
	it.curRowID = rowID
	it.curType = loc.ChangeType
	it.buf = row
	return row, true
}

// nextBlockRecord yields the raw per-record change stream of the selected
// blocks in physical write order: every record, tombstones included, no
// parent-chain merge. DELETE records carry a nil Row. Pages are decompressed
// once each and walked sequentially.
func (it *Iterator) nextBlockRecord() (Row, bool) {
	for {
		if it.curContainer != nil && it.curPage != nil && it.curPageNext < int(it.curPage.Header().EntryCount) {
			rec, err := it.curPage.RecordAt(uint32(it.curPageNext))
			if err != nil {
				it.err = err
				it.releasePage()
				return nil, false
			}
			it.curPageNext++
			it.curRowID = RowID(rec.RowID)
			it.curType = rec.ChangeType
			if rec.ChangeType == fileformat.ChangeDelete {
				return nil, true // tombstone: no payload
			}
			schema, err := it.schemaFor(rec.SchemaVersion)
			if err != nil {
				it.err = err
				it.releasePage()
				return nil, false
			}
			row, err := codec.DecodeBodyInto(it.buf, rec.Body, schema, it.store.opts.codecLimits(), it.sink)
			if err != nil {
				it.err = err
				it.releasePage()
				return nil, false
			}
			it.buf = row
			return row, true
		}
		// Page exhausted: advance to the next page within the block, then the
		// next block.
		if it.curContainer != nil && it.curPageIdx+1 < it.curContainer.PageCount() {
			it.releasePage()
			next := it.curPageIdx + 1
			page, release, err := it.curContainer.PageScratch(next)
			if err != nil {
				it.err = err
				return nil, false
			}
			it.curPage = page
			it.curPageRel = release
			it.curPageIdx = next
			it.curPageNext = 0
			continue
		}
		// Advance to the next block.
		it.releaseBlock()
		if it.blockPos >= len(it.blockIDs) {
			return nil, false
		}
		bid := it.blockIDs[it.blockPos]
		it.blockPos++
		bl := it.state.view.Block(bid)
		if bl == nil {
			it.err = fmt.Errorf("rowpack: block %d missing from view", bid)
			return nil, false
		}
		if err := it.loadBlock(bl); err != nil {
			it.err = err
			return nil, false
		}
	}
}

// loadBlock loads one rows block's page container for the block-scan cursor;
// the first page is decompressed lazily on the next record.
func (it *Iterator) loadBlock(bl *index.BlockLoc) error {
	rc, err := it.store.loader.LoadScanRows(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return err
	}
	it.curBlockID = bl.BlockID
	it.curBlk = bl
	it.curContainer = rc
	it.curPage = nil
	it.curPageRel = nil
	it.curPageIdx = -1
	it.curPageNext = 0
	it.curSchemaOK = false
	return nil
}

// nextLoc advances the k-way merge and returns the next visible row location
// after applying range and tombstone filtering. It reports ok=false at the
// end of the scan or when the EndRowID bound is reached. The returned pointer
// aliases the immutable row shard. A single-layer scan (the common FULL case)
// walks the sorted shard linearly and skips the heap machinery.
func (it *Iterator) nextLoc() (RowID, index.RowLoc, bool) {
	if len(it.layers) == 1 {
		l := it.layers[0]
		for !l.keys.Done() {
			rowID := l.keys.RowID()
			loc := l.keys.Loc()
			l.keys.Next()
			if it.opts.Start > 0 && rowID < it.opts.Start {
				continue
			}
			if it.opts.End > 0 && rowID >= it.opts.End {
				return 0, index.RowLoc{}, false
			}
			if loc.ChangeType == fileformat.ChangeDelete {
				continue // tombstone: hide the row entirely
			}
			return rowID, loc, true
		}
		return 0, index.RowLoc{}, false
	}
	for it.heap.Len() > 0 {
		winner := heap.Pop(&it.heap).(*layerIter)
		rowID := winner.keys.RowID()
		loc := winner.keys.Loc()
		// All layers currently at rowID lose; pop them and advance. The heap
		// tiebreak ensures the winner is the shallowest layer.
		for it.heap.Len() > 0 && it.heap[0].keys.RowID() == rowID {
			l := heap.Pop(&it.heap).(*layerIter)
			l.advance(&it.heap)
		}
		winner.advance(&it.heap)
		// Range filtering.
		if it.opts.Start > 0 && rowID < it.opts.Start {
			continue
		}
		if it.opts.End > 0 && rowID >= it.opts.End {
			return 0, index.RowLoc{}, false
		}
		if loc.ChangeType == fileformat.ChangeDelete {
			continue // tombstone: hide the row entirely
		}
		return rowID, loc, true
	}
	return 0, index.RowLoc{}, false
}

func (l *layerIter) advance(h *rowHeap) {
	l.keys.Next()
	if !l.keys.Done() {
		heap.Push(h, l)
	}
}

// rowAt resolves one row into dst, reusing the current block's page container
// and the currently decompressed page when the location is inside it.
func (it *Iterator) rowAt(loc index.RowLoc, dst Row) (Row, error) {
	if err := it.locateBlock(loc); err != nil {
		return nil, err
	}
	pi, err := it.curContainer.PageIndexForOrdinal(loc.ItemOrdinal)
	if err != nil {
		return nil, err
	}
	if it.curPage == nil || it.curPageIdx != pi {
		it.releasePage()
		page, release, err := it.curContainer.PageScratch(pi)
		if err != nil {
			return nil, err
		}
		it.curPage = page
		it.curPageRel = release
		it.curPageIdx = pi
	}
	rec, err := it.curPage.RecordAt(loc.ItemOrdinal - it.curContainer.Dir[pi].FirstRecordOrdinal)
	if err != nil {
		return nil, err
	}
	schema, err := it.schemaFor(rec.SchemaVersion)
	if err != nil {
		return nil, err
	}
	return codec.DecodeBodyInto(dst, rec.Body, schema, it.store.opts.codecLimits(), it.sink)
}

// schemaFor returns the codec schema for the given record schema version
// under the current block cursor, memoizing the two-level map walk until the
// block cursor moves or the version differs. The cursor must be loaded.
func (it *Iterator) schemaFor(ver uint32) (*codec.Schema, error) {
	if it.curSchemaOK && it.curSchemaVer == ver {
		return it.curSchema, nil
	}
	schema, err := it.state.schemas.schemaFor(it.curBlk, ver)
	if err != nil {
		return nil, err
	}
	it.curSchemaVer = ver
	it.curSchema = schema
	it.curSchemaOK = true
	return schema, nil
}

// locateBlock loads the page container of loc's block, reusing the current
// block when consecutive rows fall inside it.
func (it *Iterator) locateBlock(loc index.RowLoc) error {
	if it.curBlockID == loc.BlockID {
		return nil
	}
	bl := it.state.view.Block(loc.BlockID)
	if bl == nil {
		return fmt.Errorf("rowpack: block %d missing from view", loc.BlockID)
	}
	it.releaseBlock()
	rc, err := it.store.loader.LoadScanRows(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return err
	}
	it.curBlockID = loc.BlockID
	it.curBlk = bl
	it.curContainer = rc
	it.curPage = nil
	it.curPageRel = nil
	it.curPageIdx = -1
	it.curSchemaOK = false
	return nil
}

// releasePage returns the current page's pooled scratch to the pool. It must
// be called before curContainer or curBlock moves on. Idempotent.
func (it *Iterator) releasePage() {
	if it.curPageRel != nil {
		it.curPageRel()
		it.curPageRel = nil
	}
	it.curPage = nil
	it.curPageIdx = -1
}

// releaseBlock releases the current page and drops the block cursor.
// Callers must no longer reference curPage or curContainer. Idempotent.
func (it *Iterator) releaseBlock() {
	it.releasePage()
	it.curContainer = nil
	it.curBlk = nil
	it.curBlockID = 0
}

// RowID returns the current row's RowID.
func (it *Iterator) RowID() RowID { return it.curRowID }

// ChangeType returns the current record's change kind. The merged Scan view
// never yields ChangeDelete; ScanBlocks reports each record's raw kind.
func (it *Iterator) ChangeType() ChangeType { return ChangeType(it.curType) }

// Err returns the first error encountered, or nil on clean completion.
func (it *Iterator) Err() error { return it.err }

// Close releases the iterator. It is idempotent.
func (it *Iterator) Close() error {
	it.finish()
	return nil
}

func (it *Iterator) finish() {
	if it.closed {
		return
	}
	it.closed = true
	it.releaseBlock()
	if it.readHeld {
		it.readHeld = false
		it.store.readMu.RUnlock()
	}
}
