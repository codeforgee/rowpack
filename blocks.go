package rowpack

import (
	"context"
	"fmt"
	"runtime"
	"sort"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
)

// Blocks lists the physical rows segments (sealed blocks) of one table at a
// snapshot, in physical write order (ascending BlockID): a FULL snapshot
// holds the whole table, a DELTA holds exactly this transaction's changes.
// MinRowID is inclusive and MaxRowID exclusive, so consecutive sealed blocks
// tile the table's RowID space; readers enumerate spans here and read each
// one with a ranged Scan. All fields are derived from the in-memory index;
// no block is read from disk.
func (s *Store) Blocks(snap SnapshotID, table string) ([]Block, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	rc, err := s.resolveRead(snap, table)
	if err != nil {
		return nil, err
	}
	return s.blocksByTable(rc.st.view, uint64(snap), rc.tid)
}

func (s *Store) blocksByTable(view *index.View, snap uint64, tid TableID) ([]Block, error) {
	// Own-txn rows blocks, ascending BlockID (= physical write order).
	var locs []*index.BlockLoc
	for _, bl := range view.Blocks() {
		if bl.SnapshotID == snap && bl.TableID == uint32(tid) && bl.Kind == format.BlockKindRows {
			locs = append(locs, bl)
		}
	}
	if len(locs) == 0 {
		return nil, nil
	}
	sort.Slice(locs, func(i, j int) bool { return locs[i].BlockID < locs[j].BlockID })
	// Derive each block's row range from the snapshot's own row shard
	// (tombstones included): min/max RowID per BlockID, zero block I/O.
	out := make([]Block, 0, len(locs))
	byID := make(map[uint64]*Block, len(locs))
	for _, bl := range locs {
		b := Block{
			BlockID:     bl.BlockID,
			ItemCount:   bl.ItemCount,
			MinRowID:    ^RowID(0),
			RawBytes:    bl.RawSize,
			StoredBytes: bl.StoredSize,
			RawCRC32C:   bl.RawCRC32C,
		}
		out = append(out, b)
		byID[bl.BlockID] = &out[len(out)-1]
	}
	it := view.RowIter(snap, uint32(tid))
	// hit tracks blocks that actually hold a row: MaxRowID is the exclusive
	// end (max+1), so a block whose max RowID is MaxUint64 overflows to 0 and
	// must not be mistaken for an empty block by the MaxRowID==0 test below.
	hit := make(map[uint64]bool, len(locs))
	if it != nil {
		for !it.Done() {
			rowID := it.RowID()
			loc := it.Loc()
			if b := byID[loc.BlockID]; b != nil {
				hit[loc.BlockID] = true
				if RowID(rowID) < b.MinRowID {
					b.MinRowID = RowID(rowID)
				}
				if RowID(rowID)+1 > b.MaxRowID {
					b.MaxRowID = RowID(rowID) + 1
				}
			}
			it.Next()
		}
	}
	for i := range out {
		if !hit[out[i].BlockID] { // no indexed record: keep the empty range
			out[i].MinRowID = 0
		}
	}
	return out, nil
}

// ScanBlocks sequentially scans the raw per-record change stream of the
// snapshot's own blocks whose BlockID falls in [lo, hi): no parent-chain
// merge, no tombstone filtering — every record is emitted in physical write
// order with its raw ChangeType (DELETE records carry a nil Row). lo/hi must
// intersect the snapshot's own block range, otherwise ErrInvalidArgument.
// Like Scan, the iterator holds the store's read lock until Close; a leaked
// iterator is released by its GC finalizer, but defer Close regardless.
func (s *Store) ScanBlocks(ctx context.Context, snap SnapshotID, table string, lo, hi uint64) (*Iterator, error) {
	s.readMu.RLock()
	keepLock := false
	defer func() {
		if !keepLock {
			s.readMu.RUnlock()
		}
	}()
	rc, err := s.resolveRead(snap, table)
	if err != nil {
		return nil, err
	}
	blocks, err := s.blocksByTable(rc.st.view, uint64(snap), rc.tid)
	if err != nil {
		return nil, err
	}
	if lo >= hi {
		return nil, fmt.Errorf("%w: block range [%d,%d)", ErrInvalidArgument, lo, hi)
	}
	// Validate the range intersects the snapshot's own block space.
	var ids []uint64
	for _, b := range blocks {
		if b.BlockID >= lo && b.BlockID < hi {
			ids = append(ids, b.BlockID)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: block range [%d,%d) outside snapshot %d blocks", ErrInvalidArgument, lo, hi, snap)
	}
	it := &Iterator{
		store:    s,
		state:    rc.st,
		ctx:      ctx,
		snapshot: snap,
		table:    rc.tid,
		mode:     scanModeBlocks,
		blockIDs: ids,
		readHeld: true,
	}
	it.arena = &strArena{} // heap-allocated: see the arena field comment in iterator.go
	it.sink = strArenaSink(it.arena)
	keepLock = true
	runtime.SetFinalizer(it, (*Iterator).finish)
	return it, nil
}
