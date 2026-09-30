package index

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// view_arms_test.go 覆盖 View 的「查不到」与「拒绝」两臂:空 shard、空 map、
// 快照链合并堆、以及流式 apply 的每个放行又碾回来的地方。

func insRows(rowIDs ...uint64) []format.RowIndexEntry {
	return tableRows(format.ChangeInsert, rowIDs...)
}
func updRows(rowIDs ...uint64) []format.RowIndexEntry {
	return tableRows(format.ChangeUpdate, rowIDs...)
}
func delRows(rowIDs ...uint64) []format.RowIndexEntry {
	return tableRows(format.ChangeDelete, rowIDs...)
}

func tableRows(ct format.ChangeType, rowIDs ...uint64) []format.RowIndexEntry {
	out := make([]format.RowIndexEntry, 0, len(rowIDs))
	for _, id := range rowIDs {
		out = append(out, riEntry(1, id, id, 0, ct))
	}
	return out
}

// applyRows builds one txn carrying rows into v and returns the new view.
func applyRows(tb testing.TB, v *View, se format.SnapshotIndexEntry, rows []format.RowIndexEntry) *View {
	tb.Helper()
	b := NewBuilder(1)
	require.NoError(tb, b.SetSnapshot(se))
	for i := range rows {
		e := rows[i]
		e.SnapshotID = se.SnapshotID
		require.NoError(tb, b.AddRow(e))
	}
	_, txn, err := b.Build(BodyBounds{}, 0)
	require.NoError(tb, err)
	nv, err := v.Apply(txn, 32)
	require.NoError(tb, err)
	return nv
}

// fourLayerView builds a four-deep chain whose layers overlap RowIDs, so the
// LogicalRowCount merge holds every layer in one heap at a time.
func fourLayerView(tb testing.TB) *View {
	tb.Helper()
	v := applyRows(tb, EmptyView(), snapEntry(1, 0, format.SnapshotFull), insRows(1, 2, 3))
	v = applyRows(tb, v, snapEntry(2, 1, format.SnapshotDelta), append(updRows(2), insRows(4)...))
	v = applyRows(tb, v, snapEntry(3, 2, format.SnapshotDelta), append(delRows(1), insRows(5)...))
	return applyRows(tb, v, snapEntry(4, 3, format.SnapshotDelta), append(delRows(3), insRows(6)...))
}

func TestNilRowShardLookup(t *testing.T) {
	var sh *rowShard
	_, ok := sh.lookup(7)
	require.False(t, ok, "a missing table must answer \"absent\" rather than panic")
}

func TestRowAndMetadataMisses(t *testing.T) {
	v := applyRows(t, EmptyView(), snapEntry(1, 0, format.SnapshotFull), insRows(1, 2))

	t.Run("table without rows", func(t *testing.T) {
		_, ok := v.Row(1, 99, 1)
		require.False(t, ok, "no shard means no row, not a zero location")
	})

	t.Run("snapshot without metadata", func(t *testing.T) {
		require.Nil(t, v.Metadata(77, 1), "an unknown layer has no metadata layer to look in")
	})
}

// TestLogicalRowCountMergesParentChain: the RowID equality window and the heap
// sift are what actually resolve overrides and tombstones, so they need layers
// deep enough to compare both children.
func TestLogicalRowCountMergesParentChain(t *testing.T) {
	v := fourLayerView(t)

	require.EqualValues(t, 3, v.LogicalRowCount(1, 1), "the full layer alone")
	require.EqualValues(t, 4, v.LogicalRowCount(2, 1), "row 2 is overridden by the newer layer")
	require.EqualValues(t, 4, v.LogicalRowCount(3, 1), "layer 3 tombstones row 1")
	// Layer 4 tombstones row 3 and adds row 6: visible rows are 2, 4, 5, 6.
	require.EqualValues(t, 4, v.LogicalRowCount(4, 1))
	require.EqualValues(t, 0, v.LogicalRowCount(4, 9), "a table no layer mentions is empty")
}

func TestApplyStreamingRejectsGarbage(t *testing.T) {
	_, err := EmptyView().ApplyStreaming([]byte("rowpack txn placeholder"), nil, 32)
	require.Error(t, err, "a non-transaction must be rejected, not half-applied")
}

