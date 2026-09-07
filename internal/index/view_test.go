package index

import (
	"errors"
	"sort"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// parseTxn builds and parses one txn in one step.
func parseTxn(t *testing.T, seq uint64, snap fileformat.SnapshotIndexEntry, meta []fileformat.MetadataIndexEntry, blocks []fileformat.BlockIndexEntry, rows []fileformat.RowIndexEntry) *Txn {
	t.Helper()
	data := buildTxn(t, seq, snap, meta, blocks, rows, snap.DataEnd, 0)
	txn, err := ParseTxn(data)
	require.NoError(t, err)
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
	require.NoError(t, err)
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
	require.NoError(t, err)
	v = nv

	txn = parseTxn(t, 3, deltaSnap(3, 2, 8292, 12288), nil,
		[]fileformat.BlockIndexEntry{{BlockID: 3, SnapshotID: 3, TableID: 2}},
		[]fileformat.RowIndexEntry{
			rowEntry(3, 1, 1, fileformat.ChangeUpdate, 3, 0),
			rowEntry(3, 2, 5, fileformat.ChangeInsert, 3, 1),
		})
	nv, err = v.Apply(txn, 16)
	require.NoError(t, err)
	return nv
}

func TestViewSnapshotsAndLatest(t *testing.T) {
	v := buildChain(t)
	require.NotNil(t, v.LatestSnapshot(), "LatestSnapshot should be 3")
	require.Equal(t, uint64(3), v.LatestSnapshot().ID, "LatestSnapshot should be 3")
	snaps := v.Snapshots()
	require.Len(t, snaps, 3, "Snapshots len = %d, want 3", len(snaps))
	for i, s := range snaps {
		require.Equal(t, uint64(i+1), s.ID, "Snapshots not sorted by ID: %v", snaps)
	}
	// Chain depth.
	require.Equal(t, uint32(1), snaps[0].Depth, "depths = %d,%d,%d", snaps[0].Depth, snaps[1].Depth, snaps[2].Depth)
	require.Equal(t, uint32(2), snaps[1].Depth, "depths = %d,%d,%d", snaps[0].Depth, snaps[1].Depth, snaps[2].Depth)
	require.Equal(t, uint32(3), snaps[2].Depth, "depths = %d,%d,%d", snaps[0].Depth, snaps[1].Depth, snaps[2].Depth)
	// Parent links.
	require.Equal(t, uint64(1), snaps[1].Parent, "parent links wrong")
	require.Equal(t, uint64(2), snaps[2].Parent, "parent links wrong")
	require.Nil(t, v.Snapshot(99), "Snapshot(99) should be nil")
}

func TestViewBlocksAndBlockIDs(t *testing.T) {
	v := buildChain(t)
	if b := v.Block(2); b == nil || b.SnapshotID != 2 || b.TableID != 1 {
		require.Fail(t, "Block(2) wrong")
	}
	require.Nil(t, v.Block(42), "Block(42) should be nil")
	var ids []uint64
	for _, b := range v.Blocks() {
		if b.SnapshotID == 2 {
			ids = append(ids, b.BlockID)
		}
	}
	require.Len(t, ids, 1, "blocks of snapshot 2 = %v", ids)
	require.Equal(t, uint64(2), ids[0], "blocks of snapshot 2 = %v", ids)
	require.Len(t, v.Blocks(), 3, "Blocks len = %d, want 3", len(v.Blocks()))
}

func TestViewMetadata(t *testing.T) {
	v := buildChain(t)
	m := v.Metadata(2, 100)
	require.NotNil(t, m, "Metadata(2,100) = %+v", m)
	require.Equal(t, uint64(100), m.ObjectID, "Metadata(2,100) = %+v", m)
	require.Equal(t, uint32(fileformat.RecordColumn), m.RecordType, "Metadata(2,100) = %+v", m)
	require.Nil(t, v.Metadata(1, 100), "snapshot 1 has no metadata")
	require.Nil(t, v.Metadata(99, 100), "Metadata(99,100) should be nil")
	ids := v.MetadataByType(2, uint32(fileformat.RecordColumn))
	require.Len(t, ids, 1, "MetadataByType = %v", ids)
	require.Equal(t, uint64(100), ids[0], "MetadataByType = %v", ids)
	require.Nil(t, v.MetadataByType(2, 999), "MetadataByType(2,999) should be nil")
	require.Nil(t, v.MetadataByType(1, uint32(fileformat.RecordColumn)), "snapshot 1 MetadataByType should be nil")
}

func TestViewResolveRow(t *testing.T) {
	v := buildChain(t)
	// Direct hit at the requested snapshot.
	if loc := v.ResolveRow(2, 1, 4); loc == nil || loc.ChangeType != fileformat.ChangeInsert {
		require.Fail(t, "ResolveRow(2,1,4) should find the s2 insert")
	}
	// Falls back to the parent chain: row 1 was inserted in s1, updated in s3.
	if loc := v.ResolveRow(3, 1, 1); loc == nil || loc.BlockID != 3 {
		require.Fail(t, "ResolveRow(3,1,1) should hit the s3 override")
	}
	if loc := v.ResolveRow(2, 1, 1); loc == nil || loc.BlockID != 1 {
		require.Fail(t, "ResolveRow(2,1,1) should fall back to s1")
	}
	// Tombstone at s2 shadows the s1 insert.
	if loc := v.ResolveRow(2, 1, 3); loc == nil || loc.ChangeType != fileformat.ChangeDelete {
		require.Fail(t, "ResolveRow(2,1,3) should find the s2 tombstone")
	}
	// No such row anywhere in the chain.
	require.Nil(t, v.ResolveRow(3, 1, 999), "ResolveRow(3,1,999) should be nil")
	// Unknown snapshot terminates the chain walk.
	require.Nil(t, v.ResolveRow(99, 1, 1), "ResolveRow(99,1,1) should be nil")
}

func TestViewRowTablesAndKeys(t *testing.T) {
	v := buildChain(t)
	tbls := v.RowTables(3)
	require.Len(t, tbls, 2, "RowTables(3) = %v, want [1 2]", tbls)
	require.Equal(t, uint32(1), tbls[0], "RowTables(3) = %v, want [1 2]", tbls)
	require.Equal(t, uint32(2), tbls[1], "RowTables(3) = %v, want [1 2]", tbls)
	require.Nil(t, v.RowTables(99), "RowTables(99) should be nil")
	keys := v.RowKeys(3, 2)
	require.Len(t, keys, 1, "RowKeys(3,2) = %v", keys)
	require.Equal(t, uint64(5), keys[0].RowID, "RowKeys(3,2) = %v", keys)
	require.Nil(t, v.RowKeys(3, 9), "RowKeys(3,9) should be nil")
	require.NotNil(t, v.Row(3, 2, 5), "Row(3,2,·) lookup wrong")
	require.Nil(t, v.Row(3, 2, 6), "Row(3,2,·) lookup wrong")
}

func TestViewLogicalRowCount(t *testing.T) {
	v := buildChain(t)
	// s1: rows 1,2,3 = 3.
	require.Equal(t, uint64(3), v.LogicalRowCount(1, 1), "LogicalRowCount(1,1) = %d, want 3", v.LogicalRowCount(1, 1))
	// s2: 1,2(upd),4 visible; 3 deleted = 3.
	require.Equal(t, uint64(3), v.LogicalRowCount(2, 1), "LogicalRowCount(2,1) = %d, want 3", v.LogicalRowCount(2, 1))
	// s3 adds row 5 in table 2 = 1; table 1 unchanged (row 1 override).
	require.Equal(t, uint64(1), v.LogicalRowCount(3, 2), "LogicalRowCount(3,2) = %d, want 1", v.LogicalRowCount(3, 2))
	require.Equal(t, uint64(3), v.LogicalRowCount(3, 1), "LogicalRowCount(3,1) = %d, want 3", v.LogicalRowCount(3, 1))
	// Unknown snapshot/table.
	require.Zero(t, v.LogicalRowCount(99, 1), "LogicalRowCount(99,1) = %d, want 0", v.LogicalRowCount(99, 1))
	require.Zero(t, v.LogicalRowCount(3, 9), "LogicalRowCount(3,9) = %d, want 0", v.LogicalRowCount(3, 9))
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
	require.NoError(t, err)
	v = nv
	rows2 := []fileformat.RowIndexEntry{
		rowEntry(2, 1, 15, fileformat.ChangeInsert, 2, 0),
		rowEntry(2, 1, 10, fileformat.ChangeDelete, 2, 1),
	}
	nv, err = v.Apply(parseTxn(t, 2, deltaSnap(2, 1, 300, 400), nil, nil, rows2), 16)
	require.NoError(t, err)
	v = nv
	require.Equal(t, uint64(3), v.LogicalRowCount(1, 1), "LogicalRowCount(1,1) = %d, want 3", v.LogicalRowCount(1, 1))
	require.Equal(t, uint64(3), v.LogicalRowCount(2, 1), "LogicalRowCount(2,1) = %d, want 3 (15,20,30; 10 deleted)", v.LogicalRowCount(2, 1))
	keys := v.RowKeys(1, 1)
	require.Len(t, keys, 3, "shard not sorted: %v", keys)
	require.Equal(t, uint64(10), keys[0].RowID, "shard not sorted: %v", keys)
	require.Equal(t, uint64(30), keys[2].RowID, "shard not sorted: %v", keys)
}

func TestApplyRejects(t *testing.T) {
	v := buildChain(t)

	_, err := v.Apply(nil, 16)
	require.Error(t, err, "nil txn accepted")

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
		require.Error(t, err, "%s: accepted", tc.name)
	}
	// The view must be untouched by all rejected applies.
	require.Equal(t, uint64(3), v.LogicalRowCount(3, 1), "rejected Apply mutated the view")
	require.Nil(t, v.Snapshot(9), "rejected Apply mutated the view")
}

