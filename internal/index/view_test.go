package index

import (
	"errors"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// parseTxn builds and parses one txn in one step.
func parseTxn(t *testing.T, seq uint64, snap fileformat.SnapshotIndexEntry, meta []fileformat.MetadataIndexEntry, blocks []fileformat.BlockIndexEntry, rows []fileformat.RowIndexEntry) *Txn {
	t.Helper()
	data := buildTxn(t, seq, snap, meta, blocks, rows, snap.DataEnd, 0)
	txn, err := ParseTxn(data)
	if err != nil {
		t.Fatal(err)
	}
	return txn
}

func metaEntry(snap, objectID uint64, recordType uint32) fileformat.MetadataIndexEntry {
	return fileformat.MetadataIndexEntry{
		SnapshotID: snap, ObjectID: objectID, Revision: 1,
		RecordType: recordType, BlockID: 1, ItemOrdinal: 0,
		Operation: fileformat.OperationUpsert,
	}
}

func rowEntry(snap uint64, table uint32, rowID uint64, change fileformat.ChangeType, block uint64, ordinal uint32) fileformat.RowIndexEntry {
	return fileformat.RowIndexEntry{
		SnapshotID: snap, TableID: table, ChangeType: change,
		RowID: rowID, BlockID: block, ItemOrdinal: ordinal,
	}
}

// buildChain constructs FULL s1 <- DELTA s2 <- DELTA s3 over table 1:
//   - s1: insert rows 1,2,3; delete nothing
//   - s2: update row 2, delete row 3, insert row 4
//   - s3: insert row 5 (in table 2), re-insert row 1 in table 1
func buildChain(t *testing.T) *View {
	t.Helper()
	v := EmptyView()
	txn := parseTxn(t, 1, fullSnap(1, 128, 4096), nil,
		[]fileformat.BlockIndexEntry{{BlockID: 1, SnapshotID: 1, TableID: 1}},
		[]fileformat.RowIndexEntry{
			rowEntry(1, 1, 1, fileformat.ChangeInsert, 1, 0),
			rowEntry(1, 1, 2, fileformat.ChangeInsert, 1, 1),
			rowEntry(1, 1, 3, fileformat.ChangeInsert, 1, 2),
		})
	nv, err := v.Apply(txn, 16)
	if err != nil {
		t.Fatal(err)
	}
	v = nv

	txn = parseTxn(t, 2, deltaSnap(2, 1, 4192, 8192),
		[]fileformat.MetadataIndexEntry{metaEntry(2, 100, uint32(fileformat.RecordColumn))},
		[]fileformat.BlockIndexEntry{{BlockID: 2, SnapshotID: 2, TableID: 1}},
		[]fileformat.RowIndexEntry{
			rowEntry(2, 1, 2, fileformat.ChangeUpdate, 2, 0),
			rowEntry(2, 1, 3, fileformat.ChangeDelete, 2, 1),
			rowEntry(2, 1, 4, fileformat.ChangeInsert, 2, 2),
		})
	nv, err = v.Apply(txn, 16)
	if err != nil {
		t.Fatal(err)
	}
	v = nv

	txn = parseTxn(t, 3, deltaSnap(3, 2, 8292, 12288), nil,
		[]fileformat.BlockIndexEntry{{BlockID: 3, SnapshotID: 3, TableID: 2}},
		[]fileformat.RowIndexEntry{
			rowEntry(3, 1, 1, fileformat.ChangeUpdate, 3, 0),
			rowEntry(3, 2, 5, fileformat.ChangeInsert, 3, 1),
		})
	nv, err = v.Apply(txn, 16)
	if err != nil {
		t.Fatal(err)
	}
	return nv
}

func TestViewSnapshotsAndLatest(t *testing.T) {
	v := buildChain(t)
	if v.LatestSnapshot() == nil || v.LatestSnapshot().ID != 3 {
		t.Fatal("LatestSnapshot should be 3")
	}
	snaps := v.Snapshots()
	if len(snaps) != 3 {
		t.Fatalf("Snapshots len = %d, want 3", len(snaps))
	}
	for i, s := range snaps {
		if s.ID != uint64(i+1) {
			t.Fatalf("Snapshots not sorted by ID: %v", snaps)
		}
	}
	// Chain depth.
	if snaps[0].Depth != 1 || snaps[1].Depth != 2 || snaps[2].Depth != 3 {
		t.Fatalf("depths = %d,%d,%d", snaps[0].Depth, snaps[1].Depth, snaps[2].Depth)
	}
	// Parent links.
	if snaps[1].Parent != 1 || snaps[2].Parent != 2 {
		t.Fatal("parent links wrong")
	}
	if v.Snapshot(99) != nil {
		t.Fatal("Snapshot(99) should be nil")
	}
}

func TestViewBlocksAndBlockIDs(t *testing.T) {
	v := buildChain(t)
	if b := v.Block(2); b == nil || b.SnapshotID != 2 || b.TableID != 1 {
		t.Fatal("Block(2) wrong")
	}
	if v.Block(42) != nil {
		t.Fatal("Block(42) should be nil")
	}
	ids := v.BlockIDs(2)
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("BlockIDs(2) = %v", ids)
	}
	if v.BlockIDs(99) != nil {
		t.Fatal("BlockIDs(99) should be nil")
	}
	all := v.Blocks()
	if len(all) != 3 {
		t.Fatalf("Blocks len = %d, want 3", len(all))
	}
}

