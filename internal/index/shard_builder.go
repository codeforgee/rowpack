package index

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// rowShardBuilder incrementally builds per-table rowShards from a stream of
// RowIndexEntry values that arrive sorted by (TableID, RowID) — the order the
// sorted Row Index Pages are decoded (encodePage guarantees this). Each
// entry is appended directly into the columnar (rowIDs/ordinals/changes) and
// block-run (runStart/blockIDs) arrays, so no []RowKeyLoc intermediate and no
// fully materialized []RowIndexEntry page ever exists (S3-⑦ 落盘② Open peak).
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
func (b *rowShardBuilder) AddRowEntry(e fileformat.RowIndexEntry) error {
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

// finalize closes the current table's shard: append the terminal runStart
// sentinel, install the completed rowShard, and reset the per-table arrays for
// the next table. No-op when no table is active.
func (b *rowShardBuilder) finalize() error {
	if !b.have {
		return nil
	}
	b.runStart = append(b.runStart, uint32(len(b.rowIDs)))
	b.done[b.table] = &rowShard{
		rowIDs:   b.rowIDs,
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
