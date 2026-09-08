package rowpack

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
)

// V2-M4 batch reads: plan once over the immutable index view (resolve every
// requested row through the FULL/DELTA parent chain, group winners by block,
// order blocks by physical offset), then decode each block exactly once per
// request and stream rows in the requested order through a bounded reorder
// buffer (BINARY_FORMAT_V2 §11, GO_API_DESIGN_V2 §4/§6).
//
// Semantics pinned by the design review (R17/R18):
//   - duplicate input RowIDs are returned once per occurrence, 1:1 with the
//     input positions (v1 ReadBatch semantics; never silently deduplicated);
//   - invisible rows (missing or deleted) are skipped, never an error; the
//     RequestedIDs/RequestedRanges vs RowsReturned stats expose the diff;
//   - the reorder buffer is bounded by the plan itself, so a batch never
//     materializes more than its own row count;
//   - PrefetchBlocks/Parallelism are accepted for API stability; M4 decodes
//     sequentially through the bounded scan window, M5 adds parallel workers
//     that feed the same ordered emission pipeline.

// ErrInvalidRange is returned by ReadRowRanges for malformed ranges.
var ErrInvalidRange = errors.New("rowpack: invalid row id range")

// ErrBatchLimit is returned when a batch request exceeds MaxRows/MaxBytes.
var ErrBatchLimit = errors.New("rowpack: batch limit exceeded")

// RowIDRange is an inclusive-exclusive RowID span: [Start, End).
type RowIDRange struct {
	Start RowID // inclusive; 0 = from the smallest RowID
	End   RowID // exclusive; must be > Start (ReadRowRanges rejects 0)
}

// BatchOrder selects the emission order of a batch iterator.
type BatchOrder uint8

const (
	// BatchOrderRowID emits rows in ascending RowID order (default).
	BatchOrderRowID BatchOrder = iota
	// BatchOrderInput emits rows in input order; only valid for
	// ReadRowsByIDs. Duplicate inputs map 1:1 to duplicate outputs.
	BatchOrderInput
)

// BatchReadOptions bounds and orders a batch request. Zero values mean
// unlimited / defaults.
type BatchReadOptions struct {
	Order BatchOrder
	// MaxRows caps the number of resolved rows; exceeding fails the request
	// with ErrBatchLimit. 0 = unlimited.
	MaxRows uint64
	// MaxBytes caps the decoded working set (sum of candidate block raw
	// bytes); exceeding fails with ErrBatchLimit. 0 = unlimited.
	MaxBytes uint64
	// PrefetchBlocks hints how many blocks to keep decoded ahead of the
	// emission point. Accepted; M4 decodes sequentially through the scan
	// window, so the hint only caps that window's reuse.
	PrefetchBlocks int
	// Parallelism is reserved for parallel block decoding (M5); accepted but
	// currently ignored (sequential decode).
	Parallelism int
}

// BatchReadStats quantifies one batch request for performance diagnostics.
// It is not transactional information.
type BatchReadStats struct {
	RequestedIDs    uint64 // ReadRowsByIDs input length
	RequestedRanges uint64 // ReadRowRanges input length (before merging)
	MergedRanges    uint64 // after normalize/merge
	CandidateBlocks uint64 // distinct blocks referenced by winners
	BlocksRead      uint64 // blocks actually loaded (= CandidateBlocks on success)
	CacheHits       uint64 // block loads served from a cache
	StoredBytesRead uint64
	RawBytesDecoded uint64
	RowsReturned    uint64
}

// batchItem is one resolved row: where it lives and where it must be emitted.
type batchItem struct {
	emitPos int // position in the requested output order (a permutation)
	rowID   RowID
	blockID uint64
	ordinal uint32
}

// batchBlock groups items sharing one block, in physical file order.
type batchBlock struct {
	blockID    uint64
	offset     int64
	itemIdx    []int // indices into plan.items
	rawSize    uint32
	storedSize uint32
}

// batchPlan is the fully-resolved read plan (index-only work).
type batchPlan struct {
	items  []batchItem  // decode order: blocks physical, ordinals ascending
	blocks []batchBlock // physical DataOffset order
	emit   []int        // emit order: indices into items
}

// batchBufItem is one decoded-but-not-yet-emitted row. payload aliases the
// owning block's raw buffer, kept alive by the block's scanRef until every
// item of that block has been emitted (refIdx/refPending below).
type batchBufItem struct {
	emitPos int
	payload []byte
	schema  *codec.Schema
	rowID   RowID
	refIdx  int
}

