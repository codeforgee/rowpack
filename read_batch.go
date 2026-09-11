package rowpack

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/index"
)

// batchReq is one requested row: its slot in the caller's ids slice plus the
// block location the batch is ordered by.
type batchReq struct {
	outIdx  int    // index into the caller's ids slice (row goes to out[outIdx])
	blockID uint64 // block holding the row
	ordinal uint32 // ItemOrdinal of the row inside the block
}

// decodeChunkRows caps one DecodeBatchInto call. The per-chunk stack buffers
// stay small while the batch kernel still amortises per-row decode overhead.
const decodeChunkRows = 128

// batchBuffer is the internal working set of one batch read: the request
// list, the caller-visible output slice, the value slab and the string arena.
// The zero value is ready to use and buffers are rewound, not reallocated, so
// a batch read that reuses one performs no allocation after warmup.
//
// Lifetime: rows returned through a buffer — including every String/Bytes
// view — are valid only until the next read that reuses it. ReadBatch gives
// each call a fresh buffer, so its rows live until the caller drops them;
// readBatchInto exists so internal callers and tests can reuse one.
// A batchBuffer must not be copied after first use.
type batchBuffer struct {
	_ noCopy

	reqs  []batchReq // requests grouped by (blockID, ordinal)
	out   []Row      // result, indexed by the caller's id slot
	slab  []Value    // one contiguous value slab shared by every decoded row
	arena strArena
}

// noCopy makes go vet's copylocks check flag copies of a batchBuffer: copies
// would share the slab and output backing arrays and overwrite each other.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// ReadBatch reads a set of rows in one call, aggregating the request by
// block: every block touched by the batch is loaded, CRC-verified and
// decompressed at most once, no matter how many requested rows fall inside
// it. This is the batch counterpart of the per-row Get and is intended for
// the "read a range or RowID set" access pattern; the batch is returned in
// the order of ids, one row per id (duplicate ids read the row twice, like
// repeated Gets).
//
// Semantics match Get per row: every id must be visible at the snapshot
// (neither missing nor deleted), otherwise ReadBatch returns the same
// ErrNotFound error Get would and no rows are returned. Rows are freshly
// decoded and owned by the caller (like Get with a nil dst; none of the rows
// alias each other or the block buffers): String/Bytes payloads are
// materialized into a per-call arena as distinct zero-copy views, so retained
// values stay valid via GC without pinning block buffers. ctx is accepted
// for signature consistency; cancellation is not observed mid-batch (same as
// Get).
//
// ReadBatch is safe for concurrent use and counts toward Stats.Batch:
// Blocks and RawBytes quantify the aggregation (Blocks <= len(ids); with
// clustered ids, Blocks << len(ids) and the same payload is decompressed
// once per batch instead of once per row).
func (s *Store) ReadBatch(ctx context.Context, snapshot SnapshotID, table string, ids []RowID) ([]Row, error) {
	// A fresh buffer per call keeps the returned rows independent of any later
	// call; see readBatchInto for the reuse entry point used internally.
	var buf batchBuffer
	return s.readBatchInto(ctx, snapshot, table, ids, &buf)
}

