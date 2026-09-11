package rowpack

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
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
	var arena strArena
	sink := strArenaSink(&arena)
	var br batchReader
	br.init(s, st, snapshot, uint32(tid), len(ids))
	if err := br.resolve(snapshot, uint32(tid), table, ids); err != nil {
		return nil, err
	}
	if err := br.readBlocks(sink); err != nil {
		return nil, err
	}
	s.batchCalls.Add(1)
	s.batchRows.Add(uint64(len(ids)))
	s.batchBlocks.Add(br.blocks)
	s.batchRawBytes.Add(br.rawBytes)
	return br.out, nil
}

// batchReader decodes one ReadBatch call. The requests are resolved and sorted
// by (block, ordinal) up front, so the reader only walks the sorted slice:
// block by block, page by page, decoding one homogeneous schema-version chunk
// at a time.
type batchReader struct {
	store   *Store
	schemas *schemaIndex
	view    *index.View

	reqs []batchReq // sorted by (blockID, ordinal)
	out  []Row      // result, indexed by the caller's id slot
	slab []Value    // one contiguous value slab shared by every decoded row

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

// init sizes the per-call request, output and value buffers; maxColumns sizes
// the value slab so no decode ever grows it.
func (br *batchReader) init(s *Store, st *publishedState, snapshot uint64, tid uint32, n int) {
	br.store = s
	br.schemas = st.schemas
	br.view = st.view
	br.reqs = make([]batchReq, 0, n)
	br.out = make([]Row, n)
	br.slab = make([]Value, 0, n*st.schemas.maxColumns(snapshot, tid))
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
		if loc.ChangeType == fileformat.ChangeDelete {
			return fmt.Errorf("%w: (table %q, row %d) deleted in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		br.reqs = append(br.reqs, batchReq{outIdx: i, blockID: loc.BlockID, ordinal: loc.ItemOrdinal})
	}
	// (BlockID, ItemOrdinal) order reads blocks in file order and keeps every
	// page of a block one contiguous run, so each page is decompressed once.
	// Duplicates stay adjacent: repeated Gets, in sorted order.
	slices.SortFunc(br.reqs, func(a, b batchReq) int {
		if a.blockID != b.blockID {
			return cmp.Compare(a.blockID, b.blockID)
		}
		return cmp.Compare(a.ordinal, b.ordinal)
	})
	return nil
}

// readBlocks walks the sorted requests one block at a time. sink is the
// per-call decode sink, threaded as a parameter (not a field) so its arena
// stays on the caller's stack instead of escaping to the heap.
func (br *batchReader) readBlocks(sink *codec.Sink) error {
	for i := 0; i < len(br.reqs); {
		end := i + 1
		for end < len(br.reqs) && br.reqs[end].blockID == br.reqs[i].blockID {
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
	bl := br.view.Block(br.reqs[i].blockID)
	if bl == nil {
		return fmt.Errorf("rowpack: block %d missing from view", br.reqs[i].blockID)
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
	pi, err := rc.PageFor(br.reqs[i].ordinal)
	if err != nil {
		return 0, 0, err
	}
	stop := i + 1
	for stop < end {
		next, err := rc.PageFor(br.reqs[stop].ordinal)
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
	rec, err := page.RecordAt(br.reqs[i].ordinal - first)
	if err != nil {
		return 0, 0, err
	}
	ver := rec.SchemaVersion
	bodies[0], slots[0] = rec.Body, br.reqs[i].outIdx
	n := 1
	for n < len(bodies) && i+n < end {
		rec, err := page.RecordAt(br.reqs[i+n].ordinal - first)
		if err != nil {
			return 0, 0, err
		}
		if rec.SchemaVersion != ver {
			break
		}
		bodies[n], slots[n] = rec.Body, br.reqs[i+n].outIdx
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
	columns := decoder.Columns()
	start := len(br.slab)
	br.slab, err = decoder.DecodeBatchInto(br.slab, bodies, sink)
	if err != nil {
		return err
	}
	for k, slot := range slots {
		rowStart := start + k*columns
		br.out[slot] = br.slab[rowStart : rowStart+columns]
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