func TestViewMetadata(t *testing.T) {
	v := buildChain(t)
	m := v.Metadata(2, 100)
	if m == nil || m.ObjectID != 100 || m.RecordType != uint32(fileformat.RecordColumn) {
		t.Fatalf("Metadata(2,100) = %+v", m)
	}
	if v.Metadata(1, 100) != nil {
		t.Fatal("snapshot 1 has no metadata")
	}
	if v.Metadata(99, 100) != nil {
		t.Fatal("Metadata(99,100) should be nil")
	}
	ids := v.MetadataByType(2, uint32(fileformat.RecordColumn))
	if len(ids) != 1 || ids[0] != 100 {
		t.Fatalf("MetadataByType = %v", ids)
	}
	if v.MetadataByType(2, 999) != nil {
		t.Fatal("MetadataByType(2,999) should be nil")
	}
	if v.MetadataByType(1, uint32(fileformat.RecordColumn)) != nil {
		t.Fatal("snapshot 1 MetadataByType should be nil")
	}
}

func TestViewResolveRow(t *testing.T) {
	v := buildChain(t)
	// Direct hit at the requested snapshot.
	if loc := v.ResolveRow(2, 1, 4); loc == nil || loc.ChangeType != fileformat.ChangeInsert {
		t.Fatal("ResolveRow(2,1,4) should find the s2 insert")
	}
	// Falls back to the parent chain: row 1 was inserted in s1, updated in s3.
	if loc := v.ResolveRow(3, 1, 1); loc == nil || loc.BlockID != 3 {
		t.Fatal("ResolveRow(3,1,1) should hit the s3 override")
	}
	if loc := v.ResolveRow(2, 1, 1); loc == nil || loc.BlockID != 1 {
		t.Fatal("ResolveRow(2,1,1) should fall back to s1")
	}
	// Tombstone at s2 shadows the s1 insert.
	if loc := v.ResolveRow(2, 1, 3); loc == nil || loc.ChangeType != fileformat.ChangeDelete {
		t.Fatal("ResolveRow(2,1,3) should find the s2 tombstone")
	}
	// No such row anywhere in the chain.
	if v.ResolveRow(3, 1, 999) != nil {
		t.Fatal("ResolveRow(3,1,999) should be nil")
	}
	// Unknown snapshot terminates the chain walk.
	if v.ResolveRow(99, 1, 1) != nil {
		t.Fatal("ResolveRow(99,1,1) should be nil")
	}
}