// blockResult is one decoded block, produced by the sequential loop or a
// parallel worker and assembled in block order by Next.
type blockResult struct {
	blockIdx int
	items    []batchBufItem // emitPos-ordered payloads aliasing the block raw
	ref      *scanRef
	hit      bool
	err      error
}

// BatchIterator streams the rows of one batch request. It is not safe for
// concurrent use. Next returns rows in the requested order; rows invisible at
// the snapshot (missing or deleted) are skipped and never emitted.
type BatchIterator struct {
	store *Store
	st    *publishedState
	ctx   context.Context
	opts  BatchReadOptions

	plan     *batchPlan
	pending  map[int]batchBufItem // emitPos -> decoded item awaiting emission
	nextEmit int

	// block decode cursor
	blockIdx int
	// refs hold scan windows alive while any of their items are buffered;
	// refPending[i] counts buffered items owned by refs[i] and the ref is
	// released when it reaches zero.
	refs       []*scanRef
	refPending []int
	scratch    []fileformat.RowDirectoryEntry
	stats      BatchReadStats
	err        error
	closed     bool
	buf        Row // iterator-owned row when Next(dst) passes nil

	// sink materializes decoded String payloads into an append-only arena
	// (see iterator.go), removing the per-row string allocation. Pre-bound
	// once so decodeInto does not allocate a method value per row.
	sink  codec.StringSink
	arena strArena

	// parallel decode pipeline (Parallelism > 1, M5): workers decode blocks
	// concurrently; Next assembles results strictly in block order so the
	// emission semantics are identical to the sequential path.
	nextBlockIdx int
	resultBuf    map[int]blockResult
	jobs         chan int
	results      chan blockResult
	stop         chan struct{}
	pipelineDone chan struct{}
	stopOnce     sync.Once
	started      bool
	wg           sync.WaitGroup
}

// ReadRowsByIDs resolves an explicit RowID set at a snapshot and returns an
// iterator over the visible rows. Duplicate inputs return duplicate rows in
// 1:1 input-position mapping (R17). Rows missing or deleted at the snapshot
// are skipped (visible in Stats), never an error.
func (s *Store) ReadRowsByIDs(
	ctx context.Context,
	snapshot SnapshotID,
	table TableID,
	rowIDs []RowID,
	opts BatchReadOptions,
) (*BatchIterator, error) {
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if err := validateBatchOptions(opts, true); err != nil {
		return nil, err
	}
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}

	plan := &batchPlan{}
	it := &BatchIterator{
		store: s, st: st, ctx: ctx, opts: opts, plan: plan,
		pending:      make(map[int]batchBufItem),
		resultBuf:    make(map[int]blockResult),
		pipelineDone: make(chan struct{}),
	}
	it.sink = it.strSink
	it.stats.RequestedIDs = uint64(len(rowIDs))

	// Resolve every input position; winners keep their input index.
	type resolved struct {
		inIdx int
		loc   *index.RowLoc
		rowID RowID
	}
	winners := make([]resolved, 0, len(rowIDs))
	for i, id := range rowIDs {
		loc := view.ResolveRow(snapshot, table, id)
		if loc == nil || loc.ChangeType == fileformat.ChangeDelete {
			continue // invisible: skipped, reflected in stats
		}
		winners = append(winners, resolved{inIdx: i, loc: loc, rowID: id})
	}
	if uint64(len(winners)) > opts.MaxRows && opts.MaxRows > 0 {
		return nil, fmt.Errorf("%w: resolved rows %d > MaxRows %d", ErrBatchLimit, len(winners), opts.MaxRows)
	}

	items := make([]batchItem, len(winners))
	for i, w := range winners {
		items[i] = batchItem{rowID: w.rowID, blockID: w.loc.BlockID, ordinal: w.loc.ItemOrdinal}
	}
	// Emission order.
	switch opts.Order {
	case BatchOrderRowID:
		sort.SliceStable(items, func(i, j int) bool { return items[i].rowID < items[j].rowID })
	case BatchOrderInput:
		// winners are already in input order; items preserve it.
	}
	for pos := range items {
		items[pos].emitPos = pos
	}
	if err := it.finalizePlan(items); err != nil {
		return nil, err
	}
	return it, nil
}

