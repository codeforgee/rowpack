// Package index implements the .rpi index transaction format, its sequential
// replay, and the immutable in-memory index view that all reads resolve
// against. A view is published atomically on commit and never mutated in
// place, so concurrent readers can share it without locks.
package index

import (
	"fmt"
	"sort"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// SnapshotMeta is the resolved per-snapshot summary in the view.
type SnapshotMeta struct {
	ID                 uint64
	Parent             uint64
	Type               fileformat.SnapshotType
	CreatedAtUnixNano  int64
	BlockCount         uint32
	MetadataBlockCount uint32
	RowRecordCount     uint64
	RawBytes           uint64
	StoredBytes        uint64
	DataStart          uint64
	DataEnd            uint64
	DataFooterCRC      uint32
	Depth              uint32 // chain depth; FULL has depth 1
}

// BlockLoc locates a block in the .rpk data file.
type BlockLoc struct {
	BlockID     uint64
	SnapshotID  uint64
	TableID     uint32
	Kind        fileformat.BlockKind
	Compression fileformat.Compression
	DataOffset  uint64
	RawSize     uint32
	StoredSize  uint32
	ItemCount   uint32
	RawCRC32C   uint32
}

// RowLoc locates one row record inside a block payload.
type RowLoc struct {
	BlockID     uint64
	ItemOrdinal uint32
	ChangeType  fileformat.ChangeType
}

// MetadataLoc locates one metadata record inside a block payload.
type MetadataLoc struct {
	ObjectID    uint64
	Revision    uint32
	RecordType  uint32
	BlockID     uint64
	ItemOrdinal uint32
	Operation   fileformat.Operation
}

// View is an immutable snapshot of all committed index state. Every map is
// owned exclusively by the view; once built it is never modified in place.
// Commit builds a new view via Apply with copy-on-write of the touched maps.
//
// Row index memory: rows are stored as one compact sorted slice per
// (snapshot, table) shard instead of per-row map cells, cutting the resident
// footprint from ~120 B/row to 24 B/row (1M rows: ~320 MB -> ~24 MB).
type View struct {
	snapshots      map[uint64]*SnapshotMeta
	blocks         map[uint64]*BlockLoc
	blocksBySnap   map[uint64][]uint64                // SnapshotID -> sorted BlockIDs
	metadata       map[uint64]map[uint64]*MetadataLoc // SnapshotID -> ObjectID -> loc
	metadataByType map[uint64]map[uint32][]uint64     // SnapshotID -> RecordType -> sorted ObjectIDs
	rows           map[uint64]map[uint32]*rowShard    // SnapshotID -> TableID -> shard

	memoryBytes uint64
}

// rowShard is the compact row index of one (snapshot, table): entries sorted
// by RowID, tombstones included. Immutable once built.
type rowShard struct {
	entries []RowKeyLoc
}

// rowShardLookup binary-searches the shard for rowID and returns a pointer to
// the entry (valid for the shard's lifetime), or nil.
func (sh *rowShard) lookup(rowID uint64) *RowKeyLoc {
	i := sort.Search(len(sh.entries), func(i int) bool { return sh.entries[i].RowID >= rowID })
	if i >= len(sh.entries) || sh.entries[i].RowID != rowID {
		return nil
	}
	return &sh.entries[i]
}

// EmptyView returns an empty immutable view.
func EmptyView() *View {
	return &View{
		snapshots:      make(map[uint64]*SnapshotMeta),
		blocks:         make(map[uint64]*BlockLoc),
		blocksBySnap:   make(map[uint64][]uint64),
		metadata:       make(map[uint64]map[uint64]*MetadataLoc),
		metadataByType: make(map[uint64]map[uint32][]uint64),
		rows:           make(map[uint64]map[uint32]*rowShard),
	}
}

// Snapshot returns the meta of a committed snapshot, or nil.
func (v *View) Snapshot(id uint64) *SnapshotMeta { return v.snapshots[id] }

// LatestSnapshot returns the highest committed snapshot, or nil.
func (v *View) LatestSnapshot() *SnapshotMeta {
	var latest *SnapshotMeta
	for _, s := range v.snapshots {
		if latest == nil || s.ID > latest.ID {
			latest = s
		}
	}
	return latest
}

// Snapshots returns all snapshot metas sorted by ID.
func (v *View) Snapshots() []*SnapshotMeta {
	out := make([]*SnapshotMeta, 0, len(v.snapshots))
	for _, s := range v.snapshots {
		out = append(out, s)
	}
	sortSnapshotMetas(out)
	return out
}

// Block returns the block location, or nil.
func (v *View) Block(blockID uint64) *BlockLoc { return v.blocks[blockID] }

// Blocks returns all block locations (order unspecified).
func (v *View) Blocks() []*BlockLoc {
	out := make([]*BlockLoc, 0, len(v.blocks))
	for _, b := range v.blocks {
		out = append(out, b)
	}
	return out
}

// BlockIDs returns the sorted block IDs of a snapshot.
func (v *View) BlockIDs(snapshot uint64) []uint64 { return v.blocksBySnap[snapshot] }

// Row returns the row location of (snapshot, table, rowID), or nil. The
// returned pointer aliases the immutable row shard and must be treated as
// read-only.
func (v *View) Row(snapshot uint64, table uint32, rowID uint64) *RowLoc {
	sh := v.rows[snapshot][table]
	if sh == nil {
		return nil
	}
	if e := sh.lookup(rowID); e != nil {
		return &e.Loc
	}
	return nil
}

// RowKeys returns the row locations of a (snapshot, table), sorted by RowID.
// The returned slice aliases the immutable shard (no copy, no sort): callers
// must treat it as read-only.
func (v *View) RowKeys(snapshot uint64, table uint32) []RowKeyLoc {
	sh := v.rows[snapshot][table]
	if sh == nil {
		return nil
	}
	return sh.entries
}

// RowKeyLoc pairs a RowID with its location, for sorted iteration. Loc is a
// value: shards are packed, so a row costs 24 bytes of resident index memory.
type RowKeyLoc struct {
	RowID uint64
	Loc   RowLoc
}

// Metadata returns the metadata location of (snapshot, objectID), or nil.
func (v *View) Metadata(snapshot uint64, objectID uint64) *MetadataLoc {
	m := v.metadata[snapshot]
	if m == nil {
		return nil
	}
	return m[objectID]
}

// MetadataByType returns the sorted object IDs of a record type in a snapshot.
func (v *View) MetadataByType(snapshot uint64, recordType uint32) []uint64 {
	return v.metadataByType[snapshot][recordType]
}

// MemoryBytes estimates the in-memory footprint of the view.
func (v *View) MemoryBytes() uint64 { return v.memoryBytes }

// ResolveRow finds the row location for (snapshot, table, rowID) along the
// parent chain. It returns nil when no record exists.
func (v *View) ResolveRow(snapshot uint64, table uint32, rowID uint64) *RowLoc {
	cur := snapshot
	for {
		loc := v.Row(cur, table, rowID)
		if loc != nil {
			return loc
		}
		sm := v.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			return nil
		}
		cur = sm.Parent
	}
}