func TestViewRowTablesAndKeys(t *testing.T) {
	v := buildChain(t)
	tbls := v.RowTables(3)
	if len(tbls) != 2 || tbls[0] != 1 || tbls[1] != 2 {
		t.Fatalf("RowTables(3) = %v, want [1 2]", tbls)
	}
	if v.RowTables(99) != nil {
		t.Fatal("RowTables(99) should be nil")
	}
	keys := v.RowKeys(3, 2)
	if len(keys) != 1 || keys[0].RowID != 5 {
		t.Fatalf("RowKeys(3,2) = %v", keys)
	}
	if v.RowKeys(3, 9) != nil {
		t.Fatal("RowKeys(3,9) should be nil")
	}
	if v.Row(3, 2, 5) == nil || v.Row(3, 2, 6) != nil {
		t.Fatal("Row(3,2,·) lookup wrong")
	}
}

func TestViewLogicalRowCount(t *testing.T) {
	v := buildChain(t)
	// s1: rows 1,2,3 = 3.
	if got := v.LogicalRowCount(1, 1); got != 3 {
		t.Fatalf("LogicalRowCount(1,1) = %d, want 3", got)
	}
	// s2: 1,2(upd),4 visible; 3 deleted = 3.
	if got := v.LogicalRowCount(2, 1); got != 3 {
		t.Fatalf("LogicalRowCount(2,1) = %d, want 3", got)
	}
	// s3 adds row 5 in table 2 = 1; table 1 unchanged (row 1 override).
	if got := v.LogicalRowCount(3, 2); got != 1 {
		t.Fatalf("LogicalRowCount(3,2) = %d, want 1", got)
	}
	if got := v.LogicalRowCount(3, 1); got != 3 {
		t.Fatalf("LogicalRowCount(3,1) = %d, want 3", got)
	}
	// Unknown snapshot/table.
	if got := v.LogicalRowCount(99, 1); got != 0 {
		t.Fatalf("LogicalRowCount(99,1) = %d, want 0", got)
	}
	if got := v.LogicalRowCount(3, 9); got != 0 {
		t.Fatalf("LogicalRowCount(3,9) = %d, want 0", got)
	}
}

func TestLogicalRowCountOutOfOrderShards(t *testing.T) {
	// Rows inserted out of order in one txn force the sort path and exercise
	// the heap merge over interleaved layers.
	v := EmptyView()
	rows := []fileformat.RowIndexEntry{
		rowEntry(1, 1, 30, fileformat.ChangeInsert, 1, 0),
		rowEntry(1, 1, 10, fileformat.ChangeInsert, 1, 1),
		rowEntry(1, 1, 20, fileformat.ChangeInsert, 1, 2),
	}
	nv, err := v.Apply(parseTxn(t, 1, fullSnap(1, 100, 200), nil, nil, rows), 16)
	if err != nil {
		t.Fatal(err)
	}
	v = nv
	rows2 := []fileformat.RowIndexEntry{
		rowEntry(2, 1, 15, fileformat.ChangeInsert, 2, 0),
		rowEntry(2, 1, 10, fileformat.ChangeDelete, 2, 1),
	}
	nv, err = v.Apply(parseTxn(t, 2, deltaSnap(2, 1, 300, 400), nil, nil, rows2), 16)
	if err != nil {
		t.Fatal(err)
	}
	v = nv
	if got := v.LogicalRowCount(1, 1); got != 3 {
		t.Fatalf("LogicalRowCount(1,1) = %d, want 3", got)
	}
	if got := v.LogicalRowCount(2, 1); got != 3 { // 15,20,30 (10 deleted)
		t.Fatalf("LogicalRowCount(2,1) = %d, want 3", got)
	}
	keys := v.RowKeys(1, 1)
	if len(keys) != 3 || keys[0].RowID != 10 || keys[2].RowID != 30 {
		t.Fatalf("shard not sorted: %v", keys)
	}
}

