package index

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// RowKeyIter.Seek / MetadataObjects 本包覆盖为 0%（Scan 与 schema 推导在
// 根包跨包调用，不计入）。补直接单测锁定 seek 语义与 ID 排序。

func rowShardFor(t *testing.T, rows []RowKeyLoc) *rowShard {
	t.Helper()
	sh := &rowShard{}
	if err := sh.prepare(rows); err != nil {
		t.Fatal(err)
	}
	return sh
}

func TestRowKeyIterSeek(t *testing.T) {
	sh := rowShardFor(t, []RowKeyLoc{
		{RowID: 5, Loc: RowLoc{BlockID: 1, ItemOrdinal: 0}},
		{RowID: 10, Loc: RowLoc{BlockID: 1, ItemOrdinal: 1}},
		{RowID: 15, Loc: RowLoc{BlockID: 2, ItemOrdinal: 0}},
	})
	it := &RowKeyIter{it: &rowShardIter{sh: sh}}

	// 命中中间值。
	it.Seek(10)
	if it.Done() || it.RowID() != 10 {
		t.Fatalf("seek(10) -> rowID %d done %v", it.RowID(), it.Done())
	}
	// 目标小于首行：停在首行。
	it.Seek(1)
	if it.Done() || it.RowID() != 5 {
		t.Fatalf("seek(1) -> rowID %d", it.RowID())
	}
	// 目标位于两行之间：取 >= 的第一个。
	it.Seek(11)
	if it.Done() || it.RowID() != 15 {
		t.Fatalf("seek(11) -> rowID %d", it.RowID())
	}
	// 目标大于最大行：耗尽。
	it.Seek(99)
	if !it.Done() {
		t.Fatalf("seek(99) not done")
	}
	// 从末尾回退重新 seek 仍然可用。
	it.Seek(10)
	if it.Done() || it.RowID() != 10 {
		t.Fatalf("second seek(10) -> rowID %d", it.RowID())
	}
	it.Next()
	if it.Done() || it.RowID() != 15 {
		t.Fatalf("after next -> rowID %d", it.RowID())
	}
}

func TestMetadataObjectsNilAndSorted(t *testing.T) {
	v := EmptyView()
	require.Nil(t, v.MetadataObjects(1), "absent snapshot must return nil")
	// 手工构造一个含两个 object 的 snapshot。
	v.snapshots[1] = &SnapshotMeta{ID: 1}
	v.metadata[1] = map[uint64]*MetadataLoc{
		9:  {ObjectID: 9},
		2:  {ObjectID: 2},
		77: {ObjectID: 77},
	}
	got := v.MetadataObjects(1)
	if len(got) != 3 || got[0] != 2 || got[1] != 9 || got[2] != 77 {
		t.Fatalf("MetadataObjects = %v, want [2 9 77]", got)
	}
	// 空 map 且无记录类型。
	if got := v.MetadataByType(1, uint32(format.RecordTable)); len(got) != 0 {
		t.Fatalf("MetadataByType = %v", got)
	}
}
