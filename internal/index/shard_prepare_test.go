package index

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// rowShard.prepare 的「乱序条目」分支与 AddPageRows 的排序/重复守卫此前从未被
// 执行：写路径产出的 Row Index 页恒按 (TableID, RowID) 升序，所以 buildShards 收到
// 的条目总是已排序。但 prepare 是 shard 列式化的唯一入口（recovery 重建、外部
// 构造的 Txn 都走它），排序与「排序后行定位仍对齐」必须由直接单测钉住。

func TestPrepareSortsUnsortedEntries(t *testing.T) {
	// 降序写入 + 块交替：排序后 BlockID 序列必然是 3,2,3,2,3 —— 每个块切换都要
	// 开一个新 run，且 ordinals 必须跟着自己的 RowID 走。
	entries := []RowKeyLoc{
		{RowID: 90, Loc: RowLoc{BlockID: 3, ItemOrdinal: 7, ChangeType: format.ChangeUpdate}},
		{RowID: 30, Loc: RowLoc{BlockID: 2, ItemOrdinal: 1, ChangeType: format.ChangeInsert}},
		{RowID: 70, Loc: RowLoc{BlockID: 3, ItemOrdinal: 5, ChangeType: format.ChangeInsert}},
		{RowID: 10, Loc: RowLoc{BlockID: 2, ItemOrdinal: 0, ChangeType: format.ChangeInsert}},
		{RowID: 50, Loc: RowLoc{BlockID: 2, ItemOrdinal: 3, ChangeType: format.ChangeDelete}},
	}
	sh := &rowShard{}
	err := sh.prepare(entries)
	require.NoError(t, err, "prepare")
	wantIDs := []uint64{10, 30, 50, 70, 90}
	wantOrdinals := []uint32{0, 1, 3, 5, 7}
	wantChanges := []uint8{uint8(format.ChangeInsert), uint8(format.ChangeInsert), uint8(format.ChangeDelete), uint8(format.ChangeInsert), uint8(format.ChangeUpdate)}
	if sh.rowIDs.n != len(wantIDs) {
		t.Fatalf("rowIDs len %d, want %d", sh.rowIDs.n, len(wantIDs))
	}
	for i := range wantIDs {
		if sh.rowIDAt(i) != wantIDs[i] || sh.ordinals[i] != wantOrdinals[i] || sh.changes[i] != wantChanges[i] {
			t.Fatalf("row %d = (%d, ord %d, ch %d), want (%d, %d, %d)", i,
				sh.rowIDAt(i), sh.ordinals[i], sh.changes[i], wantIDs[i], wantOrdinals[i], wantChanges[i])
		}
		if loc, ok := sh.lookup(wantIDs[i]); !ok || loc.ItemOrdinal != wantOrdinals[i] || loc.BlockID == 0 {
			t.Fatalf("lookup(%d) = %+v ok=%v", wantIDs[i], loc, ok)
		}
	}
	// 排序后的块序列 2,2,2,3,3 → 两个 run；runStart 末位是哨兵 n。
	wantRuns := []uint32{0, 3, 5}
	wantBlocks := []uint64{2, 3}
	if len(sh.runStart) != len(wantRuns) || len(sh.blockIDs) != len(wantBlocks) {
		t.Fatalf("runs %v/%v, want %v/%v", sh.runStart, sh.blockIDs, wantRuns, wantBlocks)
	}
	for i := range wantRuns {
		require.Equal(t, wantRuns[i], sh.runStart[i], "runStart %v, want %v", sh.runStart, wantRuns)
	}
	for i := range wantBlocks {
		require.Equal(t, wantBlocks[i], sh.blockIDs[i], "blockIDs %v, want %v", sh.blockIDs, wantBlocks)
	}
	// rowLocAt 必须按 run 归属给出该行的 BlockID。
	for i := range wantIDs {
		wantBlock := uint64(2)
		if i >= 3 {
			wantBlock = 3
		}
		if loc := sh.rowLocAt(i); loc.BlockID != wantBlock || loc.ItemOrdinal != wantOrdinals[i] {
			t.Fatalf("rowLocAt(%d) = %+v, want block %d ordinal %d", i, loc, wantBlock, wantOrdinals[i])
		}
	}
	// Seek 定位到首个 >= target 的下标。
	it := newRowShardIter(sh)
	it.Seek(51)
	if it.Done() || it.RowID() != 70 {
		t.Fatalf("Seek(51) = %d done=%v, want 70", it.RowID(), it.Done())
	}
}

func TestPrepareRejectsDuplicates(t *testing.T) {
	// 乱序里的重复必须在排序后被拒（v1 禁止同快照重复 RowKey）。
	sh := &rowShard{}
	err := sh.prepare([]RowKeyLoc{
		{RowID: 5, Loc: RowLoc{BlockID: 1}},
		{RowID: 9, Loc: RowLoc{BlockID: 1}},
		{RowID: 5, Loc: RowLoc{BlockID: 2}},
	})
	require.Error(t, err, "duplicate RowID must be rejected")
	if want := "duplicate row 5"; !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want mention of %q", err, want)
	}
	// 已排序输入的重复同样被拒。
	if err := sh.prepare([]RowKeyLoc{
		{RowID: 1}, {RowID: 1},
	}); err == nil {
		t.Fatal("sorted duplicate RowID must be rejected")
	}
}