// ReadRowRanges resolves one or more RowID ranges at a snapshot and returns
// an iterator over the visible rows in ascending RowID order (the only order
// defined for ranges). Ranges may overlap; they are normalized and merged
// before planning. Rows in range holes or deleted rows are skipped.
func (s *Store) ReadRowRanges(
	ctx context.Context,
	snapshot SnapshotID,
	table TableID,
	ranges []RowIDRange,
	opts BatchReadOptions,
) (*BatchIterator, error) {
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if err := validateBatchOptions(opts, false); err != nil {
		return nil, err
	}
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	it := &BatchIterator{
		store: s, st: st, ctx: ctx, opts: opts, plan: &batchPlan{},
		pending:      make(map[int]batchBufItem),
		resultBuf:    make(map[int]blockResult),
		pipelineDone: make(chan struct{}),
	}
	it.sink = it.strSink
	it.stats.RequestedRanges = uint64(len(ranges))

	merged, err := normalizeRanges(ranges)
	if err != nil {
		return nil, err
	}
	it.stats.MergedRanges = uint64(len(merged))

	// Collect (rowID, depth, loc) from every chain layer, then keep the
	// shallowest entry per RowID (newest version wins; tombstones hide).
	type spanEntry struct {
		rowID RowID
		depth int
		loc   index.RowLoc
	}
	var span []spanEntry
	cur := snapshot
	for depth := 0; ; depth++ {
		keys := view.RowKeys(cur, table)
		if len(keys) > 0 {
			for _, r := range merged {
				lo := sort.Search(len(keys), func(i int) bool { return keys[i].RowID >= r.Start })
				hi := sort.Search(len(keys), func(i int) bool { return keys[i].RowID >= r.End })
				for i := lo; i < hi; i++ {
					span = append(span, spanEntry{rowID: keys[i].RowID, depth: depth, loc: keys[i].Loc})
				}
			}
		}
		sm := view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			break
		}
		cur = sm.Parent
	}
	sort.Slice(span, func(i, j int) bool {
		if span[i].rowID != span[j].rowID {
			return span[i].rowID < span[j].rowID
		}
		return span[i].depth < span[j].depth
	})
	items := make([]batchItem, 0, len(span))
	for i := 0; i < len(span); i++ {
		if i > 0 && span[i].rowID == span[i-1].rowID {
			continue // older layer loses (span is sorted by rowID, depth)
		}
		if span[i].loc.ChangeType == fileformat.ChangeDelete {
			continue // newest version is a tombstone: row invisible
		}
		items = append(items, batchItem{rowID: span[i].rowID, blockID: span[i].loc.BlockID, ordinal: span[i].loc.ItemOrdinal})
	}
	if opts.MaxRows > 0 && uint64(len(items)) > opts.MaxRows {
		return nil, fmt.Errorf("%w: resolved rows %d > MaxRows %d", ErrBatchLimit, len(items), opts.MaxRows)
	}
	// Ranges always emit in ascending RowID order.
	for pos := range items {
		items[pos].emitPos = pos
	}
	if err := it.finalizePlan(items); err != nil {
		return nil, err
	}
	return it, nil
}

