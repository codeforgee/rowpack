// Package index implements the index transaction format, its sequential
// replay, and the immutable in-memory index view that all reads resolve
// against. A view is published atomically on commit and never mutated in
// place, so concurrent readers can share it without locks.
package index

import (
	"cmp"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"sort"

	"github.com/codeforgee/rowpack/internal/format"
)

// SnapshotMeta is the resolved per-snapshot summary in the view.
type SnapshotMeta struct {
	ID                 uint64
	Parent             uint64
	Type               format.SnapshotType
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

	// chain is the ancestor chain, target first: [ID, Parent, ..., FULL
	// root]. Precomputed at Apply time (the parent chain is validated there),
	// so every chain-resolving read — row lookup, schema derivation, meta
	// inheritance — iterates a slice instead of re-walking Parent links.
	// Read-only: never mutated after Apply.
	chain []uint64
}

// Chain returns the snapshot's ancestor chain, target first: [ID, Parent,
// ..., FULL root]. The returned slice is shared immutable state; callers must
// not modify it. A meta built outside Apply (tests) falls back to itself as
// the only layer.
func (m *SnapshotMeta) Chain() []uint64 {
	if m.chain == nil {
		return []uint64{m.ID}
	}
	return m.chain
}

// BlockLoc locates a block in the .rpk data file.
type BlockLoc struct {
	BlockID     uint64
	SnapshotID  uint64
	TableID     uint32
	Kind        format.BlockKind
	Compression format.Compression
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
	ChangeType  format.ChangeType
}

// MetadataLoc locates one metadata record inside a block payload.
type MetadataLoc struct {
	ObjectID    uint64
	Revision    uint32
	RecordType  uint32
	BlockID     uint64
	ItemOrdinal uint32
	Operation   format.Operation
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
	rows           map[uint64]map[uint32]*rowShard

	memoryBytes uint64
}

// rowShard is the compact per-(snapshot, table) row index: rows sorted by
// RowID, stored columnar (RowIDs / ItemOrdinals / ChangeTypes) with the
// BlockID run-length encoded so consecutive rows inside one physical block do
// not repeat the block id. RowIDs are additionally frame-of-reference
// bit-packed (see packedRowIDs), which is what keeps the Eager index inside its
// memory gate. Immutable once built.
type rowShard struct {
	rowIDs   packedRowIDs // sorted by RowID, frame-of-reference bit-packed
	ordinals []uint32     // ItemOrdinal per row
	changes  []uint8      // ChangeType per row
	runStart []uint32     // runStart[r] = first row index of run r; runStart[len]=n
	blockIDs []uint64     // BlockID of each run
}

// len returns the number of rows.
func (sh *rowShard) len() int { return sh.rowIDs.n }

// rowIDAt returns the RowID at index i (sorted order). Decoding costs
// O(rowPackFrame); callers walking the shard in index order should use
// rowShardIter, which keeps a frame cursor and amortizes to O(1) per step.
func (sh *rowShard) rowIDAt(i int) uint64 { return sh.rowIDs.at(i) }

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
	return RowLoc{BlockID: sh.blockIDs[r], ItemOrdinal: sh.ordinals[i], ChangeType: format.ChangeType(sh.changes[i])}
}

// lookup binary-searches the shard for rowID and returns its location.
func (sh *rowShard) lookup(rowID uint64) (RowLoc, bool) {
	if sh == nil {
		return RowLoc{}, false
	}
	i, ok := sh.rowIDs.search(rowID)
	if !ok {
		return RowLoc{}, false
	}
	return sh.rowLocAt(i), true
}

// rowIter is the forward-iteration contract shared by the eager rowShard and
// the lazy page iterator: a scan/merge can walk rows in sorted RowID order
// without materializing a []RowKeyLoc or the whole table index.
type rowIter interface {
	Len() int
	Done() bool
	RowID() uint64
	Loc() RowLoc
	Next()
	Seek(uint64)
}

