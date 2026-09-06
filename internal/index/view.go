// Package index implements the .rpi index transaction format, its sequential
// replay, and the immutable in-memory index view that all reads resolve
// against. A view is published atomically on commit and never mutated in
// place, so concurrent readers can share it without locks.
package index

import (
	"fmt"

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
type View struct {
	snapshots      map[uint64]*SnapshotMeta
	blocks         map[uint64]*BlockLoc
	blocksBySnap   map[uint64][]uint64                      // SnapshotID -> sorted BlockIDs
	metadata       map[uint64]map[uint64]*MetadataLoc       // SnapshotID -> ObjectID -> loc
	metadataByType map[uint64]map[uint32][]uint64           // SnapshotID -> RecordType -> sorted ObjectIDs
	rows           map[uint64]map[uint32]map[uint64]*RowLoc // SnapshotID -> TableID -> RowID -> loc

	memoryBytes uint64
}

// EmptyView returns an empty immutable view.
func EmptyView() *View {
	return &View{
		snapshots:      make(map[uint64]*SnapshotMeta),
		blocks:         make(map[uint64]*BlockLoc),
		blocksBySnap:   make(map[uint64][]uint64),
		metadata:       make(map[uint64]map[uint64]*MetadataLoc),
		metadataByType: make(map[uint64]map[uint32][]uint64),
		rows:           make(map[uint64]map[uint32]map[uint64]*RowLoc),
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

// Row returns the row location of (snapshot, table, rowID), or nil.
func (v *View) Row(snapshot uint64, table uint32, rowID uint64) *RowLoc {
	tbl := v.rows[snapshot]
	if tbl == nil {
		return nil
	}
	return tbl[table][rowID]
}

// RowKeys returns the row locations of a (snapshot, table), sorted by RowID.
func (v *View) RowKeys(snapshot uint64, table uint32) []rowKeyLoc {
	tbl := v.rows[snapshot]
	if tbl == nil {
		return nil
	}
	rows := tbl[table]
	if len(rows) == 0 {
		return nil
	}
	out := make([]rowKeyLoc, 0, len(rows))
	for id, loc := range rows {
		out = append(out, rowKeyLoc{RowID: id, Loc: loc})
	}
	sortRowKeyLocs(out)
	return out
}

// rowKeyLoc pairs a RowID with its location, for sorted iteration.
type rowKeyLoc struct {
	RowID uint64
	Loc   *RowLoc
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

	// Rows.
	rowMap := make(map[uint32]map[uint64]*RowLoc)
	for i := range t.Rows {
		re := &t.Rows[i]
		if re.SnapshotID != se.SnapshotID {
			return nil, fmt.Errorf("rowpack: row entry wrong snapshot")
		}
		tbl := rowMap[re.TableID]
		if tbl == nil {
			tbl = make(map[uint64]*RowLoc)
			rowMap[re.TableID] = tbl
		}
		if _, dup := tbl[re.RowID]; dup {
			return nil, fmt.Errorf("rowpack: duplicate (table %d, row %d) in snapshot %d", re.TableID, re.RowID, se.SnapshotID)
		}
		tbl[re.RowID] = &RowLoc{BlockID: re.BlockID, ItemOrdinal: re.ItemOrdinal, ChangeType: re.ChangeType}
	}
	nv.rows[se.SnapshotID] = rowMap

	// Memory estimate: rough per-entry overhead plus map cells.
	nv.memoryBytes = v.memoryBytes
	nv.memoryBytes += 64 + uint64(len(t.Metadata))*56 + uint64(len(t.Blocks))*72 + uint64(len(t.Rows))*64
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
		rows:           make(map[uint64]map[uint32]map[uint64]*RowLoc, len(v.rows)+1),
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