func TestPrepareEmptyShard(t *testing.T) {
	sh := &rowShard{}
	err := sh.prepare(nil)
	require.NoError(t, err, "prepare(nil)")
	if sh.len() != 0 || len(sh.runStart) != 1 || sh.runStart[0] != 0 {
		t.Fatalf("empty shard = %d rows, runStart %v, want 0 rows, [0]", sh.len(), sh.runStart)
	}
	if _, ok := sh.lookup(1); ok {
		t.Fatal("lookup on empty shard must miss")
	}
}

func TestShardBuilderPageRowGuards(t *testing.T) {
	newPage := func(ids ...uint64) *pageRows {
		return &pageRows{
			rowIDs: ids, ordinals: make([]uint32, len(ids)), changes: make([]uint8, len(ids)),
			tableRunStart: []uint32{0, uint32(len(ids))}, tableIDs: []uint32{1},
			blockRunStart: []uint32{0, uint32(len(ids))}, blockIDs: []uint64{7},
		}
	}
	cases := []struct {
		name  string
		snap  uint64
		page  *pageRows
		match string
	}{
		{"wrong-snapshot", 99, newPage(1, 2), "wrong snapshot"},
		{"descending-inside", 5, newPage(4, 3), "not ascending"},
		{"duplicate-inside", 5, newPage(6, 6), "duplicate row"},
	}
	for _, tc := range cases {
		b := newRowShardBuilder(5, 4)
		if err := b.AddPageRows(tc.page, tc.snap); err == nil {
			t.Fatalf("%s: want error", tc.name)
		} else {
			require.Contains(t, err.Error(), tc.match, "%s: err = %v, want mention of %q", tc.name, err, tc.match)
		}
	}

	// 跨页边界：上一页末 id 与下一页首 id 的关系。
	b := newRowShardBuilder(5, 4)
	err := b.AddPageRows(newPage(5, 10, 20), 5)
	require.NoError(t, err, "first page")
	err = b.AddPageRows(newPage(21), 5)
	require.NoError(t, err, "in-order continuation must pass")
	if err := b.AddPageRows(newPage(5), 5); err == nil || !strings.Contains(err.Error(), "not ascending") {
		t.Fatalf("page starting below the previous max must be rejected, got %v", err)
	}
	if err := b.AddPageRows(newPage(21), 5); err == nil || !strings.Contains(err.Error(), "duplicate row") {
		t.Fatalf("page starting at the previous max must be rejected, got %v", err)
	}

	// 表切换与块 run 归属：两张表共用一个块，run 不得被表边界切断。
	b2 := newRowShardBuilder(7, 8)
	multi := &pageRows{
		rowIDs:   []uint64{1, 2, 3, 4},
		ordinals: []uint32{0, 1, 2, 3},
		changes:  []uint8{0, 0, 0, 0},
		// 表 1 拥有 [0,2)，表 2 拥有 [2,4)
		tableRunStart: []uint32{0, 2, 4},
		tableIDs:      []uint32{1, 2},
		blockRunStart: []uint32{0, 4},
		blockIDs:      []uint64{9},
	}
	err = b2.AddPageRows(multi, 7)
	require.NoError(t, err, "multi-table page")
	shards, err := b2.finish()
	require.NoError(t, err, "finish")
	if len(shards) != 2 {
		t.Fatalf("got %d shards, want 2", len(shards))
	}
	for tid, want := range map[uint32]int{1: 2, 2: 2} {
		if shards[tid].len() != want {
			t.Fatalf("shard %d has %d rows, want %d", tid, shards[tid].len(), want)
		}
		if len(shards[tid].blockIDs) != 1 || shards[tid].blockIDs[0] != 9 {
			t.Fatalf("shard %d block runs %v, want single run in block 9", tid, shards[tid].blockIDs)
		}
	}
}

// TestShardBuilderRowEntryGuards 直接钉住流式 AddRowEntry 的快照归属、表切换与
// 升序/重复守卫。
func TestShardBuilderRowEntryGuards(t *testing.T) {
	b := newRowShardBuilder(3, 4)
	if err := b.AddRowEntry(format.RowIndexEntry{SnapshotID: 4, TableID: 1, RowID: 1}); err == nil ||
		!strings.Contains(err.Error(), "wrong snapshot") {
		t.Fatalf("wrong snapshot must be rejected, got %v", err)
	}
	err := b.AddRowEntry(format.RowIndexEntry{SnapshotID: 3, TableID: 1, RowID: 5})
	require.NoError(t, err, "first entry")
	if err := b.AddRowEntry(format.RowIndexEntry{SnapshotID: 3, TableID: 1, RowID: 4}); err == nil ||
		!strings.Contains(err.Error(), "not ascending") {
		t.Fatalf("descending RowID must be rejected, got %v", err)
	}
	if err := b.AddRowEntry(format.RowIndexEntry{SnapshotID: 3, TableID: 1, RowID: 5}); err == nil ||
		!strings.Contains(err.Error(), "duplicate row") {
		t.Fatalf("duplicate RowID must be rejected, got %v", err)
	}
	// 表切换：切换即结算当前 shard，新表从自己的第一个 id 开始判序。
	err = b.AddRowEntry(format.RowIndexEntry{SnapshotID: 3, TableID: 2, RowID: 1})
	require.NoError(t, err, "table switch")
	shards, err := b.finish()
	require.NoError(t, err, "finish")
	if len(shards) != 2 || shards[1].len() != 1 || shards[2].len() != 1 {
		t.Fatalf("shards %v, want two single-row shards", shards)
	}
}