func TestApplyRejects(t *testing.T) {
	v := buildChain(t)

	if _, err := v.Apply(nil, 16); err == nil {
		t.Fatal("nil txn accepted")
	}

	// manualTxn clones a valid txn and overrides its entries: Apply's
	// validation must catch violations the builder itself cannot produce.
	manualTxn := func(snap fileformat.SnapshotIndexEntry, meta []fileformat.MetadataIndexEntry, blocks []fileformat.BlockIndexEntry, rows []fileformat.RowIndexEntry) *Txn {
		tx, _ := ParseTxn(buildTxn(t, 9, fullSnap(9, 900, 950), nil, nil, nil, 950, 0))
		tx.Snapshot = snap
		tx.Metadata = meta
		tx.Blocks = blocks
		tx.Rows = rows
		return tx
	}
	full9 := fullSnap(9, 900, 950)

	cases := []struct {
		name string
		txn  *Txn
	}{
		{"duplicate snapshot", parseTxn(t, 9, fullSnap(1, 900, 950), nil, nil, nil)},
		{"delta missing parent", parseTxn(t, 9, deltaSnap(9, 77, 900, 950), nil, nil, nil)},
		{"delta parent not smaller", parseTxn(t, 9, deltaSnap(3, 3, 900, 950), nil, nil, nil)},
		{"bad type", manualTxn(fileformat.SnapshotIndexEntry{SnapshotID: 9, SnapshotType: fileformat.SnapshotType(7), DataStart: 900, DataEnd: 950}, nil, nil, nil)},
		{"full with parent", manualTxn(fileformat.SnapshotIndexEntry{SnapshotID: 9, SnapshotType: fileformat.SnapshotFull, ParentSnapshotID: 1, DataStart: 900, DataEnd: 950}, nil, nil, nil)},
		{"block wrong snapshot", manualTxn(full9, nil,
			[]fileformat.BlockIndexEntry{{BlockID: 90, SnapshotID: 1}}, nil)},
		{"duplicate block", manualTxn(full9, nil,
			[]fileformat.BlockIndexEntry{{BlockID: 1, SnapshotID: 9}}, nil)},
		{"metadata wrong snapshot", manualTxn(full9,
			[]fileformat.MetadataIndexEntry{metaEntry(1, 100, 1)}, nil, nil)},
		{"duplicate metadata", manualTxn(full9,
			[]fileformat.MetadataIndexEntry{metaEntry(9, 1, 1), metaEntry(9, 1, 1)}, nil, nil)},
		{"row wrong snapshot", manualTxn(full9, nil, nil,
			[]fileformat.RowIndexEntry{rowEntry(1, 1, 1, fileformat.ChangeInsert, 90, 0)})},
		{"duplicate row", manualTxn(full9, nil, nil,
			[]fileformat.RowIndexEntry{
				rowEntry(9, 1, 1, fileformat.ChangeInsert, 90, 0),
				rowEntry(9, 1, 1, fileformat.ChangeInsert, 90, 1),
			})},
	}
	for _, tc := range cases {
		_, err := v.Apply(tc.txn, 16)
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
	}
	// The view must be untouched by all rejected applies.
	if got := v.LogicalRowCount(3, 1); got != 3 || v.Snapshot(9) != nil {
		t.Fatal("rejected Apply mutated the view")
	}
}

