package rowpack

import (
	"context"
	"errors"
	"fmt"
	"sort"

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
//     sequentially (one block at a time through the bounded scan window).

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
	emitPos int    // position in the requested output order (a permutation)
	rowID   RowID
	blockID uint64
	ordinal uint32
}

// batchBlock groups items sharing one block, in physical file order.
type batchBlock struct {
	blockID uint64
	offset  int64
	itemIdx []int // indices into plan.items
	rawSize uint32
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
	payload []byte
	schema  *codec.Schema
	rowID   RowID
	refIdx  int
}

// BatchIterator streams the rows of one batch request. It is not safe for
// concurrent use. Next returns rows in the requested order; rows invisible at
// the snapshot (missing or deleted) are skipped and never emitted.
type BatchIterator struct {
	store *Store
	st    *publishedState
	ctx   context.Context
	opts  BatchReadOptions

	plan    *batchPlan
	pending map[int]batchBufItem // emitPos -> decoded item awaiting emission
	nextEmit int

	// block decode cursor
	blockIdx int
	// refs hold scan windows alive while any of their items are buffered;
	// refPending[i] counts buffered items owned by refs[i] and the ref is
	// released when it reaches zero.
	refs        []*scanRef
	refPending  []int
	scratch     []fileformat.RowDirectoryEntry
	stats      BatchReadStats
	err        error
	closed     bool
	buf        Row // iterator-owned row when Next(dst) passes nil
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
		pending: make(map[int]batchBufItem),
	}
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
		pending: make(map[int]batchBufItem),
	}
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
		plan.blocks = append(plan.blocks, batchBlock{blockID: bid, offset: int64(bl.DataOffset), itemIdx: idxs, rawSize: bl.RawSize})
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
		// Decode the next block in physical order and buffer its items.
		if it.blockIdx >= len(it.plan.blocks) {
			it.err = fmt.Errorf("rowpack: batch plan exhausted at emit %d/%d", it.nextEmit, total)
			it.releaseAll()
			return 0, nil, false
		}
		if derr := it.decodeNextBlock(); derr != nil {
			it.err = derr
			it.releaseAll()
			return 0, nil, false
		}
	}
}

// decodeNextBlock loads and parses the next planned block once and buffers
// every requested item of that block.
func (it *BatchIterator) decodeNextBlock() error {
	pb := &it.plan.blocks[it.blockIdx]
	it.blockIdx++
	bl := it.st.view.Block(pb.blockID)
	if bl == nil {
		return fmt.Errorf("rowpack: block %d missing from view", pb.blockID)
	}
	ref, hit, err := it.store.loadBatchBlock(int64(bl.DataOffset), pb.blockID)
	if err != nil {
		return err
	}
	if hit {
		it.stats.CacheHits++
	}
	it.stats.BlocksRead++
	it.stats.StoredBytesRead += uint64(bl.StoredSize)
	it.stats.RawBytesDecoded += uint64(bl.RawSize)

	rp, err := block.ParseRowsDirectory(ref.Raw(), bl.ItemCount, it.scratch)
	if err != nil {
		ref.Release()
		return err
	}
	it.scratch = rp.Entries

	// Track the ref until all of this block's items are emitted.
	pendingCount := 0
	for _, ii := range pb.itemIdx {
		item := &it.plan.items[ii]
		if int(item.ordinal) >= len(rp.Entries) {
			ref.Release()
			return fmt.Errorf("rowpack: row ordinal %d out of range in block %d", item.ordinal, pb.blockID)
		}
		ent := &rp.Entries[item.ordinal]
		schema := it.st.schemas.schema(bl.SnapshotID, bl.TableID, ent.SchemaVersion)
		if schema == nil {
			ref.Release()
			return fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, ent.SchemaVersion)
		}
		it.pending[item.emitPos] = batchBufItem{
			payload: rp.RowBytes(int(item.ordinal)),
			schema:  schema,
			rowID:   item.rowID,
			refIdx:  len(it.refs),
		}
		pendingCount++
	}
	it.refs = append(it.refs, ref)
	it.refPending = append(it.refPending, pendingCount)
	return nil
}

// decodeInto decodes one buffered item into dst.
func (it *BatchIterator) decodeInto(item batchBufItem, dst Row) (Row, error) {
	if dst == nil {
		if it.buf == nil {
			it.buf = make(Row, len(item.schema.Columns))
		}
		dst = it.buf
	}
	return codec.DecodeInto(dst, item.payload, item.schema, it.store.opts.codecLimits(), nil)
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

// Close releases the iterator. It is idempotent.
func (it *BatchIterator) Close() error {
	it.closed = true
	it.releaseAll()
	return nil
}

// loadBatchBlock loads a block for batch decoding: random-read cache first,
// then the bounded scan window; a miss decompresses into a pooled scratch.
// The second return value reports a cache/window hit.
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
		blk := &block.Block{Header: sc.Header, Raw: make([]byte, len(sc.Raw))}
		copy(blk.Raw, sc.Raw)
		s.loader.scan.Put(blockID, int64(len(blk.Raw)), blk)
		sc.Release()
		return &scanRef{blk: blk}, false, nil
	}
	return &scanRef{blk: &sc.Block, sc: sc}, false, nil
}