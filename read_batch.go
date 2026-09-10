package rowpack

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// batchReq is one requested row: its slot in the caller's ids slice plus the
// location the sort below orders by.
type batchReq struct {
	outIdx  int    // index into the caller's ids slice (row goes to out[outIdx])
	blockID uint64 // block holding the row
	ordinal uint32 // ItemOrdinal of the row inside the block
}

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
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableID(snapshot, table)
	if !ok {
		return nil, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Resolve every id along the parent chain and group requests by block.
	// Resolution is index-only (no block I/O); the per-row cost is one binary
	// search per chain layer, the same as Get.
	reqs := make([]batchReq, 0, len(ids))
	for i, id := range ids {
		loc, ok := view.ResolveRow(snapshot, uint32(tid), id)
		if !ok {
			return nil, fmt.Errorf("%w: (table %q, row %d) in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		if loc.ChangeType == fileformat.ChangeDelete {
			return nil, fmt.Errorf("%w: (table %q, row %d) deleted in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		reqs = append(reqs, batchReq{outIdx: i, blockID: loc.BlockID, ordinal: loc.ItemOrdinal})
	}
	// Order by (BlockID, ItemOrdinal): blocks in file order (sequential reads
	// for a clustered snapshot) and every page of a block one contiguous run,
	// so each page is decompressed once. Duplicates stay adjacent, like
	// repeated Gets. A sorted slice replaces the two per-batch maps this used
	// to build: no hashing, no growth, deterministic order.
	slices.SortFunc(reqs, func(a, b batchReq) int {
		if a.blockID != b.blockID {
			return cmp.Compare(a.blockID, b.blockID)
		}
		return cmp.Compare(a.ordinal, b.ordinal)
	})

	out := make([]Row, len(ids))
	// Per-call arena: String/Bytes payloads materialize as zero-copy views
	// into bounded chunks instead of one heap copy per value. Rows remain
	// caller-owned: each value gets its own arena region (never aliased by
	// another row), and views survive chunk rotation, so retained values keep
	// their chunk alive via GC without pinning whole block buffers.
	var arena strArena
	sink := strArenaSink(&arena)
	// Rows decode into one contiguous slab, so N rows cost one allocation
	// instead of one []Value per row. The capacity comes from the widest schema
	// version of the table, so no decode ever grows the slab. Rows share it in
	// disjoint regions, so they still never alias each other.
	ncols := st.schemas.maxColumns(snapshot, uint32(tid))
	slab := make([]Value, 0, len(ids)*ncols)
	var blocks, rawBytes uint64
	for i := 0; i < len(reqs); {
		bid := reqs[i].blockID
		bl := view.Block(bid)
		if bl == nil {
			return nil, fmt.Errorf("rowpack: block %d missing from view", bid)
		}
		// Load the page container (no page decompressed yet), then walk the
		// block's requests in ordinal order.
		rc, err := s.loader.LoadRows(int64(bl.DataOffset), bid)
		if err != nil {
			return nil, err
		}
		blocks++
		rawBytes += uint64(bl.RawSize)
		for i < len(reqs) && reqs[i].blockID == bid {
			pi, err := rc.PageFor(reqs[i].ordinal)
			if err != nil {
				return nil, err
			}
			// Every request up to (not including) end lands in page pi.
			end := i + 1
			for end < len(reqs) && reqs[end].blockID == bid {
				next, err := rc.PageFor(reqs[end].ordinal)
				if err != nil {
					return nil, err
				}
				if next != pi {
					break
				}
				end++
			}
			page, release, err := rc.PageScratch(pi)
			if err != nil {
				return nil, err
			}
			dir := &rc.Dir[pi]
			for ; i < end; i++ {
				rec, err := page.RecordAt(reqs[i].ordinal - dir.FirstRecordOrdinal)
				if err != nil {
					release()
					return nil, err
				}
				dst := slab[len(slab) : len(slab) : len(slab)+ncols]
				row, err := s.decodeInto(rec, dst, rowDecodeContext{block: bl, schema: st.schemas, sink: sink})
				if err != nil {
					release()
					return nil, err
				}
				// A row wider than the widest schema was allocated fresh (not
				// aliasing the slab), so never advance past the slab's cap.
				if len(row) <= cap(slab)-len(slab) {
					slab = slab[:len(slab)+len(row)]
				}
				out[reqs[i].outIdx] = row
			}
			release()
		}
	}
	s.batchCalls.Add(1)
	s.batchRows.Add(uint64(len(ids)))
	s.batchBlocks.Add(blocks)
	s.batchRawBytes.Add(rawBytes)
	return out, nil
}
