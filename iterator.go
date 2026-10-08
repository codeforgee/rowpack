package rowpack

import (
	"context"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
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

	// merge is the k-way merge over the snapshot's parent-chain layers
	// (index.MergeIter: shallowest layer wins RowID ties); nextLoc applies
	// range and tombstone filtering on top.
	merge *index.MergeIter

	curRowID RowID
	curType  format.ChangeType // current record's change kind

	// buf is the iterator-managed reusable row used when Next is called with
	// a nil dst. It grows on demand and is overwritten by every Next call.
	buf Row

	// sink materializes decoded String/Bytes payloads as append-only views
	// into the arena (strArena), eliminating per-row payload copy
	// allocations. See strArena for the view-lifetime guarantee.
	//
	// The arena is a separate heap object, NOT an inline field: the sink's
	// closures capture &arena, and a back-pointer into this finalizer-bearing
	// struct would form a cycle that includes the finalizer block — such
	// cycles are not guaranteed to be collected (runtime.SetFinalizer), so a
	// leaked iterator would pin the store's read lock forever.
	sink  *codec.Sink
	arena *strArena

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

	// decoders memoizes the schema-bound decoder for the current block and
	// schema version (see blockDecoders).
	decoders blockDecoders

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
		String: func(payload []byte) string {
			b := a.materializeBytes(payload)
			if len(b) == 0 {
				return ""
			}
			return unsafe.String(&b[0], len(b))
		},
		Bytes: func(payload []byte) []byte { return a.materializeBytes(payload) },
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
		// Rotate to a fresh chunk with at least enough room; never grow in
		// place, because views into the old chunk may still be referenced.
		c := max(len(payload), iterArenaChunkSize)
		a.chunk = make([]byte, 0, c)
	}
	off := len(a.chunk)
	a.chunk = append(a.chunk, payload...)
	return a.chunk[off:len(a.chunk):len(a.chunk)]
}

// reset rewinds the current chunk so the next batch reuses its capacity.
// Views handed out before the reset are invalidated, which is exactly the
// batchBuffer reuse contract; the iterator never calls it.
func (a *strArena) reset() { a.chunk = a.chunk[:0] }

// Scan opens an iterator over the visible rows of the named table at
// snapshot: the parent-chain merged view with tombstones filtered out.
//
// The iterator holds the store's read lock until Close: Store.Close waits for
// open iterators. A leaked (never-closed) iterator is eventually released by
// its GC finalizer, which unblocks a blocked Store.Close — but never rely on
// finalization: defer Close immediately after Scan.
func (s *Store) Scan(ctx context.Context, snapshot SnapshotID, table string, opts ScanOptions) (*Iterator, error) {
	s.readMu.RLock()
	it, err := s.scanLocked(ctx, snapshot, table, opts)
	if err != nil {
		s.readMu.RUnlock()
		return nil, err
	}
	return it, nil // the read lock transfers to the iterator
}