// normalizeRanges validates and merges overlapping or adjacent ranges.
func normalizeRanges(ranges []RowIDRange) ([]RowIDRange, error) {
	out := make([]RowIDRange, 0, len(ranges))
	for _, r := range ranges {
		// RowID 0 never exists, so Start==0 means "from the smallest";
		// End==0 has no meaning here (Scan-only sentinel, R18).
		if r.End == 0 || r.End <= r.Start {
			return nil, fmt.Errorf("%w: [%d,%d)", ErrInvalidRange, r.Start, r.End)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	merged := out[:0]
	for _, r := range out {
		if n := len(merged); n > 0 && r.Start <= merged[n-1].End {
			if r.End > merged[n-1].End {
				merged[n-1].End = r.End
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged, nil
}

func validateBatchOptions(opts BatchReadOptions, allowInputOrder bool) error {
	switch opts.Order {
	case BatchOrderRowID:
	case BatchOrderInput:
		if !allowInputOrder {
			return fmt.Errorf("%w: BatchOrderInput is only valid for ReadRowsByIDs", ErrInvalidArgument)
		}
	default:
		return fmt.Errorf("%w: batch order %d", ErrInvalidArgument, opts.Order)
	}
	if opts.Parallelism < 0 {
		return fmt.Errorf("%w: negative Parallelism %d", ErrInvalidArgument, opts.Parallelism)
	}
	return nil
}

// finalizePlan groups items by block, orders blocks by physical offset, and
// records the emission permutation. items must already carry emitPos. It
// returns ErrBatchLimit when the candidate working set exceeds MaxBytes.
func (it *BatchIterator) finalizePlan(items []batchItem) error {
	plan := it.plan
	plan.items = items
	plan.emit = make([]int, len(items))
	for i := range items {
		plan.emit[items[i].emitPos] = i
	}
	if len(items) == 0 {
		return nil
	}
	byBlock := make(map[uint64][]int)
	for i := range items {
		byBlock[items[i].blockID] = append(byBlock[items[i].blockID], i)
	}
	plan.blocks = make([]batchBlock, 0, len(byBlock))
	for bid, idxs := range byBlock {
		bl := it.st.view.Block(bid)
		if bl == nil {
			it.err = fmt.Errorf("rowpack: block %d missing from view", bid)
			return it.err
		}
		sort.Slice(idxs, func(a, b int) bool { return items[idxs[a]].ordinal < items[idxs[b]].ordinal })
		plan.blocks = append(plan.blocks, batchBlock{blockID: bid, offset: int64(bl.DataOffset), itemIdx: idxs, rawSize: bl.RawSize, storedSize: bl.StoredSize})
	}
	sort.Slice(plan.blocks, func(a, b int) bool { return plan.blocks[a].offset < plan.blocks[b].offset })
	it.stats.CandidateBlocks = uint64(len(plan.blocks))
	if it.opts.MaxBytes > 0 {
		var raw uint64
		for i := range plan.blocks {
			raw += uint64(plan.blocks[i].rawSize)
		}
		if raw > it.opts.MaxBytes {
			return fmt.Errorf("%w: candidate raw bytes %d > MaxBytes %d", ErrBatchLimit, raw, it.opts.MaxBytes)
		}
	}
	return nil
}

// Next advances to the next row in the requested order, decoding it into dst
// (nil dst uses an iterator-owned buffer; keep the returned Row as the next
// dst to reuse its backing storage). It returns ok=false at the end; call
// Err to distinguish completion from failure. Rows are valid until the next
// Next call on this iterator.
func (it *BatchIterator) Next(dst Row) (RowID, Row, bool) {
	if it.closed || it.err != nil {
		return 0, nil, false
	}
	if it.ctx != nil {
		select {
		case <-it.ctx.Done():
			it.err = it.ctx.Err()
			it.releaseAll()
			return 0, nil, false
		default:
		}
	}
	total := len(it.plan.items)
	for {
		if item, ok := it.pending[it.nextEmit]; ok {
			delete(it.pending, it.nextEmit)
			it.nextEmit++
			row, derr := it.decodeInto(item, dst)
			if derr != nil {
				it.err = derr
				it.releaseAll()
				return 0, nil, false
			}
			it.stats.RowsReturned++
			it.releaseRefIfDone(item.refIdx)
			return item.rowID, row, true
		}
		if it.nextEmit >= total {
			return 0, nil, false // clean end
		}
		// Need more decoded blocks: pull the next result in block order.
		if it.blockIdx >= len(it.plan.blocks) {
			it.err = fmt.Errorf("rowpack: batch plan exhausted at emit %d/%d", it.nextEmit, total)
			it.releaseAll()
			return 0, nil, false
		}
		if derr := it.nextBlockResult(); derr != nil {
			it.err = derr
			it.releaseAll()
			return 0, nil, false
		}
	}
}

// nextBlockResult obtains the next block result in physical block order —
// from the parallel pipeline (results assembled via resultBuf) or the
// sequential loop — and buffers its items.
func (it *BatchIterator) nextBlockResult() error {
	if it.opts.Parallelism > 1 {
		if !it.started {
			it.startPipeline()
		}
		for {
			if r, ok := it.resultBuf[it.nextBlockIdx]; ok {
				delete(it.resultBuf, it.nextBlockIdx)
				it.insertResult(r)
				it.nextBlockIdx++
				return nil
			}
			select {
			case r, ok := <-it.results:
				if !ok {
					return fmt.Errorf("rowpack: batch pipeline stopped early")
				}
				if r.err != nil {
					return r.err
				}
				if r.blockIdx == it.nextBlockIdx {
					it.insertResult(r)
					it.nextBlockIdx++
					return nil
				}
				it.resultBuf[r.blockIdx] = r // out-of-order: buffer until in order
			case <-it.stop:
				return fmt.Errorf("rowpack: batch pipeline stopped")
			}
		}
	}
	r, err := it.decodeBlock(it.blockIdx)
	if err != nil {
		return err
	}
	it.blockIdx++
	it.insertResult(r)
	return nil
}

// strSink copies payload into the iterator's append-only string arena and
// returns a zero-copy view backed by an arena chunk (see iterator.go).
func (it *BatchIterator) strSink(payload []byte) string {
	return it.arena.materialize(payload)
}

// decodeBlock loads, verifies and parses one planned block, producing the
// buffered items for it. Safe for concurrent use (read-only plan/view +
// concurrent-safe loader); workers never touch iterator mutable state.
func (it *BatchIterator) decodeBlock(bidx int) (blockResult, error) {
	pb := &it.plan.blocks[bidx]
	bl := it.st.view.Block(pb.blockID)
	if bl == nil {
		return blockResult{}, fmt.Errorf("rowpack: block %d missing from view", pb.blockID)
	}
	ref, hit, err := it.store.loadBatchBlock(int64(bl.DataOffset), pb.blockID)
	if err != nil {
		return blockResult{}, err
	}
	// Sequential decodes (Parallelism <= 1, single Next goroutine) reuse the
	// iterator's directory slice across blocks (each parse fully overwrites
	// it); parallel workers each pass nil — a shared slice would race.
	var dir []fileformat.RowDirectoryEntry
	if it.opts.Parallelism <= 1 {
		dir = it.scratch
	}
	rp, perr := block.ParseRowsDirectory(ref.Raw(), bl.ItemCount, dir)
	if perr != nil {
		ref.Release()
		return blockResult{}, perr
	}
	if it.opts.Parallelism <= 1 {
		it.scratch = rp.Entries
	}
	items := make([]batchBufItem, 0, len(pb.itemIdx))
	for _, ii := range pb.itemIdx {
		item := &it.plan.items[ii]
		if int(item.ordinal) >= len(rp.Entries) {
			ref.Release()
			return blockResult{}, fmt.Errorf("rowpack: row ordinal %d out of range in block %d", item.ordinal, pb.blockID)
		}
		ent := &rp.Entries[item.ordinal]
		schema := it.st.schemas.schema(bl.SnapshotID, bl.TableID, ent.SchemaVersion)
		if schema == nil {
			ref.Release()
			return blockResult{}, fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, ent.SchemaVersion)
		}
		items = append(items, batchBufItem{
			emitPos: item.emitPos,
			payload: rp.RowBytes(int(item.ordinal)),
			schema:  schema,
			rowID:   item.rowID,
		})
	}
	return blockResult{blockIdx: bidx, items: items, ref: ref, hit: hit}, nil
}

// insertResult buffers one block's items and records the ref until they are
// all emitted. Single-goroutine (Next) only.
func (it *BatchIterator) insertResult(r blockResult) {
	refIdx := len(it.refs)
	it.refs = append(it.refs, r.ref)
	it.refPending = append(it.refPending, len(r.items))
	for _, b := range r.items {
		it.pending[b.emitPos] = batchBufItem{
			payload: b.payload,
			schema:  b.schema,
			rowID:   b.rowID,
			refIdx:  refIdx,
		}
	}
	if r.hit {
		it.stats.CacheHits++
	}
	it.stats.BlocksRead++
	it.stats.StoredBytesRead += uint64(it.plan.blocks[r.blockIdx].storedSize)
	it.stats.RawBytesDecoded += uint64(it.plan.blocks[r.blockIdx].rawSize)
}

// startPipeline launches the parallel decode workers (M5). Workers consume
// block indices in plan order; Next assembles results in the same order, so
// emission semantics match the sequential path exactly. shutdown() releases
// everything.
func (it *BatchIterator) startPipeline() {
	it.started = true
	it.resultBuf = make(map[int]blockResult)
	n := it.opts.Parallelism
	if n > len(it.plan.blocks) {
		n = len(it.plan.blocks)
	}
	it.jobs = make(chan int)
	it.results = make(chan blockResult, n*2)
	it.stop = make(chan struct{})
	it.wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer it.wg.Done()
			for bidx := range it.jobs {
				r, err := it.decodeBlock(bidx)
				if err != nil {
					r = blockResult{blockIdx: bidx, err: err}
				}
				select {
				case it.results <- r:
				case <-it.stop:
					if r.ref != nil {
						r.ref.Release()
					}
					return
				}
			}
		}()
	}
	go func() {
		defer close(it.jobs)
		for i := range it.plan.blocks {
			select {
			case it.jobs <- i:
			case <-it.stop:
				return
			}
		}
	}()
	go func() {
		it.wg.Wait()
		close(it.results)
		close(it.pipelineDone)
	}()
}

// shutdownPipeline stops feeding, drains in-flight results (releasing their
// refs) and waits for every pipeline goroutine to exit. Idempotent; safe
// after errors, cancellation or Close.
func (it *BatchIterator) shutdownPipeline() {
	if it.stop == nil {
		return
	}
	it.stopOnce.Do(func() { close(it.stop) })
	for r := range it.results {
		if r.ref != nil {
			r.ref.Release()
		}
	}
	for _, r := range it.resultBuf {
		if r.ref != nil {
			r.ref.Release()
		}
	}
	it.resultBuf = nil
	<-it.pipelineDone
}

// decodeInto decodes one buffered item into dst.
func (it *BatchIterator) decodeInto(item batchBufItem, dst Row) (Row, error) {
	if dst == nil {
		if it.buf == nil {
			it.buf = make(Row, len(item.schema.Columns))
		}
		dst = it.buf
	}
	return codec.DecodeInto(dst, item.payload, item.schema, it.store.opts.codecLimits(), it.sink)
}

// releaseRefIfDone releases the block reference once every buffered item it
// owns has been emitted. Cache-owned refs release as a no-op.
func (it *BatchIterator) releaseRefIfDone(refIdx int) {
	if refIdx < 0 || refIdx >= len(it.refPending) {
		return
	}
	it.refPending[refIdx]--
	if it.refPending[refIdx] == 0 && it.refs[refIdx] != nil {
		it.refs[refIdx].Release()
		it.refs[refIdx] = nil
	}
}

// releaseAll releases every held block reference and drops buffered items.
func (it *BatchIterator) releaseAll() {
	it.shutdownPipeline()
	for _, r := range it.refs {
		if r != nil {
			r.Release()
		}
	}
	it.refs = nil
	it.refPending = nil
	it.pending = make(map[int]batchBufItem)
}

// Err returns the first error encountered, or nil on clean completion.
func (it *BatchIterator) Err() error { return it.err }

// Stats returns the cumulative request statistics; safe to call after Close.
func (it *BatchIterator) Stats() BatchReadStats { return it.stats }

// Close releases the iterator and waits for any parallel decode workers to
// exit. It is idempotent.
func (it *BatchIterator) Close() error {
	it.closed = true
	it.releaseAll()
	return nil
}

// loadBatchBlock loads a block for batch decoding: random-read cache first,
// then the streaming scan window; a miss decompresses into a pooled transient
// scratch and is promoted into the scan window while it has room (reusing the
// scratch buffer itself when it is exact-fit, so promotion costs no copy in
// the common uniform-block case; oversized scratch is copied so window
// accounting stays tight).
func (s *Store) loadBatchBlock(offset int64, blockID uint64) (*scanRef, bool, error) {
	if s.loader.cache != nil {
		if v, ok := s.loader.cache.Get(blockID); ok {
			return &scanRef{blk: v.(*block.Block)}, true, nil
		}
		if v, ok := s.loader.scan.Get(blockID); ok {
			return &scanRef{blk: v.(*block.Block)}, true, nil
		}
	}
	sc, err := s.loader.reader.ReadAtBlockTransient(offset)
	if err != nil {
		return nil, false, err
	}
	if s.loader.scan != nil && uint64(len(sc.Raw)) <= s.loader.scan.Remaining() {
		if cap(sc.Raw) == len(sc.Raw) {
			// Exact-fit scratch: transfer ownership into the window.
			blk := sc.Block
			sc.Detach()
			s.loader.scan.Put(blockID, int64(len(blk.Raw)), &blk)
			return &scanRef{blk: &blk}, false, nil
		}
		blk := &block.Block{Header: sc.Header, Raw: make([]byte, len(sc.Raw))}
		copy(blk.Raw, sc.Raw)
		s.loader.scan.Put(blockID, int64(len(blk.Raw)), blk)
		sc.Release()
		return &scanRef{blk: blk}, false, nil
	}
	return &scanRef{blk: &sc.Block, sc: sc}, false, nil
}