// RowTables returns the table IDs that have row entries at the snapshot.
func (v *View) RowTables(snapshot uint64) []uint32 {
	tbl := v.rows[snapshot]
	if tbl == nil {
		return nil
	}
	out := make([]uint32, 0, len(tbl))
	for t := range tbl {
		out = append(out, t)
	}
	sortU32s(out)
	return out
}

// LogicalRowCount returns the number of rows visible at a snapshot for a
// table after resolving overrides and tombstones along the parent chain. It
// merges the per-layer sorted incremental indexes without reading blocks.
func (v *View) LogicalRowCount(snapshot uint64, table uint32) uint64 {
	type layer struct {
		keys  []RowKeyLoc
		pos   int
		depth int
	}
	type rowHeap []*layer
	less := func(h rowHeap, i, j int) bool {
		a, b := h[i].keys[h[i].pos], h[j].keys[h[j].pos]
		if a.RowID != b.RowID {
			return a.RowID < b.RowID
		}
		return h[i].depth < h[j].depth
	}
	push := func(h *rowHeap, l *layer) {
		*h = append(*h, l)
		i := len(*h) - 1
		for i > 0 {
			p := (i - 1) / 2
			if less(*h, i, p) {
				(*h)[i], (*h)[p] = (*h)[p], (*h)[i]
				i = p
			} else {
				break
			}
		}
	}
	pop := func(h *rowHeap) *layer {
		top := (*h)[0]
		(*h)[0] = (*h)[len(*h)-1]
		*h = (*h)[:len(*h)-1]
		i := 0
		for {
			l, r := 2*i+1, 2*i+2
			m := i
			if l < len(*h) && less(*h, l, m) {
				m = l
			}
			if r < len(*h) && less(*h, r, m) {
				m = r
			}
			if m == i {
				break
			}
			(*h)[i], (*h)[m] = (*h)[m], (*h)[i]
			i = m
		}
		return top
	}
	advance := func(h *rowHeap, l *layer) {
		l.pos++
		if l.pos < len(l.keys) {
			push(h, l)
		}
	}
	var layers []*layer
	cur := snapshot
	for depth := 0; ; depth++ {
		keys := v.RowKeys(cur, table)
		if len(keys) > 0 {
			layers = append(layers, &layer{keys: keys, depth: depth})
		}
		sm := v.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			break
		}
		cur = sm.Parent
	}
	var h rowHeap
	for _, l := range layers {
		push(&h, l)
	}
	var count uint64
	for len(h) > 0 {
		winner := pop(&h)
		rowID := winner.keys[winner.pos].RowID
		loc := winner.keys[winner.pos].Loc
		for len(h) > 0 && h[0].keys[h[0].pos].RowID == rowID {
			advance(&h, pop(&h))
		}
		advance(&h, winner)
		if loc.ChangeType != fileformat.ChangeDelete {
			count++
		}
	}
	return count
}