// scanLocked builds the merge-mode iterator. The caller holds the read lock;
// on success it transfers to the returned iterator (readHeld).
func (s *Store) scanLocked(ctx context.Context, snapshot SnapshotID, table string, opts ScanOptions) (*Iterator, error) {
	rc, err := s.resolveRead(snapshot, table)
	if err != nil {
		return nil, err
	}
	if opts.Start > 0 && opts.End > 0 && opts.Start >= opts.End {
		return nil, fmt.Errorf("%w: scan start %d >= end %d", ErrInvalidArgument, opts.Start, opts.End)
	}
	view := rc.st.view
	// Build the parent chain layers (target snapshot first); the merge
	// iterator dedups by RowID with the shallowest layer winning.
	sm := view.Snapshot(uint64(snapshot))
	layers := make([]*index.RowKeyIter, 0, len(sm.Chain()))
	for _, cur := range sm.Chain() {
		keys := view.RowIter(cur, uint32(rc.tid))
		if keys != nil && keys.Len() > 0 {
			if opts.Start > 0 {
				keys.Seek(uint64(opts.Start))
			}
			layers = append(layers, keys)
		}
	}
	it := &Iterator{store: s, state: rc.st, ctx: ctx, snapshot: snapshot, table: rc.tid, opts: opts, mode: scanModeMerge, merge: index.NewMergeIter(layers...), readHeld: true}
	// Pre-bind the string-arena sink once (a method value allocated per
	// expression evaluation would otherwise cost one allocation per Next).
	// The arena must live outside the Iterator allocation: strArenaSink's
	// closures capture &arena, and an inline arena would create a
	// self-reference cycle through the finalizer-bearing object, which the
	// GC is not guaranteed to collect (see the field comment above).
	it.arena = &strArena{}
	it.sink = strArenaSink(it.arena)
	runtime.SetFinalizer(it, (*Iterator).finish)
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
				it.fail(err)
				it.releasePage()
				return nil, false
			}
			it.curPageNext++
			it.curRowID = RowID(rec.RowID)
			it.curType = rec.ChangeType
			if rec.ChangeType == format.ChangeDelete {
				return nil, true // tombstone: no payload
			}
			decoder, err := it.decoders.forVersion(rec.SchemaVersion)
			if err != nil {
				it.fail(err)
				it.releasePage()
				return nil, false
			}
			row, err := decoder.DecodeInto(it.buf, rec.Body, it.sink)
			if err != nil {
				it.fail(err)
				it.releasePage()
				return nil, false
			}
			it.buf = row
			return row, true
		}
		// Page exhausted: advance to the next page within the block, then the
		// next block. next must be captured before releasePage, which resets
		// curPageIdx to -1.
		if it.curContainer != nil && it.curPageIdx+1 < it.curContainer.PageCount() {
			next := it.curPageIdx + 1
			it.releasePage()
			page, release, err := it.curContainer.PageScratch(next)
			if err != nil {
				it.fail(err)
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
	it.decoders.bind(it.state.schemas, bl)
	return nil
}

// nextLoc advances the k-way merge and returns the next visible row location
// after applying range and tombstone filtering. It reports ok=false at the
// end of the scan or when the EndRowID bound is reached.
func (it *Iterator) nextLoc() (RowID, index.RowLoc, bool) {
	for {
		rowID, loc, ok := it.merge.Next()
		if !ok {
			return 0, index.RowLoc{}, false
		}
		// Range filtering: layers were Seek'ed to Start at scan setup, so the
		// cursor can never yield a row below Start — only the End bound needs
		// checking.
		if it.opts.End > 0 && rowID >= uint64(it.opts.End) {
			return 0, index.RowLoc{}, false
		}
		if loc.ChangeType == format.ChangeDelete {
			continue // tombstone: hide the row entirely (never for FULL-only layers)
		}
		return RowID(rowID), loc, true
	}
}

// rowAt resolves one row into dst, reusing the current block's page container
// and the currently decompressed page when the location is inside it. Every
// failure is classified as a CorruptionError for the iterator's snapshot and
// table.
func (it *Iterator) rowAt(loc index.RowLoc, dst Row) (Row, error) {
	if err := it.locateBlock(loc); err != nil {
		return nil, err
	}
	pi, err := it.curContainer.PageFor(loc.ItemOrdinal)
	if err != nil {
		return nil, it.store.recordError(it.curBlk, it.snapshot, it.table, err)
	}
	if it.curPage == nil || it.curPageIdx != pi {
		it.releasePage()
		page, release, err := it.curContainer.PageScratch(pi)
		if err != nil {
			return nil, it.store.recordError(it.curBlk, it.snapshot, it.table, err)
		}
		it.curPage = page
		it.curPageRel = release
		it.curPageIdx = pi
	}
	rec, err := it.curPage.RecordAt(loc.ItemOrdinal - it.curContainer.Dir[pi].FirstRecordOrdinal)
	if err != nil {
		return nil, it.store.recordError(it.curBlk, it.snapshot, it.table, err)
	}
	decoder, err := it.decoders.forVersion(rec.SchemaVersion)
	if err != nil {
		return nil, it.store.recordError(it.curBlk, it.snapshot, it.table, err)
	}
	row, err := decoder.DecodeInto(dst, rec.Body, it.sink)
	if err != nil {
		return nil, it.store.recordError(it.curBlk, it.snapshot, it.table, err)
	}
	return row, nil
}

// fail latches a record-level read failure on the iterator, classified against
// the current block/snapshot/table context. Callers must not clear it.curBlk
// before calling it when the block identity matters.
func (it *Iterator) fail(err error) {
	it.err = it.store.recordError(it.curBlk, it.snapshot, it.table, err)
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
	it.decoders.bind(it.state.schemas, bl)
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
	runtime.SetFinalizer(it, nil)
	it.finish()
	return nil
}

// finish releases the read lock and block cursor exactly once. It is
// finalizer-safe: it only touches the store's readMu and iterator state.
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
