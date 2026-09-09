package rowpack

import (
	"context"
	"fmt"
	"sort"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// batchReq is one requested row located inside a target block.
type batchReq struct {
	outIdx  int    // index into the caller's ids slice (row goes to out[outIdx])
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
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableIDByName(snapshot, table)
	if !ok {
		return nil, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	// Resolve every id along the parent chain and group requests by block.
	// Resolution is index-only (no block I/O); the per-row cost is one binary
	// search per chain layer, the same as Get.
	groups := make(map[uint64][]batchReq, 8)
	blockIDs := make([]uint64, 0, 8)
	for i, id := range ids {
		loc, ok := view.ResolveRow(snapshot, uint32(tid), id)
		if !ok {
			return nil, fmt.Errorf("%w: (table %q, row %d) in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		if loc.ChangeType == fileformat.ChangeDelete {
			return nil, fmt.Errorf("%w: (table %q, row %d) deleted in snapshot %d", ErrNotFound, table, id, snapshot)
		}
		reqs := groups[loc.BlockID]
		if len(reqs) == 0 {
			blockIDs = append(blockIDs, loc.BlockID)
		}
		groups[loc.BlockID] = append(reqs, batchReq{outIdx: i, ordinal: loc.ItemOrdinal})
	}
	// Walk blocks in ascending ID order (= the file layout order for a fully
	// clustered snapshot), so a batch that spans many blocks reads
	// sequentially rather than jumping around the file.
	sort.Slice(blockIDs, func(i, j int) bool { return blockIDs[i] < blockIDs[j] })

	out := make([]Row, len(ids))
	// Per-call arena: String/Bytes payloads materialize as zero-copy views
	// into bounded chunks instead of one heap copy per value. Rows remain
	// caller-owned: each value gets its own arena region (never aliased by
	// another row), and views survive chunk rotation, so retained values keep
	// their chunk alive via GC without pinning whole block buffers.
	var arena strArena
	sink := strArenaSink(&arena)
	var blocks, rawBytes uint64
	for _, bid := range blockIDs {
		bl := view.Block(bid)
		if bl == nil {
			return nil, fmt.Errorf("rowpack: block %d missing from view", bid)
		}
		// Load the page container (no page decompressed yet), then group the
		// requested rows by page so each page is decompressed exactly once per
		// batch regardless of how many requested rows fall in it.
		rc, err := s.loader.LoadRows(int64(bl.DataOffset), bid)
		if err != nil {
			return nil, err
		}
		blocks++
		rawBytes += uint64(bl.RawSize)
		reqByPage := make(map[uint32][]batchReq, 4)
		for _, req := range groups[bid] {
			pi, err := rc.PageIndexForOrdinal(req.ordinal)
			if err != nil {
				return nil, err
			}
			reqByPage[uint32(pi)] = append(reqByPage[uint32(pi)], req)
		}
		for pi, pageReqs := range reqByPage {
			page, release, err := rc.PageScratch(int(pi))
			if err != nil {
				return nil, err
			}
			dir := &rc.Dir[pi]
			for _, req := range pageReqs {
				rec, err := page.RecordAt(req.ordinal - dir.FirstRecordOrdinal)
				if err != nil {
					release()
					return nil, err
				}
				row, err := s.decodeBodyRecordInto(rec, bl, st.schemas, nil, sink)
				if err != nil {
					release()
					return nil, err
				}
				out[req.outIdx] = row
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