// RowKeyIter is a forward iterator over rows of one (snapshot, table) in
// sorted RowID order. In Eager mode it walks a compact rowShard SoA without
// allocating a transient []RowKeyLoc; in Lazy mode it streams sorted Row Index
// Pages one at a time. Both expose the same position-based access for a k-way
// merge (parent chains) and sequential scans.
type RowKeyIter struct {
	it rowIter
}

// Len returns the number of rows.
func (it *RowKeyIter) Len() int { return it.it.Len() }

// Done reports whether the iterator is exhausted.
func (it *RowKeyIter) Done() bool { return it.it.Done() }

// RowID returns the RowID at the current position.
func (it *RowKeyIter) RowID() uint64 { return it.it.RowID() }

// Loc returns the RowLoc at the current position.
func (it *RowKeyIter) Loc() RowLoc { return it.it.Loc() }

// Next advances to the next position.
func (it *RowKeyIter) Next() { it.it.Next() }

// Seek advances to the first RowID greater than or equal to target.
func (it *RowKeyIter) Seek(target uint64) { it.it.Seek(target) }

// rowShardIter is the Eager rowIter over a compact rowShard. It holds a cursor
// inside the packed RowID column — current frame, running RowID, and the bit
// offset of the next delta — so Next stays O(1). Without the cursor every
// RowID() would replay the frame from its base and a full scan would cost
// O(n*rowPackFrame).
type rowShardIter struct {
	sh     *rowShard
	pos    int
	frame  int    // frame the cursor sits on; -1 once exhausted
	rowID  uint64 // RowID at pos, kept current by Next
	bitPos int    // bit offset of the delta that produces pos+1
}

func newRowShardIter(sh *rowShard) *rowShardIter {
	it := &rowShardIter{sh: sh}
	it.seekIndex(0)
	return it
}

func (it *rowShardIter) Len() int   { return it.sh.len() }
func (it *rowShardIter) Done() bool { return it.pos >= it.sh.len() }

// RowID returns the RowID at the current position.
func (it *rowShardIter) RowID() uint64 { return it.rowID }
func (it *rowShardIter) Loc() RowLoc   { return it.sh.rowLocAt(it.pos) }

// Next advances one row. Inside a frame it reads the next delta; crossing a
// frame boundary re-decodes, which happens once per rowPackFrame rows.
func (it *rowShardIter) Next() {
	it.pos++
	if it.pos >= it.sh.len() {
		it.frame = -1
		return
	}
	if f := it.pos / rowPackFrame; f != it.frame {
		it.seekIndex(it.pos)
		return
	}
	w := int(it.sh.rowIDs.width[it.frame])
	it.rowID += getBits(it.sh.rowIDs.data, it.bitPos, w)
	it.bitPos += w
}

// Seek advances to the first RowID greater than or equal to target.
func (it *rowShardIter) Seek(target uint64) {
	i, _ := it.sh.rowIDs.search(target)
	it.seekIndex(i)
}

// seekIndex positions the cursor on shard index i and materializes the running
// RowID for it.
func (it *rowShardIter) seekIndex(i int) {
	p := &it.sh.rowIDs
	if i >= p.n {
		it.pos = p.n
		it.frame = -1
		it.rowID = 0
		return
	}
	it.pos = i
	f := i / rowPackFrame
	it.frame = f
	w := int(p.width[f])
	it.rowID = p.at(i)
	it.bitPos = int(p.bitOff[f]) + (i-f*rowPackFrame+1)*w
}

