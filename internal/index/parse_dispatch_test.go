package index

import (
	"fmt"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// entrySink is a TxnSink that implements RowEntrySink (but deliberately not
// rowBatchSink), forcing the page parser down the per-entry walkPage path —
// the fallback streamApply keeps for sinks without columnar batch support and
// the only exercise rowShardBuilder.AddRowEntry ever gets.
type entrySink struct {
	snapID uint64
	have   bool
	shards *rowShardBuilder
}

func (s *entrySink) SetSnapshot(e format.SnapshotIndexEntry) error {
	if s.have {
		return fmt.Errorf("duplicate snapshot")
	}
	s.have = true
	s.snapID = e.SnapshotID
	s.shards = newRowShardBuilder(e.SnapshotID, 0)
	return nil
}

func (s *entrySink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *entrySink) AddBlock(format.BlockIndexEntry) error       { return nil }

func (s *entrySink) AddRowEntry(e format.RowIndexEntry) error { return s.shards.AddRowEntry(e) }

func (s *entrySink) AddRows(batch []format.RowIndexEntry) error {
	for i := range batch {
		if err := s.AddRowEntry(batch[i]); err != nil {
			return err
		}
	}
	return nil
}

// batchSinkSink implements only the TxnSink baseline: the parser decodes each
// page via decodePage and hands out []RowIndexEntry batches through AddRows —
// the third, least-exercised dispatch branch.
type rowsSink struct {
	snapID uint64
	have   bool
	shards *rowShardBuilder
}

func (s *rowsSink) SetSnapshot(e format.SnapshotIndexEntry) error {
	if s.have {
		return fmt.Errorf("duplicate snapshot")
	}
	s.have = true
	s.snapID = e.SnapshotID
	s.shards = newRowShardBuilder(e.SnapshotID, 0)
	return nil
}

func (s *rowsSink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *rowsSink) AddBlock(format.BlockIndexEntry) error       { return nil }

func (s *rowsSink) AddRows(batch []format.RowIndexEntry) error {
	for i := range batch {
		if err := s.shards.AddRowEntry(batch[i]); err != nil {
			return err
		}
	}
	return nil
}

// TestParseSinkDispatchPathsEquivalence builds one txn and parses it through
// all three sink dispatch branches (rowBatchSink / RowEntrySink / plain
// TxnSink AddRows). The resulting row shards must be identical — the fallback
// paths previously had zero coverage.
func TestParseSinkDispatchPathsEquivalence(t *testing.T) {
	full := format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull, BlockCount: 1}
	cases := []struct {
		name string
		rows []format.RowIndexEntry
	}{
		{"empty", nil},
		{"single-table", riSeq(1000, 40)},
		{"multi-table", []format.RowIndexEntry{
			riEntry(1, 1, 1, 0, format.ChangeInsert),
			riEntry(1, 2, 1, 1, format.ChangeInsert),
			riEntry(1, 3, 2, 0, format.ChangeDelete),
			riEntry(2, 1, 3, 0, format.ChangeInsert),
			riEntry(2, 2, 3, 1, format.ChangeUpdate),
			riEntry(3, 1, 3, 0, format.ChangeInsert),
		}},
		// Same BlockID re-appearing after another block within one table:
		// non-monotonic block runs (possible in a DELTA whose RowID order
		// differs from its block write order).
		{"block-run-restart", []format.RowIndexEntry{
			riEntry(1, 1, 9, 0, format.ChangeInsert),
			riEntry(1, 2, 3, 0, format.ChangeInsert),
			riEntry(1, 3, 9, 1, format.ChangeInsert),
			riEntry(1, 4, 3, 1, format.ChangeInsert),
			riEntry(1, 5, 9, 2, format.ChangeUpdate),
		}},
		{"multi-page", mixedRows(5200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBuilder(1)
			b.SetRowDedup(false)
			if err := b.SetSnapshot(full); err != nil {
				t.Fatal(err)
			}
			for i := range tc.rows {
				e := tc.rows[i]
				e.SnapshotID = 1
				if err := b.AddRow(e); err != nil {
					t.Fatal(err)
				}
			}
			data, txn, err := b.Build(BodyBounds{}, 0)
			if err != nil {
				t.Fatal(err)
			}

			// Path 1: buffered Apply (reference).
			vBuf, err := EmptyView().Apply(txn, 32)
			if err != nil {
				t.Fatal(err)
			}
			// Path 2: streaming batch sink (AddRowBatch).
			vStream, err := EmptyView().ApplyStreaming(data, nil, 32)
			if err != nil {
				t.Fatal(err)
			}
			// Path 3: per-entry sink (AddRowEntry via walkPage).
			var es entrySink
			if _, err := parseStream(data, nil, &es); err != nil {
				t.Fatal(err)
			}
			if !es.have {
				t.Fatal("entry sink never saw the snapshot chunk")
			}
			// Path 4: plain AddRows batches (decodePage branch).
			var rs rowsSink
			if _, err := parseStream(data, nil, &rs); err != nil {
				t.Fatal(err)
			}
			if !rs.have {
				t.Fatal("rows sink never saw the snapshot chunk")
			}

			vEntry := EmptyView()
			shards, err := es.shards.finish()
			if err != nil {
				t.Fatal(err)
			}
			vEntry.rows[1] = shards
			vRows := EmptyView()
			shards2, err := rs.shards.finish()
			if err != nil {
				t.Fatal(err)
			}
			vRows.rows[1] = shards2

			compareRowShards(t, vBuf, vStream)
			compareRowShards(t, vBuf, vEntry)
			compareRowShards(t, vBuf, vRows)
		})
	}
}