// Apply returns a NEW immutable view that adds the committed txn's entries.
// It validates the snapshot parent chain, uniqueness, and limits before
// returning. The receiver is not modified.
func (v *View) Apply(t *Txn, maxDepth uint32) (*View, error) {
	if t == nil {
		return nil, fmt.Errorf("rowpack: nil txn")
	}
	nv := v.shallowCopy()
	// Snapshot entry.
	se := &t.Snapshot
	if _, dup := v.snapshots[se.SnapshotID]; dup {
		return nil, fmt.Errorf("rowpack: snapshot %d already committed", se.SnapshotID)
	}
	depth := uint32(1)
	if se.SnapshotType == fileformat.SnapshotDelta {
		parent := v.snapshots[se.ParentSnapshotID]
		if parent == nil {
			return nil, fmt.Errorf("rowpack: DELTA snapshot %d parent %d not committed", se.SnapshotID, se.ParentSnapshotID)
		}
		if se.ParentSnapshotID >= se.SnapshotID {
			return nil, fmt.Errorf("rowpack: DELTA snapshot %d parent %d not smaller", se.SnapshotID, se.ParentSnapshotID)
		}
		if se.SnapshotType != fileformat.SnapshotDelta && se.SnapshotType != fileformat.SnapshotFull {
			return nil, fmt.Errorf("rowpack: snapshot %d bad type %d", se.SnapshotID, se.SnapshotType)
		}
		depth = parent.Depth + 1
		if depth > maxDepth {
			return nil, fmt.Errorf("rowpack: snapshot %d depth %d exceeds limit %d", se.SnapshotID, depth, maxDepth)
		}
	} else if se.SnapshotType != fileformat.SnapshotFull {
		return nil, fmt.Errorf("rowpack: snapshot %d bad type %d", se.SnapshotID, se.SnapshotType)
	}
	if se.ParentSnapshotID != 0 && se.SnapshotType == fileformat.SnapshotFull {
		return nil, fmt.Errorf("rowpack: FULL snapshot %d has parent %d", se.SnapshotID, se.ParentSnapshotID)
	}

	meta := &SnapshotMeta{
		ID:                se.SnapshotID,
		Parent:            se.ParentSnapshotID,
		Type:              se.SnapshotType,
		CreatedAtUnixNano: se.CreatedUnixNano,
		BlockCount:        se.BlockCount,
		RowRecordCount:    se.RowRecordCount,
		DataStart:         se.DataStart,
		DataEnd:           se.DataEnd,
		DataFooterCRC:     se.DataFooterCRC32C,
		Depth:             depth,
	}
	nv.snapshots[se.SnapshotID] = meta

	// Blocks.
	for i := range t.Blocks {
		be := &t.Blocks[i]
		if be.SnapshotID != se.SnapshotID {
			return nil, fmt.Errorf("rowpack: block %d belongs to snapshot %d, want %d", be.BlockID, be.SnapshotID, se.SnapshotID)
		}
		if _, dup := v.blocks[be.BlockID]; dup {
			return nil, fmt.Errorf("rowpack: block %d already exists", be.BlockID)
		}
		bl := &BlockLoc{
			BlockID: be.BlockID, SnapshotID: be.SnapshotID, TableID: be.TableID,
			Kind: be.BlockKind, Compression: be.Compression, DataOffset: be.DataOffset,
			RawSize: be.RawSize, StoredSize: be.StoredSize, ItemCount: be.ItemCount,
			RawCRC32C: be.RawCRC32C,
		}
		nv.blocks[be.BlockID] = bl
		nv.blocksBySnap[se.SnapshotID] = append(nv.blocksBySnap[se.SnapshotID], be.BlockID)
		meta.StoredBytes += uint64(be.StoredSize)
	}
	sortU64s(nv.blocksBySnap[se.SnapshotID])

	// Metadata.
	metaMap := make(map[uint64]*MetadataLoc)
	typeMap := make(map[uint32][]uint64)
	for i := range t.Metadata {
		me := &t.Metadata[i]
		if me.SnapshotID != se.SnapshotID {
			return nil, fmt.Errorf("rowpack: metadata entry %d wrong snapshot", me.ObjectID)
		}
		if _, dup := metaMap[me.ObjectID]; dup {
			return nil, fmt.Errorf("rowpack: metadata object %d duplicated in snapshot %d", me.ObjectID, se.SnapshotID)
		}
		metaMap[me.ObjectID] = &MetadataLoc{
			ObjectID: me.ObjectID, Revision: me.Revision, RecordType: me.RecordType,
			BlockID: me.BlockID, ItemOrdinal: me.ItemOrdinal, Operation: me.Operation,
		}
		typeMap[me.RecordType] = append(typeMap[me.RecordType], me.ObjectID)
	}
	for k := range typeMap {
		sortU64s(typeMap[k])
	}
	nv.metadata[se.SnapshotID] = metaMap
	nv.metadataByType[se.SnapshotID] = typeMap

	// Rows: build one compact sorted shard per table. Tombstones are kept
	// (readers filter them), duplicates within (snapshot, table) are rejected.
	rowSets := make(map[uint32][]RowKeyLoc)
	for i := range t.Rows {
		re := &t.Rows[i]
		if re.SnapshotID != se.SnapshotID {
			return nil, fmt.Errorf("rowpack: row entry wrong snapshot")
		}
		rowSets[re.TableID] = append(rowSets[re.TableID], RowKeyLoc{
			RowID: re.RowID,
			Loc:   RowLoc{BlockID: re.BlockID, ItemOrdinal: re.ItemOrdinal, ChangeType: re.ChangeType},
		})
	}
	rowMap := make(map[uint32]*rowShard, len(rowSets))
	for tid, entries := range rowSets {
		// Fast path: the writer's insertion order is already sorted in the
		// common sequential case; verify before sorting.
		sorted := true
		for i := 1; i < len(entries); i++ {
			if entries[i].RowID < entries[i-1].RowID {
				sorted = false
				break
			}
		}
		if !sorted {
			sortRowKeyLocs(entries)
		}
		for i := 1; i < len(entries); i++ {
			if entries[i].RowID == entries[i-1].RowID {
				return nil, fmt.Errorf("rowpack: duplicate (table %d, row %d) in snapshot %d", tid, entries[i].RowID, se.SnapshotID)
			}
		}
		rowMap[tid] = &rowShard{entries: entries}
	}
	nv.rows[se.SnapshotID] = rowMap

	// Memory estimate: rough per-entry overhead plus map cells and packed
	// row-shard entries (24 B per row).
	nv.memoryBytes = v.memoryBytes
	nv.memoryBytes += 64 + uint64(len(t.Metadata))*56 + uint64(len(t.Blocks))*72
	for _, entries := range rowSets {
		nv.memoryBytes += 48 + uint64(len(entries))*24
	}
	return nv, nil
}