// RowIter returns a fresh iterator over rows for (snapshot, table), or nil
// when there are no rows.
func (v *View) RowIter(snapshot uint64, table uint32) *RowKeyIter {
	sh := v.rows[snapshot][table]
	if sh == nil {
		return nil
	}
	return &RowKeyIter{it: newRowShardIter(sh)}
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
	slices.SortFunc(out, func(a, b *SnapshotMeta) int { return cmp.Compare(a.ID, b.ID) })
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
// returned pointer aliases immutable row-shard or cached page entries and must
// be treated as read-only.
func (v *View) Row(snapshot uint64, table uint32, rowID uint64) (RowLoc, bool) {
	// Per-snapshot: a lazy snapshot uses the fence+page source; a snapshot that
	// was rebuilt eagerly (corrupt-txn recovery fallback) uses rowShards.
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

// MetadataObjects returns every metadata object ID visible in a snapshot layer,
// sorted ascending. Record types are not distinguished: callers that need to
// bound an ID space must see records of types they do not otherwise query.
func (v *View) MetadataObjects(snapshot uint64) []uint64 {
	m := v.metadata[snapshot]
	if len(m) == 0 {
		return nil
	}
	out := make([]uint64, 0, len(m))
	for oid := range m {
		out = append(out, oid)
	}
	slices.Sort(out)
	return out
}

// MemoryBytes estimates the in-memory footprint of the view.
func (v *View) MemoryBytes() uint64 { return v.memoryBytes }

// MemoryBreakdown splits the residential index estimate into the parts that
// make it up. MemoryBytes is one number on purpose — it is a gate, not an
// accounting — but a size change is only actionable once you can see which
// part moved, so this exists purely as a diagnostic for tooling.
//
// The numbers are the estimate's own terms, not measured RSS: a shard costs
// its slices plus a fixed 48 B, and nothing here accounts for Go slice
// capacity overshoot or map cells.
type MemoryBreakdown struct {
	Shards          int    // distinct (snapshot, table) row shards
	RowEntries      int    // index entries summed over every shard
	Runs            int    // block runs: one per run, not one per row
	RowIDsBytes     uint64 // rowIDs   bit-packed, see packedRowIDs.bytes
	OrdinalsBytes   uint64 // ordinals 4 B/entry
	ChangesBytes    uint64 // changes  1 B/entry
	RunStartBytes   uint64 // runStart 4 B/run
	BlockIDsBytes   uint64 // blockIDs 8 B/run
	ShardFixedBytes uint64 // 48 B/shard whatever its size
	SlackBytes      uint64 // cap-len: capacity append reserved and never filled
}

// SliceBytes is the columnar payload: the part that scales with entries.
func (b MemoryBreakdown) SliceBytes() uint64 {
	return b.RowIDsBytes + b.OrdinalsBytes + b.ChangesBytes + b.RunStartBytes + b.BlockIDsBytes
}

// CapacityBytes is what the slices actually reserve. SlackBytes is the gap:
// append grows by doubling, so a shard built incrementally can end up holding
// nearly twice the capacity it needs. Clipping it back costs nothing — no
// layout change, no read-path change — which makes it the cheapest thing on
// this list.
func (b MemoryBreakdown) CapacityBytes() uint64 { return b.SliceBytes() + b.SlackBytes }

// AccountedBytes is what this breakdown explains. MemoryBytes minus this is
// the per-txn overhead finishMemory adds (metadata, blocks, map cells) — the
// part no columnar change touches.
func (b MemoryBreakdown) AccountedBytes() uint64 { return b.SliceBytes() + b.ShardFixedBytes }

// MemoryBreakdown walks every (snapshot, table) shard and sums its slices.
// Note the axis: shards are per snapshot, so the same row written in N
// snapshots appears N times here — that repetition is the point, it is what
// makes a long DELTA chain expensive.
func (v *View) MemoryBreakdown() MemoryBreakdown {
	var b MemoryBreakdown
	for _, byTable := range v.rows {
		for _, sh := range byTable {
			b.Shards++
			b.RowEntries += sh.rowIDs.n
			b.Runs += len(sh.blockIDs)
			b.RowIDsBytes += sh.rowIDs.bytes()
			b.OrdinalsBytes += uint64(len(sh.ordinals)) * 4
			b.ChangesBytes += uint64(len(sh.changes))
			b.RunStartBytes += uint64(len(sh.runStart)) * 4
			b.BlockIDsBytes += uint64(len(sh.blockIDs)) * 8
			// Packed columns carry no append slack; the only over-allocation
			// left in them is the +16 read pad on packedRowIDs.data.
			b.SlackBytes += uint64(cap(sh.ordinals)-len(sh.ordinals))*4 +
				uint64(cap(sh.changes)-len(sh.changes)) +
				uint64(cap(sh.runStart)-len(sh.runStart))*4 +
				uint64(cap(sh.blockIDs)-len(sh.blockIDs))*8
		}
	}
	b.ShardFixedBytes = uint64(b.Shards) * 48
	return b
}

// PackBlockSize is the frame size used by PackProfile's block estimate: the
// number of entries that share one bit width. It tracks rowPackFrame so the
// ordinal forecast is shaped like the RowID column that already ships.
const PackBlockSize = rowPackFrame

// PackProfile measures how far the shard columns compress under
// frame-of-reference bit packing. RowIDs already ship packed, so they report a
// measured pair; ordinals are still a []uint32, so they report a forecast.
//
//	Raw    — the counterfactual: a plain []uint64/[]uint32
//	Packed — what the packed RowID column actually costs (RowIDs only)
//	Var    — every value carries its own width; the theoretical floor
//	Block  — every PackBlockSize values share one width, the max in the frame;
//	         this is the shape you would actually implement
type PackProfile struct {
	Entries int
	// RowIDs: already packed.
	RawRowIDBits    uint64
	PackedRowIDBits uint64
	// Ordinals: still raw, so Var/Block are the headroom left over.
	RawOrdinalBits   uint64
	VarOrdinalBits   uint64
	BlockOrdinalBits uint64
	Frames           int // number of PackBlockSize frames
}

// PackedRowIDBytes is the real resident cost of the packed RowID column.
func (p PackProfile) PackedRowIDBytes() uint64 { return p.PackedRowIDBits / 8 }

// OrdinalFrameBytes is the block flavour for ordinals plus one uint32 base per
// frame — the minimum metadata a decoder needs to start a frame.
func (p PackProfile) OrdinalFrameBytes() uint64 {
	return p.BlockOrdinalBits/8 + uint64(p.Frames)*4
}

// PackProfile walks every shard and accumulates the bit widths above. Ordinals
// are not monotonic across runs, so they are measured as raw values.
func (v *View) PackProfile() PackProfile {
	var p PackProfile
	for _, byTable := range v.rows {
		for _, sh := range byTable {
			n := sh.rowIDs.n
			p.Entries += n
			p.RawRowIDBits += uint64(n) * 64
			p.PackedRowIDBits += sh.rowIDs.bytes() * 8
			p.RawOrdinalBits += uint64(n) * 32
			for i := 0; i < n; i++ {
				p.VarOrdinalBits += uint64(bits.Len64(uint64(sh.ordinals[i])))
			}
			for base := 0; base < n; base += PackBlockSize {
				end := min(base+PackBlockSize, n)
				var maxOrd uint64
				for i := base; i < end; i++ {
					if uint64(sh.ordinals[i]) > maxOrd {
						maxOrd = uint64(sh.ordinals[i])
					}
				}
				p.BlockOrdinalBits += uint64(end-base) * uint64(bits.Len64(maxOrd))
				p.Frames++
			}
		}
	}
	return p
}

// ResolveRow finds the row location for (snapshot, table, rowID) along the
// parent chain.
func (v *View) ResolveRow(snapshot uint64, table uint32, rowID uint64) (RowLoc, bool) {
	sm := v.snapshots[snapshot]
	if sm == nil {
		return RowLoc{}, false
	}
	for _, cur := range sm.chain {
		if loc, ok := v.Row(cur, table, rowID); ok {
			return loc, true
		}
	}
	return RowLoc{}, false
}

// LogicalRowCount returns the number of rows visible at a snapshot for a
// table after resolving overrides and tombstones along the parent chain. It
// merges the per-layer sorted incremental indexes without reading blocks.
func (v *View) LogicalRowCount(snapshot uint64, table uint32) uint64 {
	sm := v.snapshots[snapshot]
	if sm == nil {
		return 0
	}
	layers := make([]*RowKeyIter, 0, len(sm.chain))
	for _, cur := range sm.chain {
		if keys := v.RowIter(cur, table); keys != nil && keys.Len() > 0 {
			layers = append(layers, keys)
		}
	}
	m := NewMergeIter(layers...)
	var count uint64
	for {
		_, loc, ok := m.Next()
		if !ok {
			return count
		}
		if loc.ChangeType != format.ChangeDelete {
			count++
		}
	}
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
	a, err := v.newViewApply(t.Snapshot, maxDepth)
	if err != nil {
		return nil, err
	}
	if err := a.applyBlocks(t.Blocks); err != nil {
		return nil, err
	}
	if err := a.applyMetadata(t.Metadata); err != nil {
		return nil, err
	}
	rowMap, err := buildShards(t, a.snapID)
	if err != nil {
		return nil, err
	}
	a.next.rows[a.snapID] = rowMap
	a.finishMemory(len(t.Metadata), len(t.Blocks), rowMap)
	return a.next, nil
}

// viewApply is the install context of one snapshot application: the view
// being copied (old), the copy under construction (next), and the snapshot
// entry being installed (snapID/meta). The buffered Apply path and the
// streaming sink both funnel their blocks, metadata and row shards through it,
// so the copy-on-write pair never travels as loose arguments.
type viewApply struct {
	old    *View
	next   *View
	meta   *SnapshotMeta
	snapID uint64
}

// newViewApply validates the snapshot entry against v (chain, uniqueness,
// depth) and installs it into the new view copy, returning the install
// context.
func (v *View) newViewApply(se format.SnapshotIndexEntry, maxDepth uint32) (*viewApply, error) {
	nv, meta, err := v.beginApply(se, maxDepth)
	if err != nil {
		return nil, err
	}
	return &viewApply{old: v, next: nv, meta: meta, snapID: se.SnapshotID}, nil
}

// beginApply validates the snapshot entry against v (chain, uniqueness,
// depth) and installs it into the new view copy, returning the copy and its
// meta. Shared by Apply and the streaming sink.
func (v *View) beginApply(se format.SnapshotIndexEntry, maxDepth uint32) (*View, *SnapshotMeta, error) {
	nv := v.shallowCopy()
	if _, dup := v.snapshots[se.SnapshotID]; dup {
		return nil, nil, fmt.Errorf("rowpack: snapshot %d already committed", se.SnapshotID)
	}
	depth := uint32(1)
	var chain []uint64
	if se.SnapshotType == format.SnapshotDelta {
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
		chain = make([]uint64, 0, len(parent.chain)+1)
		chain = append(chain, se.SnapshotID)
		chain = append(chain, parent.chain...)
	} else if se.SnapshotType != format.SnapshotFull {
		return nil, nil, fmt.Errorf("rowpack: snapshot %d bad type %d", se.SnapshotID, se.SnapshotType)
	} else {
		chain = []uint64{se.SnapshotID}
	}
	if se.ParentSnapshotID != 0 && se.SnapshotType == format.SnapshotFull {
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
		chain:             chain,
	}
	nv.snapshots[se.SnapshotID] = meta
	return nv, meta, nil
}

// applyBlocks validates and installs the txn's block entries into next,
// accumulating stored bytes into meta.
func (a *viewApply) applyBlocks(blocks []format.BlockIndexEntry) error {
	for i := range blocks {
		be := &blocks[i]
		if be.SnapshotID != a.snapID {
			return fmt.Errorf("rowpack: block %d belongs to snapshot %d, want %d", be.BlockID, be.SnapshotID, a.snapID)
		}
		if _, dup := a.old.blocks[be.BlockID]; dup {
			return fmt.Errorf("rowpack: block %d already exists", be.BlockID)
		}
		bl := &BlockLoc{
			BlockID: be.BlockID, SnapshotID: be.SnapshotID, TableID: be.TableID,
			Kind: be.BlockKind, Compression: be.Compression, DataOffset: be.DataOffset,
			RawSize: be.RawSize, StoredSize: be.StoredSize, ItemCount: be.ItemCount,
			RawCRC32C: be.RawCRC32C,
		}
		a.next.blocks[be.BlockID] = bl
		a.meta.StoredBytes += uint64(be.StoredSize)
	}
	return nil
}

// applyMetadata builds and installs the metadata location maps for the
// snapshot being applied.
func (a *viewApply) applyMetadata(metadata []format.MetadataIndexEntry) error {
	metaMap := make(map[uint64]*MetadataLoc)
	typeMap := make(map[uint32][]uint64)
	for i := range metadata {
		me := &metadata[i]
		if me.SnapshotID != a.snapID {
			return fmt.Errorf("rowpack: metadata entry %d wrong snapshot", me.ObjectID)
		}
		if _, dup := metaMap[me.ObjectID]; dup {
			return fmt.Errorf("rowpack: metadata object %d duplicated in snapshot %d", me.ObjectID, a.snapID)
		}
		metaMap[me.ObjectID] = &MetadataLoc{
			ObjectID: me.ObjectID, Revision: me.Revision, RecordType: me.RecordType,
			BlockID: me.BlockID, ItemOrdinal: me.ItemOrdinal, Operation: me.Operation,
		}
		typeMap[me.RecordType] = append(typeMap[me.RecordType], me.ObjectID)
	}
	for k := range typeMap {
		slices.Sort(typeMap[k])
	}
	a.next.metadata[a.snapID] = metaMap
	a.next.metadataByType[a.snapID] = typeMap
	return nil
}

// finishMemory applies the per-txn memory estimate to next: rough
// per-entry overhead plus map cells and packed row-shard entries (24 B/row).
func (a *viewApply) finishMemory(nMeta, nBlocks int, rowMap map[uint32]*rowShard) {
	a.next.memoryBytes = a.old.memoryBytes
	a.next.memoryBytes += 64 + uint64(nMeta)*56 + uint64(nBlocks)*72
	for _, sh := range rowMap {
		// Columnar storage: packed RowID + ItemOrdinal (4) + ChangeType (1) per
		// row, plus the block-run directory (BlockID + runStart per run).
		a.next.memoryBytes += 48 +
			sh.rowIDs.bytes() +
			uint64(len(sh.ordinals))*4 +
			uint64(len(sh.changes))*1 +
			uint64(len(sh.blockIDs))*8 +
			uint64(len(sh.runStart))*4
	}
}

// ApplyStreaming parses the serialized IndexTxn at data and applies it in one
// fused pass: entries stream from the parser straight into the new view's
// compact shard slices, never materializing Txn.Rows (~40 B/row off the Open
// peak). Validation semantics are identical to Apply(ParseTxn(data, nil)).
func (v *View) ApplyStreaming(data []byte, crypto *ChunkCrypto, maxDepth uint32) (*View, error) {
	ap := &streamApply{old: v, maxDepth: maxDepth}
	_, err := parseStream(data, crypto, ap)
	if err != nil {
		return nil, err
	}
	if err := ap.finish(); err != nil {
		return nil, err
	}
	return ap.ap.next, nil
}

// streamApply is the TxnSink behind ApplyStreaming. Blocks and metadata are
// few (per flushed block), so they buffer; rows — the per-row bulk — append
// directly into an incremental rowShardBuilder, so no []RowKeyLoc intermediate
// and no fully materialized []RowIndexEntry page exists on the Open path. The
// first (single-table) shard preallocates exactly from the header's row count.
type streamApply struct {
	old      *View
	maxDepth uint32
	ap       *viewApply // set by SetSnapshot

	blocks   []format.BlockIndexEntry
	metadata []format.MetadataIndexEntry

	shards *rowShardBuilder
	hint   int
}

func (a *streamApply) ReserveRows(hint int) { a.hint = hint }

func (a *streamApply) SetSnapshot(e format.SnapshotIndexEntry) error {
	if a.ap != nil {
		return fmt.Errorf("rowpack: duplicate snapshot chunk")
	}
	ap, err := a.old.newViewApply(e, a.maxDepth)
	if err != nil {
		return err
	}
	a.ap = ap
	return nil
}

func (a *streamApply) AddBlock(e format.BlockIndexEntry) error {
	if a.ap == nil {
		return fmt.Errorf("rowpack: block entry before snapshot")
	}
	a.blocks = append(a.blocks, e)
	return nil
}

func (a *streamApply) AddMetadata(e format.MetadataIndexEntry) error {
	if a.ap == nil {
		return fmt.Errorf("rowpack: metadata entry before snapshot")
	}
	a.metadata = append(a.metadata, e)
	return nil
}

// AddRowBatch implements rowBatchSink: the page parser decodes a whole index
// page into columnar form and hands it over in one call, so the shard columns
// are appended and validated per page instead of per row.
func (a *streamApply) AddRowBatch(b *pageRows, snapshotID uint64) error {
	if a.ap == nil {
		return fmt.Errorf("rowpack: row entry before snapshot")
	}
	if snapshotID != a.ap.snapID {
		return fmt.Errorf("rowpack: row entry wrong snapshot")
	}
	if a.shards == nil {
		a.shards = newRowShardBuilder(a.ap.snapID, a.hint)
	}
	return a.shards.AddPageRows(b, snapshotID)
}

// AddRowEntry implements RowEntrySink: the page parser (via walkPage)
// hands each decoded entry straight to the shard builder, one at a time, so
// no per-page []RowIndexEntry is materialized. It is the fallback for sinks
// that do not implement rowBatchSink.
func (a *streamApply) AddRowEntry(e format.RowIndexEntry) error {
	if a.ap == nil {
		return fmt.Errorf("rowpack: row entry before snapshot")
	}
	if a.shards == nil {
		a.shards = newRowShardBuilder(a.ap.snapID, a.hint)
	}
	return a.shards.AddRowEntry(e)
}

// AddRows implements TxnSink. On the Open path the parser uses AddRowEntry
// directly; AddRows remains as the batch fallback (buffered / non-streaming
// sinks) and delegates to AddRowEntry.
func (a *streamApply) AddRows(batch []format.RowIndexEntry) error {
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
func (a *streamApply) finish() error {
	if a.ap == nil {
		return fmt.Errorf("rowpack: index txn has no snapshot chunk")
	}
	if err := a.ap.applyBlocks(a.blocks); err != nil {
		return err
	}
	if err := a.ap.applyMetadata(a.metadata); err != nil {
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
	a.ap.next.rows[a.ap.snapID] = rowMap
	a.ap.finishMemory(len(a.metadata), len(a.blocks), rowMap)
	return nil
}

// buildShards converts t.Rows into one sorted rowShard per table. It
// returns (nil, nil) when t has no row entries. Duplicates within a
// (snapshot, table) and entries owned by another snapshot are rejected.
func buildShards(t *Txn, snapshot uint64) (map[uint32]*rowShard, error) {
	n := len(t.Rows)
	if n == 0 {
		return nil, nil
	}
	checkOwned := func(re *format.RowIndexEntry) error {
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
		slices.SortFunc(entries, func(a, b RowKeyLoc) int { return cmp.Compare(a.RowID, b.RowID) })
	}
	n := len(entries)
	rowIDs := make([]uint64, n) // filled, then packed; a shard keeps no raw column
	sh.ordinals = make([]uint32, n)
	sh.changes = make([]uint8, n)
	sh.runStart = make([]uint32, 0, 8)
	sh.blockIDs = make([]uint64, 0, 8)
	for i := range n {
		e := entries[i]
		if i > 0 && e.RowID == entries[i-1].RowID {
			return fmt.Errorf("rowpack: duplicate row %d in snapshot", e.RowID)
		}
		rowIDs[i] = e.RowID
		sh.ordinals[i] = e.Loc.ItemOrdinal
		sh.changes[i] = uint8(e.Loc.ChangeType)
		if i == 0 || e.Loc.BlockID != entries[i-1].Loc.BlockID {
			sh.runStart = append(sh.runStart, uint32(i))
			sh.blockIDs = append(sh.blockIDs, e.Loc.BlockID)
		}
	}
	sh.runStart = append(sh.runStart, uint32(n))
	sh.rowIDs = packRowIDs(rowIDs)
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
	maps.Copy(nv.snapshots, v.snapshots)
	maps.Copy(nv.blocks, v.blocks)
	maps.Copy(nv.metadata, v.metadata)
	maps.Copy(nv.metadataByType, v.metadataByType)
	maps.Copy(nv.rows, v.rows)
	nv.memoryBytes = v.memoryBytes
	return nv
}