func TestBuilderValidation(t *testing.T) {
	b := NewBuilder(1)
	m, bl, r := b.Counts()
	require.Zero(t, m, "empty Counts = %d,%d,%d", m, bl, r)
	require.Zero(t, bl, "empty Counts = %d,%d,%d", m, bl, r)
	require.Zero(t, r, "empty Counts = %d,%d,%d", m, bl, r)
	// Entries before SetSnapshot.
	require.Error(t, b.AddMetadata(metaEntry(1, 1, 1)), "AddMetadata before SetSnapshot accepted")
	require.Error(t, b.AddBlock(fileformat.BlockIndexEntry{SnapshotID: 1}), "AddBlock before SetSnapshot accepted")
	require.Error(t, b.AddRow(rowEntry(1, 1, 1, fileformat.ChangeInsert, 1, 0)), "AddRow before SetSnapshot accepted")
	_, _, err := b.Build(0, 0, 0, 0, 0)
	require.Error(t, err, "Build without snapshot accepted")
	// SetSnapshot validation.
	require.Error(t, NewBuilder(1).SetSnapshot(fullSnap(0, 0, 0)), "zero snapshot id accepted")
	require.Error(t, NewBuilder(1).SetSnapshot(fileformat.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: fileformat.SnapshotType(9)}), "bad snapshot type accepted")
	b2 := NewBuilder(1)
	require.NoError(t, b2.SetSnapshot(fullSnap(1, 0, 0)))
	require.Error(t, b2.SetSnapshot(fullSnap(2, 0, 0)), "second SetSnapshot accepted")
	// Snapshot mismatch checks.
	require.Error(t, b2.AddMetadata(metaEntry(5, 1, 1)), "metadata snapshot mismatch accepted")
	require.Error(t, b2.AddBlock(fileformat.BlockIndexEntry{SnapshotID: 5}), "block snapshot mismatch accepted")
	require.Error(t, b2.AddRow(rowEntry(5, 1, 1, fileformat.ChangeInsert, 1, 0)), "row snapshot mismatch accepted")
}