// shallowCopy copies the top-level maps so the new view can add the new
// snapshot's keys without touching shared maps.
func (v *View) shallowCopy() *View {
	nv := &View{
		snapshots:      make(map[uint64]*SnapshotMeta, len(v.snapshots)+1),
		blocks:         make(map[uint64]*BlockLoc, len(v.blocks)+8),
		blocksBySnap:   make(map[uint64][]uint64, len(v.blocksBySnap)+1),
		metadata:       make(map[uint64]map[uint64]*MetadataLoc, len(v.metadata)+1),
		metadataByType: make(map[uint64]map[uint32][]uint64, len(v.metadataByType)+1),
		rows:           make(map[uint64]map[uint32]*rowShard, len(v.rows)+1),
	}
	for k, s := range v.snapshots {
		nv.snapshots[k] = s
	}
	for k, b := range v.blocks {
		nv.blocks[k] = b
	}
	for k, ids := range v.blocksBySnap {
		nv.blocksBySnap[k] = ids
	}
	for k, m := range v.metadata {
		nv.metadata[k] = m
	}
	for k, m := range v.metadataByType {
		nv.metadataByType[k] = m
	}
	for k, r := range v.rows {
		nv.rows[k] = r
	}
	nv.memoryBytes = v.memoryBytes
	return nv
}