// TestApplyStreamingRejectsDuplicateMetadataObjects: the streaming path buffers
// metadata and validates it at finish, where the duplicate check owns the error.
func TestApplyStreamingRejectsDuplicateMetadataObjects(t *testing.T) {
	b := NewBuilder(1)
	require.NoError(t, b.SetSnapshot(snapEntry(5, 0, format.SnapshotFull)))
	require.NoError(t, b.AddMetadata(metaEntry(5, 1, 1)))
	require.NoError(t, b.AddMetadata(metaEntry(5, 1, 1)))
	data, _, err := b.Build(BodyBounds{}, 0)
	require.NoError(t, err)

	_, err = EmptyView().ApplyStreaming(data, nil, 32)
	require.Error(t, err)
	require.Contains(t, err.Error(), "metadata object 1 duplicated")
}

// TestApplyStreamingRejectsForeignBlocks: blocks are validated against the view
// being copied, so a txn reusing an older snapshot's block id is refused.
func TestApplyStreamingRejectsForeignBlocks(t *testing.T) {
	first := NewBuilder(1)
	require.NoError(t, first.SetSnapshot(snapEntry(6, 0, format.SnapshotFull)))
	require.NoError(t, first.AddBlock(blockEntry(11, 6, 1)))
	data, _, err := first.Build(BodyBounds{}, 0)
	require.NoError(t, err)
	v, err := EmptyView().ApplyStreaming(data, nil, 32)
	require.NoError(t, err)

	second := NewBuilder(1)
	require.NoError(t, second.SetSnapshot(snapEntry(7, 0, format.SnapshotFull)))
	require.NoError(t, second.AddBlock(blockEntry(11, 7, 1)))
	data, _, err = second.Build(BodyBounds{}, 0)
	require.NoError(t, err)

	_, err = v.ApplyStreaming(data, nil, 32)
	require.Error(t, err)
	require.Contains(t, err.Error(), "block 11 already exists")
}

func TestStreamApplyGuards(t *testing.T) {
	t.Run("snapshot install fails", func(t *testing.T) {
		v := applyRows(t, EmptyView(), snapEntry(1, 0, format.SnapshotFull), insRows(1))
		a := &streamApply{old: v, maxDepth: 32}
		err := a.SetSnapshot(snapEntry(1, 0, format.SnapshotFull))
		require.Error(t, err, "the chain validators own the answer; the sink must not paper over it")
		require.Contains(t, err.Error(), "already committed")
	})

	t.Run("second snapshot chunk", func(t *testing.T) {
		a := &streamApply{old: EmptyView(), maxDepth: 32}
		require.NoError(t, a.SetSnapshot(snapEntry(2, 0, format.SnapshotFull)))
		require.ErrorContains(t, a.SetSnapshot(snapEntry(3, 0, format.SnapshotFull)), "duplicate snapshot chunk")
	})

	t.Run("row batch before the snapshot", func(t *testing.T) {
		a := &streamApply{old: EmptyView(), maxDepth: 32}
		require.ErrorContains(t, a.AddRowBatch(&pageRows{}, 0), "row entry before snapshot")
	})

	t.Run("row batch owned by another snapshot", func(t *testing.T) {
		a := &streamApply{old: EmptyView(), maxDepth: 32}
		require.NoError(t, a.SetSnapshot(snapEntry(8, 0, format.SnapshotFull)))
		require.ErrorContains(t, a.AddRowBatch(&pageRows{}, 999), "row entry wrong snapshot")
	})

	t.Run("finish without a snapshot chunk", func(t *testing.T) {
		a := &streamApply{old: EmptyView(), maxDepth: 32}
		require.ErrorContains(t, a.finish(), "no snapshot chunk")
	})
}

// TestBuildShardsRejectsDuplicateRowKeys pins strict duplicates in one layer,
// which the eager path can only catch while compacting into columnar form.
func TestBuildShardsRejectsDuplicateRowKeys(t *testing.T) {
	owned := func(rows []format.RowIndexEntry) []format.RowIndexEntry {
		for i := range rows {
			rows[i].SnapshotID = 1
		}
		return rows
	}

	single := []format.RowIndexEntry{
		riEntry(1, 4, 1, 0, format.ChangeInsert),
		riEntry(1, 4, 1, 1, format.ChangeUpdate),
	}
	_, err := buildShards(&Txn{Rows: owned(single)}, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate row 4")

	multi := []format.RowIndexEntry{
		riEntry(1, 4, 1, 0, format.ChangeInsert),
		riEntry(1, 4, 1, 1, format.ChangeUpdate),
		riEntry(2, 9, 1, 0, format.ChangeInsert),
	}
	_, err = buildShards(&Txn{Rows: owned(multi)}, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate row 4", "the multi-table fallback must check every shard")
}