// TestStreamApplyFallbackDelegates exercises the streamApply per-entry and
// AddRows fallbacks directly: ApplyStreaming always selects the batch path,
// so these delegates would otherwise stay uncovered forever.
func TestStreamApplyFallbackDelegates(t *testing.T) {
	full := format.SnapshotIndexEntry{SnapshotID: 7, SnapshotType: format.SnapshotFull, BlockCount: 1}

	ap := &streamApply{old: EmptyView(), maxDepth: 32}
	require.Error(t, ap.AddRowEntry(format.RowIndexEntry{}), "entry before snapshot")
	require.NoError(t, ap.AddRows(nil), "empty batch is a no-op even before a snapshot")
	require.Error(t, ap.AddRows([]format.RowIndexEntry{{}}), "rows before snapshot")
	require.Error(t, ap.AddBlock(format.BlockIndexEntry{}), "block before snapshot")
	require.Error(t, ap.AddMetadata(format.MetadataIndexEntry{}), "metadata before snapshot")

	require.NoError(t, ap.SetSnapshot(full))
	require.Error(t, ap.AddRowEntry(format.RowIndexEntry{}), "wrong snapshot")
	require.NoError(t, ap.AddRowEntry(format.RowIndexEntry{
		SnapshotID: 7, TableID: 1, RowID: 1, BlockID: 1, ChangeType: format.ChangeInsert,
	}))
	require.NoError(t, ap.AddRows([]format.RowIndexEntry{
		{SnapshotID: 7, TableID: 1, RowID: 2, BlockID: 1, ChangeType: format.ChangeInsert},
	}))
	require.NoError(t, ap.AddBlock(format.BlockIndexEntry{BlockID: 1, SnapshotID: 7}))
	require.NoError(t, ap.AddMetadata(format.MetadataIndexEntry{ObjectID: 1, SnapshotID: 7}))
	require.NoError(t, ap.finish())

	// Compare against the batch path for the same entries.
	b := NewBuilder(7)
	b.SetRowDedup(false)
	require.NoError(t, b.SetSnapshot(full))
	for _, e := range []format.RowIndexEntry{
		{SnapshotID: 7, TableID: 1, RowID: 1, BlockID: 1, ChangeType: format.ChangeInsert},
		{SnapshotID: 7, TableID: 1, RowID: 2, BlockID: 1, ChangeType: format.ChangeInsert},
	} {
		require.NoError(t, b.AddRow(e))
	}
	data, _, err := b.Build(BodyBounds{}, 0)
	require.NoError(t, err)
	vBatch, err := EmptyView().ApplyStreaming(data, nil, 32)
	require.NoError(t, err)
	vEntry := EmptyView()
	vEntry.rows[7] = map[uint32]*rowShard(nil)
	entryShards, err := ap.shards.finish()
	require.NoError(t, err)
	vEntry.rows[7] = entryShards
	compareRowShards(t, vBatch, vEntry)

	// Guards: duplicate snapshot chunk; finish without SetSnapshot.
	ap2 := &streamApply{old: EmptyView(), maxDepth: 32}
	require.NoError(t, ap2.SetSnapshot(full))
	require.Error(t, ap2.SetSnapshot(full))
	ap3 := &streamApply{old: EmptyView(), maxDepth: 32}
	require.Error(t, ap3.finish(), "finish without a snapshot chunk must fail")
}
