package index

import (
	"fmt"

	"github.com/codeforgee/rowpack/internal/format"
)

// rowShardBuilder incrementally builds per-table rowShards from a stream of
// RowIndexEntry values that arrive sorted by (TableID, RowID) — the order the
// sorted Row Index Pages are decoded (encodePage guarantees this). Each
// entry is appended directly into the columnar (rowIDs/ordinals/changes) and
// block-run (runStart/blockIDs) arrays, so no []RowKeyLoc intermediate and no
// fully materialized []RowIndexEntry page ever exists.
// finish() validates ascending order + rejects duplicates, then finalizes each
// run by appending the terminal runStart sentinel.
type rowShardBuilder struct {
	snapID uint64

	table     uint32
	have      bool
	rowIDs    []uint64
	ordinals  []uint32
	changes   []uint8
	runStart  []uint32
	blockIDs  []uint64
	prevBlock uint64
	haveBlock bool

	done map[uint32]*rowShard
}

// newRowShardBuilder creates an empty builder. hint is the total expected row
// count from the txn header (0 = unknown); the first (single-table) shard is
// preallocated to it so a one-table snapshot never reallocates during growth.
func newRowShardBuilder(snapID uint64, hint int) *rowShardBuilder {
	b := &rowShardBuilder{snapID: snapID, done: make(map[uint32]*rowShard)}
	if hint > 0 {
		b.rowIDs = make([]uint64, 0, hint)
		b.ordinals = make([]uint32, 0, hint)
		b.changes = make([]uint8, 0, hint)
		b.runStart = make([]uint32, 0, 8)
		b.blockIDs = make([]uint64, 0, 8)
	}
	return b
}

// AddRowEntry appends one decoded row entry into the current table's shard.
// Entries must arrive sorted by (TableID, RowID); a table switch finalizes the
// current shard. Ascending order and absence of duplicate RowKeys are validated.
func (b *rowShardBuilder) AddRowEntry(e format.RowIndexEntry) error {
	if e.SnapshotID != b.snapID {
		return fmt.Errorf("rowpack: row entry wrong snapshot")
	}
	if !b.have {
		b.table = e.TableID
		b.have = true
	} else if e.TableID != b.table {
		if err := b.finalize(); err != nil {
			return err
		}
		b.table = e.TableID
		b.have = true
	}
	if n := len(b.rowIDs); n > 0 {
		last := b.rowIDs[n-1]
		if e.RowID < last {
			return fmt.Errorf("rowpack: rows not ascending at %d", e.RowID)
		}
		if e.RowID == last {
			return fmt.Errorf("rowpack: duplicate row %d in snapshot", e.RowID)
		}
	}
	b.rowIDs = append(b.rowIDs, e.RowID)
	b.ordinals = append(b.ordinals, e.ItemOrdinal)
	b.changes = append(b.changes, uint8(e.ChangeType))
	if !b.haveBlock || e.BlockID != b.prevBlock {
		b.runStart = append(b.runStart, uint32(len(b.rowIDs)-1))
		b.blockIDs = append(b.blockIDs, e.BlockID)
		b.prevBlock = e.BlockID
		b.haveBlock = true
	}
	return nil
}

// AddPageRows appends one decoded index page in bulk: a table switch
// finalizes the current shard, ascending order and duplicates are validated
// over the whole batch (including across the page boundary), and the columns
// plus block runs are appended wholesale. This replaces one AddRowEntry call
// per row with a handful of calls per page.
func (b *rowShardBuilder) AddPageRows(p *pageRows, snapID uint64) error {
	if len(p.rowIDs) == 0 {
		return nil
	}
	if snapID != b.snapID {
		return fmt.Errorf("rowpack: row entry wrong snapshot")
	}
	for t := 0; t < len(p.tableIDs); t++ {
		start, end := int(p.tableRunStart[t]), int(p.tableRunStart[t+1])
		if b.have && p.tableIDs[t] != b.table {
			if err := b.finalize(); err != nil {
				return err
			}
		}
		b.table, b.have = p.tableIDs[t], true
		base := len(b.rowIDs)
		if base > 0 {
			last, first := b.rowIDs[base-1], p.rowIDs[start]
			if first < last {
				return fmt.Errorf("rowpack: rows not ascending at %d", first)
			}
			if first == last {
				return fmt.Errorf("rowpack: duplicate row %d in snapshot", first)
			}
		}
		for i := start + 1; i < end; i++ {
			if p.rowIDs[i] < p.rowIDs[i-1] {
				return fmt.Errorf("rowpack: rows not ascending at %d", p.rowIDs[i])
			}
			if p.rowIDs[i] == p.rowIDs[i-1] {
				return fmt.Errorf("rowpack: duplicate row %d in snapshot", p.rowIDs[i])
			}
		}
		// Block runs intersecting this table run. A run continuing from the
		// previous page (same BlockID) keeps its existing run start.
		for r := 0; r < len(p.blockIDs); r++ {
			lo, hi := int(p.blockRunStart[r]), int(p.blockRunStart[r+1])
			if lo >= end || hi <= start {
				continue
			}
			bid := p.blockIDs[r]
			if !b.haveBlock || bid != b.prevBlock {
				b.runStart = append(b.runStart, uint32(base+max(lo, start)-start))
				b.blockIDs = append(b.blockIDs, bid)
				b.prevBlock, b.haveBlock = bid, true
			}
		}
		b.rowIDs = append(b.rowIDs, p.rowIDs[start:end]...)
		b.ordinals = append(b.ordinals, p.ordinals[start:end]...)
		b.changes = append(b.changes, p.changes[start:end]...)
	}
	return nil
}

// finalize closes the current table's shard: append the terminal runStart
// sentinel, install the completed rowShard, and reset the per-table arrays for
// the next table. No-op when no table is active.
func (b *rowShardBuilder) finalize() error {
	if !b.have {
		return nil
	}
	b.runStart = append(b.runStart, uint32(len(b.rowIDs)))
	b.done[b.table] = &rowShard{
		rowIDs:   packRowIDs(b.rowIDs),
		ordinals: b.ordinals,
		changes:  b.changes,
		runStart: b.runStart,
		blockIDs: b.blockIDs,
	}
	b.table = 0
	b.have = false
	b.rowIDs = nil
	b.ordinals = nil
	b.changes = nil
	b.runStart = nil
	b.blockIDs = nil
	b.prevBlock = 0
	b.haveBlock = false
	return nil
}

// finish finalizes the last active shard and returns the per-table rowShard
// map, or nil when no rows were appended.
func (b *rowShardBuilder) finish() (map[uint32]*rowShard, error) {
	if err := b.finalize(); err != nil {
		return nil, err
	}
	if len(b.done) == 0 {
		return nil, nil
	}
	return b.done, nil
}
