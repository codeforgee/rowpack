// Package index implements the index transaction format, its sequential
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
	metadata       map[uint64]map[uint64]*MetadataLoc // SnapshotID -> ObjectID -> loc
	metadataByType map[uint64]map[uint32][]uint64     // SnapshotID -> RecordType -> sorted ObjectIDs
	rows           map[uint64]map[uint32]*rowShard    // SnapshotID -> TableID -> shard

	memoryBytes uint64
}

// rowShard is the compact per-(snapshot, table) row index: rows sorted by
// RowID, stored columnar (RowIDs / ItemOrdinals / ChangeTypes) with the
// BlockID run-length encoded so consecutive rows inside one physical block do
// not repeat the block id. Residential cost ~13 B/row (vs 24 B/row for a
// []RowKeyLoc), meeting the Eager index memory gate. Immutable once built.
type rowShard struct {
	rowIDs   []uint64 // sorted by RowID, len n
	ordinals []uint32 // ItemOrdinal per row
	changes  []uint8  // ChangeType per row
	runStart []uint32 // runStart[r] = first row index of run r; runStart[len]=n
	blockIDs []uint64 // BlockID of each run
}

// len returns the number of rows.
func (sh *rowShard) len() int { return len(sh.rowIDs) }

// rowIDAt returns the RowID at index i (sorted order).
func (sh *rowShard) rowIDAt(i int) uint64 { return sh.rowIDs[i] }

// runFor finds the run index owning row index i via binary search.
func (sh *rowShard) runFor(i int) int {
	r := sort.Search(len(sh.blockIDs), func(r int) bool { return int(sh.runStart[r+1]) > i })
	if r >= len(sh.blockIDs) {
		r = len(sh.blockIDs) - 1
	}
	return r
}

// rowLocAt returns the RowLoc of the row at index i.
func (sh *rowShard) rowLocAt(i int) RowLoc {
	r := sh.runFor(i)
	return RowLoc{BlockID: sh.blockIDs[r], ItemOrdinal: sh.ordinals[i], ChangeType: fileformat.ChangeType(sh.changes[i])}
}

// lookup binary-searches the shard for rowID and returns its location.
func (sh *rowShard) lookup(rowID uint64) (RowLoc, bool) {
	if sh == nil {
		return RowLoc{}, false
	}
	i := sort.Search(len(sh.rowIDs), func(i int) bool { return sh.rowIDs[i] >= rowID })
	if i >= len(sh.rowIDs) || sh.rowIDs[i] != rowID {
		return RowLoc{}, false
	}
	return sh.rowLocAt(i), true
}

// RowKeyIter is a forward iterator over a compact rowShard in sorted RowID
// order. It exposes position-based random access (peek/advance) for a k-way
// merge and sequential walks without materializing a 24 B/row []RowKeyLoc, so
// a scan does not allocate a transient materialized copy of the shard.
type RowKeyIter struct {
	sh  *rowShard
	pos int
}

// Len returns the number of rows.
func (it *RowKeyIter) Len() int { return it.sh.len() }

// Done reports whether the iterator is exhausted.
func (it *RowKeyIter) Done() bool { return it.pos >= it.sh.len() }

// RowID returns the RowID at the current position.
func (it *RowKeyIter) RowID() uint64 { return it.sh.rowIDAt(it.pos) }

// Loc returns the RowLoc at the current position.
func (it *RowKeyIter) Loc() RowLoc { return it.sh.rowLocAt(it.pos) }

// Next advances to the next position.
func (it *RowKeyIter) Next() { it.pos++ }

// RowIter returns a fresh iterator over the shard for (snapshot, table), or
// nil when there are no rows.
func (v *View) RowIter(snapshot uint64, table uint32) *RowKeyIter {
	sh := v.rows[snapshot][table]
	if sh == nil {
		return nil
	}
	return &RowKeyIter{sh: sh}
}