// readBatchInto is ReadBatch writing through a caller-owned batchBuffer, so a
// repeated batch read does not reallocate the request, output and decode slab
// (~700 B/row at 7 columns) on every call. Unexported on purpose: reusing a
// buffer invalidates the previous call's rows, a contract only code in this
// package may take on.
//
// buf must not be nil, and must not be reused concurrently.
func (s *Store) readBatchInto(ctx context.Context, snapshot SnapshotID, table string, ids []RowID, buf *batchBuffer) ([]Row, error) {
	if buf == nil {
		return nil, fmt.Errorf("%w: nil batchBuffer", ErrInvalidArgument)
	}
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if st.view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableID(snapshot, table)
	if !ok {
		return nil, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	var br batchReader
	br.init(s, st, snapshot, uint32(tid), len(ids), buf)
	if err := br.resolve(snapshot, uint32(tid), table, ids); err != nil {
		return nil, err
	}
	// The sink is a per-call local (not a field): its closures never escape, so
	// it and the arena stay on the stack instead of adding per-call heap
	// objects. Only the arena's chunk is retained by buf.
	sink := strArenaSink(&buf.arena)
	if err := br.readBlocks(sink); err != nil {
		return nil, err
	}
	s.batchCalls.Add(1)
	s.batchRows.Add(uint64(len(ids)))
	s.batchBlocks.Add(br.blocks)
	s.batchRawBytes.Add(br.rawBytes)
	return buf.out, nil
}

// batchReader decodes one ReadBatch call. The requests are resolved and sorted
// by (block, ordinal) up front, so the reader only walks the sorted slice:
// block by block, page by page, decoding one homogeneous schema-version chunk
// at a time. Its buffers live in the caller's batchBuffer.
type batchReader struct {
	store   *Store
	schemas *schemaIndex
	view    *index.View
	buf     *batchBuffer

	// curBlock plus the decoder memo belong to the block being read: blocks
	// normally hold a single schema version, so repeated pages of one block
	// resolve and prepare the decoder once.
	curBlock *index.BlockLoc
	decVer   uint32
	decoder  codec.Decoder
	decOK    bool

	blocks   uint64 // Stats.Batch counters
	rawBytes uint64
}

// init rewinds the caller's buffers for a batch of n ids, growing each only
// when this batch is larger than any previous one. maxColumns sizes the value
// slab so no decode ever grows it.
func (br *batchReader) init(s *Store, st *publishedState, snapshot uint64, tid uint32, n int, buf *batchBuffer) {
	br.store = s
	br.schemas = st.schemas
	br.view = st.view
	br.buf = buf

	if cap(buf.reqs) < n {
		buf.reqs = make([]batchReq, 0, n)
	} else {
		buf.reqs = buf.reqs[:0]
	}
	if cap(buf.out) < n {
		buf.out = make([]Row, n)
	} else {
		buf.out = buf.out[:n]
	}
	if need := n * st.schemas.maxColumns(snapshot, tid); cap(buf.slab) < need {
		buf.slab = make([]Value, 0, need)
	} else {
		buf.slab = buf.slab[:0]
	}
	buf.arena.reset()
}

// resolve maps every id to its row location along the parent chain and groups
// the requests by block. Resolution is index-only (no block I/O); the per-row
// cost is one binary search per chain layer, the same as Get.
func (br *batchReader) resolve(snapshot uint64, tid uint32, table string, ids []RowID) error {
	for i, id := range ids {
		loc, ok := br.view.ResolveRow(snapshot, tid, id)
		if !ok {
			return fmt.Errorf("%w: (table %q, row %d) in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		if loc.ChangeType == format.ChangeDelete {
			return fmt.Errorf("%w: (table %q, row %d) deleted in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		br.buf.reqs = append(br.buf.reqs, batchReq{outIdx: i, blockID: loc.BlockID, ordinal: loc.ItemOrdinal})
	}
	// (BlockID, ItemOrdinal) order reads blocks in file order and keeps every
	// page of a block one contiguous run, so each page is decompressed once.
	// Duplicates stay adjacent: repeated Gets, in sorted order.
	slices.SortFunc(br.buf.reqs, func(a, b batchReq) int {
		if a.blockID != b.blockID {
			return cmp.Compare(a.blockID, b.blockID)
		}
		return cmp.Compare(a.ordinal, b.ordinal)
	})
	return nil
}

// readBlocks walks the sorted requests one block at a time. sink is the
// per-call decode sink, threaded rather than stored so it does not escape.
func (br *batchReader) readBlocks(sink *codec.Sink) error {
	reqs := br.buf.reqs
	for i := 0; i < len(reqs); {
		end := i + 1
		for end < len(reqs) && reqs[end].blockID == reqs[i].blockID {
			end++
		}
		if err := br.readBlock(i, end, sink); err != nil {
			return err
		}
		i = end
	}
	return nil
}

// readBlock decodes reqs[i:end], which all live in one block: the page
// container is loaded once and each page's requests are decoded in ordinal
// order.
func (br *batchReader) readBlock(i, end int, sink *codec.Sink) error {
	bl := br.view.Block(br.buf.reqs[i].blockID)
	if bl == nil {
		return fmt.Errorf("rowpack: block %d missing from view", br.buf.reqs[i].blockID)
	}
	rc, err := br.store.loader.LoadRows(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return err
	}
	br.curBlock = bl
	br.decOK = false
	br.blocks++
	br.rawBytes += uint64(bl.RawSize)

	for i < end {
		pi, stop, err := br.pageRun(rc, i, end)
		if err != nil {
			return err
		}
		if err := br.readPage(rc, pi, i, stop, sink); err != nil {
			return err
		}
		i = stop
	}
	return nil
}

// pageRun returns the index of the page holding reqs[i] and the end of the
// run of requests inside that same page. reqs are ordinal-sorted, so the run
// is a contiguous slice.
func (br *batchReader) pageRun(rc *block.RowsContainer, i, end int) (int, int, error) {
	reqs := br.buf.reqs
	pi, err := rc.PageFor(reqs[i].ordinal)
	if err != nil {
		return 0, 0, err
	}
	stop := i + 1
	for stop < end {
		next, err := rc.PageFor(reqs[stop].ordinal)
		if err != nil {
			return 0, 0, err
		}
		if next != pi {
			break
		}
		stop++
	}
	return pi, stop, nil
}

// readPage decodes reqs[i:end], all inside page pi and ordered by ordinal. The
// page is decompressed once; its records are decoded in runs that share one
// schema version, at most decodeChunkRows rows per DecodeBatchInto call.
func (br *batchReader) readPage(rc *block.RowsContainer, pi, i, end int, sink *codec.Sink) error {
	page, release, err := rc.PageScratch(pi)
	if err != nil {
		return err
	}
	defer release()

	first := rc.Dir[pi].FirstRecordOrdinal
	var bodies [decodeChunkRows][]byte
	var slots [decodeChunkRows]int
	for i < end {
		ver, n, err := br.collectChunk(page, first, i, end, bodies[:], slots[:])
		if err != nil {
			return err
		}
		if err := br.decodeChunk(ver, bodies[:n], slots[:n], sink); err != nil {
			return err
		}
		clear(bodies[:n]) // drop references into the pooled page scratch
		i += n
	}
	return nil
}

// collectChunk gathers the longest run of requests starting at i that share
// one schema version, up to len(bodies) rows, writing each record body and its
// output slot into bodies and slots. It returns the run's schema version and
// length.
func (br *batchReader) collectChunk(page *block.RowsPage, first uint32, i, end int, bodies [][]byte, slots []int) (uint32, int, error) {
	reqs := br.buf.reqs
	rec, err := page.RecordAt(reqs[i].ordinal - first)
	if err != nil {
		return 0, 0, err
	}
	ver := rec.SchemaVersion
	bodies[0], slots[0] = rec.Body, reqs[i].outIdx
	n := 1
	for n < len(bodies) && i+n < end {
		rec, err := page.RecordAt(reqs[i+n].ordinal - first)
		if err != nil {
			return 0, 0, err
		}
		if rec.SchemaVersion != ver {
			break
		}
		bodies[n], slots[n] = rec.Body, reqs[i+n].outIdx
		n++
	}
	return ver, n, nil
}

// decodeChunk decodes one homogeneous run and scatters the resulting row views
// into their caller slots.
func (br *batchReader) decodeChunk(ver uint32, bodies [][]byte, slots []int, sink *codec.Sink) error {
	decoder, err := br.decoderFor(ver)
	if err != nil {
		return err
	}
	buf := br.buf
	columns := decoder.Columns()
	start := len(buf.slab)
	buf.slab, err = decoder.DecodeBatchInto(buf.slab, bodies, sink)
	if err != nil {
		return err
	}
	for k, slot := range slots {
		rowStart := start + k*columns
		buf.out[slot] = buf.slab[rowStart : rowStart+columns]
	}
	return nil
}

// decoderFor returns the prepared decoder for a schema version under the
// current block, memoizing it for the block's remaining pages.
func (br *batchReader) decoderFor(ver uint32) (codec.Decoder, error) {
	if br.decOK && br.decVer == ver {
		return br.decoder, nil
	}
	decoder, err := br.schemas.decoderFor(br.curBlock, ver)
	if err != nil {
		return codec.Decoder{}, err
	}
	br.decVer, br.decoder, br.decOK = ver, decoder, true
	return decoder, nil
}