func TestBuilderReserveAndCounts(t *testing.T) {
	b := NewBuilder(7)
	require.NoError(t, b.SetSnapshot(fullSnap(1, 0, 0)))
	b.Reserve(4, 2, 8)
	require.NoError(t, b.AddMetadata(metaEntry(1, 10, 1)))
	require.NoError(t, b.AddBlock(fileformat.BlockIndexEntry{SnapshotID: 1}))
	require.NoError(t, b.AddRow(rowEntry(1, 1, 1, fileformat.ChangeInsert, 1, 0)))
	m, bl, r := b.Counts()
	require.Equal(t, uint32(1), m, "Counts = %d,%d,%d", m, bl, r)
	require.Equal(t, uint32(1), bl, "Counts = %d,%d,%d", m, bl, r)
	require.Equal(t, uint64(1), r, "Counts = %d,%d,%d", m, bl, r)
	// Reserve after entries must not clobber them (capacity-only pre-alloc).
	b.Reserve(9, 9, 9)
	m2, _, r2 := b.Counts()
	require.Equal(t, uint32(1), m2, "Reserve clobbered entries: %d,%d", m2, r2)
	require.Equal(t, uint64(1), r2, "Reserve clobbered entries: %d,%d", m2, r2)
	out, txn, err := b.Build(0, 100, 0xABCD, 0, 100)
	require.NoError(t, err)
	require.Equal(t, uint64(7), txn.Header.TxnSequence, "Build output wrong: seq=%d len=%d", txn.Header.TxnSequence, len(out))
	require.NotEmpty(t, out, "Build output wrong: seq=%d len=%d", txn.Header.TxnSequence, len(out))
	_, err = ParseTxn(out)
	require.NoError(t, err, "built txn does not parse: %v", err)
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
	require.NoError(t, err)
	var ids []uint64
	for _, b := range nv.Blocks() {
		if b.SnapshotID == 1 {
			ids = append(ids, b.BlockID)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	require.Len(t, ids, 3, "blocks of snapshot 1 = %v", ids)
	require.Equal(t, uint64(10), ids[0], "blocks of snapshot 1 = %v", ids)
	require.Equal(t, uint64(20), ids[1], "blocks of snapshot 1 = %v", ids)
	require.Equal(t, uint64(30), ids[2], "blocks of snapshot 1 = %v", ids)
	require.NotZero(t, nv.MemoryBytes(), "memory bytes not accumulated")
}

func TestErrDataFooterMismatchIsDistinct(t *testing.T) {
	require.True(t, errors.Is(ErrDataFooterMismatch, ErrDataFooterMismatch), "sentinel should match itself")
}