// EmptyView returns an empty immutable view.
func EmptyView() *View {
	return &View{
		snapshots:      make(map[uint64]*SnapshotMeta),
		blocks:         make(map[uint64]*BlockLoc),
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

// Row returns the row location of (snapshot, table, rowID), or nil. The
// returned pointer aliases the immutable row shard and must be treated as
// read-only.
func (v *View) Row(snapshot uint64, table uint32, rowID uint64) (RowLoc, bool) {
	sh := v.rows[snapshot][table]
	if sh == nil {
		return RowLoc{}, false
	}
	return sh.lookup(rowID)
}

// RowKeyLoc pairs a RowID with its location. It is the intermediate form used
// while building a rowShard from the decoded entry stream (the builders
// materialize then compact it); the shard itself stores the columnar/block-run
// form rather than a []RowKeyLoc.
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
func (v *View) ResolveRow(snapshot uint64, table uint32, rowID uint64) (RowLoc, bool) {
	cur := snapshot
	for {
		if loc, ok := v.Row(cur, table, rowID); ok {
			return loc, true
		}
		sm := v.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			return RowLoc{}, false
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
		keys  *RowKeyIter
		depth int
	}
	type rowHeap []*layer
	less := func(h rowHeap, i, j int) bool {
		a, b := h[i].keys.RowID(), h[j].keys.RowID()
		if a != b {
			return a < b
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
		l.keys.Next()
		if !l.keys.Done() {
			push(h, l)
		}
	}
	var layers []*layer
	cur := snapshot
	for depth := 0; ; depth++ {
		keys := v.RowIter(cur, table)
		if keys != nil && keys.Len() > 0 {
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
		rowID := winner.keys.RowID()
		loc := winner.keys.Loc()
		for len(h) > 0 && h[0].keys.RowID() == rowID {
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
// returning. The receiver is not modified. This is the buffered form, used by
// the recovery rebuild path; the Open replay path uses ApplyStreaming to skip
// the intermediate []RowIndexEntry.
func (v *View) Apply(t *Txn, maxDepth uint32) (*View, error) {
	if t == nil {
		return nil, fmt.Errorf("rowpack: nil txn")
	}
	nv, meta, err := v.beginApply(t.Snapshot, maxDepth)
	if err != nil {
		return nil, err
	}
	if err := applyBlocks(v, nv, meta, t.Snapshot.SnapshotID, t.Blocks); err != nil {
		return nil, err
	}
	if err := applyMetadata(nv, t.Snapshot.SnapshotID, t.Metadata); err != nil {
		return nil, err
	}
	rowMap, err := buildRowShards(t, t.Snapshot.SnapshotID)
	if err != nil {
		return nil, err
	}
	nv.rows[t.Snapshot.SnapshotID] = rowMap
	finishApplyMemory(v, nv, len(t.Metadata), len(t.Blocks), rowMap)
	return nv, nil
}

// beginApply validates the snapshot entry against v (chain, uniqueness,
// depth) and installs it into the new view copy, returning the copy and its
// meta. Shared by Apply and the streaming sink.
func (v *View) beginApply(se fileformat.SnapshotIndexEntry, maxDepth uint32) (*View, *SnapshotMeta, error) {
	nv := v.shallowCopy()
	if _, dup := v.snapshots[se.SnapshotID]; dup {
		return nil, nil, fmt.Errorf("rowpack: snapshot %d already committed", se.SnapshotID)
	}
	depth := uint32(1)
	if se.SnapshotType == fileformat.SnapshotDelta {
		parent := v.snapshots[se.ParentSnapshotID]
		if parent == nil {
			return nil, nil, fmt.Errorf("rowpack: DELTA snapshot %d parent %d not committed", se.SnapshotID, se.ParentSnapshotID)
		}
		if se.ParentSnapshotID >= se.SnapshotID {
			return nil, nil, fmt.Errorf("rowpack: DELTA snapshot %d parent %d not smaller", se.SnapshotID, se.ParentSnapshotID)
		}
		depth = parent.Depth + 1
		if depth > maxDepth {
			return nil, nil, fmt.Errorf("rowpack: snapshot %d depth %d exceeds limit %d", se.SnapshotID, depth, maxDepth)
		}
	} else if se.SnapshotType != fileformat.SnapshotFull {
		return nil, nil, fmt.Errorf("rowpack: snapshot %d bad type %d", se.SnapshotID, se.SnapshotType)
	}
	if se.ParentSnapshotID != 0 && se.SnapshotType == fileformat.SnapshotFull {
		return nil, nil, fmt.Errorf("rowpack: FULL snapshot %d has parent %d", se.SnapshotID, se.ParentSnapshotID)
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
	return nv, meta, nil
}

// applyBlocks validates and installs the txn's block entries into nv,
// accumulating stored bytes into meta. Shared by Apply and the streaming sink.
func applyBlocks(old, nv *View, meta *SnapshotMeta, snapshotID uint64, blocks []fileformat.BlockIndexEntry) error {
	for i := range blocks {
		be := &blocks[i]
		if be.SnapshotID != snapshotID {
			return fmt.Errorf("rowpack: block %d belongs to snapshot %d, want %d", be.BlockID, be.SnapshotID, snapshotID)
		}
		if _, dup := old.blocks[be.BlockID]; dup {
			return fmt.Errorf("rowpack: block %d already exists", be.BlockID)
		}
		bl := &BlockLoc{
			BlockID: be.BlockID, SnapshotID: be.SnapshotID, TableID: be.TableID,
			Kind: be.BlockKind, Compression: be.Compression, DataOffset: be.DataOffset,
			RawSize: be.RawSize, StoredSize: be.StoredSize, ItemCount: be.ItemCount,
			RawCRC32C: be.RawCRC32C,
		}
		nv.blocks[be.BlockID] = bl
		meta.StoredBytes += uint64(be.StoredSize)
	}
	return nil
}

// applyMetadata builds and installs the metadata location maps for one
// snapshot. Shared by Apply and the streaming sink.
func applyMetadata(nv *View, snapshotID uint64, metadata []fileformat.MetadataIndexEntry) error {
	metaMap := make(map[uint64]*MetadataLoc)
	typeMap := make(map[uint32][]uint64)
	for i := range metadata {
		me := &metadata[i]
		if me.SnapshotID != snapshotID {
			return fmt.Errorf("rowpack: metadata entry %d wrong snapshot", me.ObjectID)
		}
		if _, dup := metaMap[me.ObjectID]; dup {
			return fmt.Errorf("rowpack: metadata object %d duplicated in snapshot %d", me.ObjectID, snapshotID)
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
	nv.metadata[snapshotID] = metaMap
	nv.metadataByType[snapshotID] = typeMap
	return nil
}

// finishApplyMemory applies the per-txn memory estimate to nv: rough
// per-entry overhead plus map cells and packed row-shard entries (24 B/row).
func finishApplyMemory(old, nv *View, nMeta, nBlocks int, rowMap map[uint32]*rowShard) {
	nv.memoryBytes = old.memoryBytes
	nv.memoryBytes += 64 + uint64(nMeta)*56 + uint64(nBlocks)*72
	for _, sh := range rowMap {
		// Columnar storage: RowID (8) + ItemOrdinal (4) + ChangeType (1) per row,
		// plus the block-run directory (BlockID + runStart per run).
		nv.memoryBytes += 48 +
			uint64(len(sh.rowIDs))*8 +
			uint64(len(sh.ordinals))*4 +
			uint64(len(sh.changes))*1 +
			uint64(len(sh.blockIDs))*8 +
			uint64(len(sh.runStart))*4
	}
}

// ApplyStreaming parses the serialized IndexTxn at data and applies it in one
// fused pass: entries stream from the parser straight into the new view's
// compact shard slices, never materializing Txn.Rows (~40 B/row off the Open
// peak). Validation semantics are identical to Apply(ParseTxnChunked(data)).
func (v *View) ApplyStreaming(data []byte, crypto *ChunkCrypto, maxDepth uint32) (*View, error) {
	ap := &streamApply{old: v, maxDepth: maxDepth}
	t, err := ParseTxnChunkedStreaming(data, crypto, ap)
	if err != nil {
		return nil, err
	}
	if err := ap.finish(t); err != nil {
		return nil, err
	}
	return ap.nv, nil
}

// streamApply is the TxnSink behind ApplyStreaming. Blocks and metadata are
// few (per flushed block), so they buffer; rows — the per-row bulk — append
// directly into an incremental rowShardBuilder, so no []RowKeyLoc intermediate
// and no fully materialized []RowIndexEntry page exists on the Open path. The
// first (single-table) shard preallocates exactly from the header's row count.
type streamApply struct {
	old      *View
	nv       *View
	maxDepth uint32

	meta   *SnapshotMeta
	snapID uint64

	blocks   []fileformat.BlockIndexEntry
	metadata []fileformat.MetadataIndexEntry

	shards   *rowShardBuilder
	hint     int
	rowCount uint64
}

func (a *streamApply) ReserveRows(hint int) { a.hint = hint }

func (a *streamApply) SetSnapshot(e fileformat.SnapshotIndexEntry) error {
	if a.meta != nil {
		return fmt.Errorf("rowpack: duplicate snapshot chunk")
	}
	nv, meta, err := a.old.beginApply(e, a.maxDepth)
	if err != nil {
		return err
	}
	a.nv = nv
	a.meta = meta
	a.snapID = e.SnapshotID
	return nil
}

func (a *streamApply) AddBlock(e fileformat.BlockIndexEntry) error {
	if a.meta == nil {
		return fmt.Errorf("rowpack: block entry before snapshot")
	}
	a.blocks = append(a.blocks, e)
	return nil
}

func (a *streamApply) AddMetadata(e fileformat.MetadataIndexEntry) error {
	if a.meta == nil {
		return fmt.Errorf("rowpack: metadata entry before snapshot")
	}
	a.metadata = append(a.metadata, e)
	return nil
}

// AddRowEntry implements RowEntrySink: the page parser (via walkRowIndexPage)
// hands each decoded entry straight to the shard builder, one at a time, so
// no per-page []RowIndexEntry is materialized.
func (a *streamApply) AddRowEntry(e fileformat.RowIndexEntry) error {
	if a.meta == nil {
		return fmt.Errorf("rowpack: row entry before snapshot")
	}
	if a.shards == nil {
		a.shards = newRowShardBuilder(a.snapID, a.hint)
	}
	if err := a.shards.AddRowEntry(e); err != nil {
		return err
	}
	a.rowCount++
	return nil
}

// AddRows implements TxnSink. On the Open path the parser uses AddRowEntry
// directly; AddRows remains as the batch fallback (buffered / non-streaming
// sinks) and delegates to AddRowEntry.
func (a *streamApply) AddRows(batch []fileformat.RowIndexEntry) error {
	for i := range batch {
		if err := a.AddRowEntry(batch[i]); err != nil {
			return err
		}
	}
	return nil
}

// finish installs the buffered blocks/metadata and the accumulated shards
// into the new view. Called after the parser validated header/footer CRCs
// and all entry counts.
func (a *streamApply) finish(t *Txn) error {
	if a.meta == nil {
		return fmt.Errorf("rowpack: index txn has no snapshot chunk")
	}
	if err := applyBlocks(a.old, a.nv, a.meta, a.snapID, a.blocks); err != nil {
		return err
	}
	if err := applyMetadata(a.nv, a.snapID, a.metadata); err != nil {
		return err
	}
	var rowMap map[uint32]*rowShard
	if a.shards != nil {
		var err error
		rowMap, err = a.shards.finish()
		if err != nil {
			return err
		}
	}
	a.nv.rows[a.snapID] = rowMap
	finishApplyMemory(a.old, a.nv, len(a.metadata), len(a.blocks), rowMap)
	return nil
}

// buildRowShards converts t.Rows into one sorted rowShard per table. It
// returns (nil, nil) when t has no row entries. Duplicates within a
// (snapshot, table) and entries owned by another snapshot are rejected.
func buildRowShards(t *Txn, snapshot uint64) (map[uint32]*rowShard, error) {
	n := len(t.Rows)
	if n == 0 {
		return nil, nil
	}
	checkOwned := func(re *fileformat.RowIndexEntry) error {
		if re.SnapshotID != snapshot {
			return fmt.Errorf("rowpack: row entry wrong snapshot")
		}
		return nil
	}
	first := t.Rows[0].TableID
	singleTable := true
	for i := 1; i < n; i++ {
		if t.Rows[i].TableID != first {
			singleTable = false
			break
		}
	}
	if singleTable {
		entries := make([]RowKeyLoc, n)
		for i := range t.Rows {
			re := &t.Rows[i]
			if err := checkOwned(re); err != nil {
				return nil, err
			}
			entries[i] = RowKeyLoc{
				RowID: re.RowID,
				Loc:   RowLoc{BlockID: re.BlockID, ItemOrdinal: re.ItemOrdinal, ChangeType: re.ChangeType},
			}
		}
		sh := &rowShard{}
		if err := sh.prepare(entries); err != nil {
			return nil, err
		}
		return map[uint32]*rowShard{first: sh}, nil
	}
	// Multi-table fallback: count rows per table, preallocate each shard
	// exactly, then fill by index (no capacity doubling).
	rowSets := make(map[uint32][]RowKeyLoc)
	counts := make(map[uint32]int)
	for i := range t.Rows {
		counts[t.Rows[i].TableID]++
	}
	for tid, c := range counts {
		rowSets[tid] = make([]RowKeyLoc, c)
	}
	pos := make(map[uint32]int, len(counts))
	for i := range t.Rows {
		re := &t.Rows[i]
		if err := checkOwned(re); err != nil {
			return nil, err
		}
		tid := re.TableID
		p := pos[tid]
		rowSets[tid][p] = RowKeyLoc{
			RowID: re.RowID,
			Loc:   RowLoc{BlockID: re.BlockID, ItemOrdinal: re.ItemOrdinal, ChangeType: re.ChangeType},
		}
		pos[tid] = p + 1
	}
	out := make(map[uint32]*rowShard, len(rowSets))
	for tid, entries := range rowSets {
		sh := &rowShard{}
		if err := sh.prepare(entries); err != nil {
			return nil, err
		}
		out[tid] = sh
	}
	return out, nil
}

// prepare sorts (when needed), validates, and converts shard entries into the
// compact SoA/block-run form. It rejects strict duplicates (v1 forbids
// duplicate RowKeys in one snapshot). After it returns, sh no longer holds the
// temporary []RowKeyLoc, so the resident footprint is the columnar form.
func (sh *rowShard) prepare(entries []RowKeyLoc) error {
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
	n := len(entries)
	sh.rowIDs = make([]uint64, n)
	sh.ordinals = make([]uint32, n)
	sh.changes = make([]uint8, n)
	sh.runStart = make([]uint32, 0, 8)
	sh.blockIDs = make([]uint64, 0, 8)
	for i := 0; i < n; i++ {
		e := entries[i]
		if i > 0 && e.RowID == entries[i-1].RowID {
			return fmt.Errorf("rowpack: duplicate row %d in snapshot", e.RowID)
		}
		sh.rowIDs[i] = e.RowID
		sh.ordinals[i] = e.Loc.ItemOrdinal
		sh.changes[i] = uint8(e.Loc.ChangeType)
		if i == 0 || e.Loc.BlockID != entries[i-1].Loc.BlockID {
			sh.runStart = append(sh.runStart, uint32(i))
			sh.blockIDs = append(sh.blockIDs, e.Loc.BlockID)
		}
	}
	sh.runStart = append(sh.runStart, uint32(n))
	return nil
}

// shallowCopy copies the top-level maps so the new view can add the new
// snapshot's keys without touching shared maps.
func (v *View) shallowCopy() *View {
	nv := &View{
		snapshots:      make(map[uint64]*SnapshotMeta, len(v.snapshots)+1),
		blocks:         make(map[uint64]*BlockLoc, len(v.blocks)+8),
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