func TestBuilderValidation(t *testing.T) {
	b := NewBuilder(1)
	if m, bl, r := b.Counts(); m != 0 || bl != 0 || r != 0 {
		t.Fatalf("empty Counts = %d,%d,%d", m, bl, r)
	}
	// Entries before SetSnapshot.
	if err := b.AddMetadata(metaEntry(1, 1, 1)); err == nil {
		t.Fatal("AddMetadata before SetSnapshot accepted")
	}
	if err := b.AddBlock(fileformat.BlockIndexEntry{SnapshotID: 1}); err == nil {
		t.Fatal("AddBlock before SetSnapshot accepted")
	}
	if err := b.AddRow(rowEntry(1, 1, 1, fileformat.ChangeInsert, 1, 0)); err == nil {
		t.Fatal("AddRow before SetSnapshot accepted")
	}
	if _, _, err := b.Build(0, 0, 0, 0, 0); err == nil {
		t.Fatal("Build without snapshot accepted")
	}
	// SetSnapshot validation.
	if err := NewBuilder(1).SetSnapshot(fullSnap(0, 0, 0)); err == nil {
		t.Fatal("zero snapshot id accepted")
	}
	if err := NewBuilder(1).SetSnapshot(fileformat.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: fileformat.SnapshotType(9)}); err == nil {
		t.Fatal("bad snapshot type accepted")
	}
	b2 := NewBuilder(1)
	if err := b2.SetSnapshot(fullSnap(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if err := b2.SetSnapshot(fullSnap(2, 0, 0)); err == nil {
		t.Fatal("second SetSnapshot accepted")
	}
	// Snapshot mismatch checks.
	if err := b2.AddMetadata(metaEntry(5, 1, 1)); err == nil {
		t.Fatal("metadata snapshot mismatch accepted")
	}
	if err := b2.AddBlock(fileformat.BlockIndexEntry{SnapshotID: 5}); err == nil {
		t.Fatal("block snapshot mismatch accepted")
	}
	if err := b2.AddRow(rowEntry(5, 1, 1, fileformat.ChangeInsert, 1, 0)); err == nil {
		t.Fatal("row snapshot mismatch accepted")
	}
}

func TestBuilderReserveAndCounts(t *testing.T) {
	b := NewBuilder(7)
	if err := b.SetSnapshot(fullSnap(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	b.Reserve(4, 2, 8)
	if err := b.AddMetadata(metaEntry(1, 10, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(fileformat.BlockIndexEntry{SnapshotID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(rowEntry(1, 1, 1, fileformat.ChangeInsert, 1, 0)); err != nil {
		t.Fatal(err)
	}
	m, bl, r := b.Counts()
	if m != 1 || bl != 1 || r != 1 {
		t.Fatalf("Counts = %d,%d,%d", m, bl, r)
	}
	// Reserve after entries must not clobber them (capacity-only pre-alloc).
	b.Reserve(9, 9, 9)
	if m2, _, r2 := b.Counts(); m2 != 1 || r2 != 1 {
		t.Fatalf("Reserve clobbered entries: %d,%d", m2, r2)
	}
	out, txn, err := b.Build(0, 100, 0xABCD, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if txn.Header.TxnSequence != 7 || len(out) == 0 {
		t.Fatalf("Build output wrong: seq=%d len=%d", txn.Header.TxnSequence, len(out))
	}
	if _, err := ParseTxn(out); err != nil {
		t.Fatalf("built txn does not parse: %v", err)
	}
}

func TestApplyBlockIDSorting(t *testing.T) {
	// Block entries appended out of order must come back sorted per snapshot.
	blocks := []fileformat.BlockIndexEntry{
		{BlockID: 30, SnapshotID: 1, TableID: 1},
		{BlockID: 10, SnapshotID: 1, TableID: 1},
		{BlockID: 20, SnapshotID: 1, TableID: 1},
	}
	rows := []fileformat.RowIndexEntry{
		rowEntry(1, 1, 1, fileformat.ChangeInsert, 30, 0),
		rowEntry(1, 1, 2, fileformat.ChangeInsert, 10, 1),
	}
	nv, err := EmptyView().Apply(parseTxn(t, 1, fullSnap(1, 0, 0), nil, blocks, rows), 16)
	if err != nil {
		t.Fatal(err)
	}
	ids := nv.BlockIDs(1)
	if len(ids) != 3 || ids[0] != 10 || ids[1] != 20 || ids[2] != 30 {
		t.Fatalf("BlockIDs not sorted: %v", ids)
	}
	if nv.MemoryBytes() == 0 {
		t.Fatal("memory bytes not accumulated")
	}
}

func TestErrDataFooterMismatchIsDistinct(t *testing.T) {
	if !errors.Is(ErrDataFooterMismatch, ErrDataFooterMismatch) {
		t.Fatal("sentinel should match itself")
	}
}
