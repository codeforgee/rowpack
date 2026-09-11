package index

import (
	"testing"

	"github.com/rowpack/rowpack/internal/format"
)

// TestStreamApplyMatchesBuffered verifies that the Open-path streaming apply
// (ApplyStreaming → rowShardBuilder) produces byte-identical row shards to the
// buffered Apply (buildShards). It covers empty snapshots, single-table,
// multi-table, and multi-page (crossing the indexPageEntryCount boundary)
// shapes. Every test builds the txn once, applies it both ways, and compares
// the resulting (RowID, ItemOrdinal, ChangeType, BlockID) per table.
func TestStreamApplyMatchesBuffered(t *testing.T) {
	full := format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull, BlockCount: 1}
	cases := []struct {
		name string
		rows []format.RowIndexEntry
	}{
		{"empty", nil},
		{"single-table-seq", riSeq(1000, 40)},
		{"multi-table", []format.RowIndexEntry{
			riEntry(1, 1, 1, 0, format.ChangeInsert),
			riEntry(1, 2, 1, 1, format.ChangeInsert),
			riEntry(1, 3, 2, 0, format.ChangeDelete),
			riEntry(2, 1, 3, 0, format.ChangeInsert),
			riEntry(2, 2, 3, 1, format.ChangeUpdate),
			riEntry(3, 1, 3, 0, format.ChangeInsert),
		}},
		{"multi-table-multi-page", mixedRows(5200)},
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
			v1, err := EmptyView().Apply(txn, 32)
			if err != nil {
				t.Fatal(err)
			}
			v2, err := EmptyView().ApplyStreaming(data, nil, 32)
			if err != nil {
				t.Fatal(err)
			}
			compareRowShards(t, v1, v2)
		})
	}
}

// mixedRows builds a deterministic multi-table set that spans several index
// pages (indexPageEntryCount entries), forcing table boundaries to fall in the
// middle of a page. Tables are interleaved in insertion order; the builder
// sorts by (TableID, RowID) so both paths must agree.
func mixedRows(n int) []format.RowIndexEntry {
	rows := make([]format.RowIndexEntry, 0, n)
	// Three tables, each with strictly increasing RowIDs; interleave so the
	// tables are not globally contiguous in insertion order (maliciously
	// unsorted input) — the encode path re-sorts it.
	for i := 0; i < n; i++ {
		rows = append(rows, riEntry(1, uint64(i)+1, uint64(i%97+1), uint32(i%50), format.ChangeInsert))
		rows = append(rows, riEntry(2, uint64(i)+1, uint64(i%89+1), uint32(i%30), format.ChangeUpdate))
		rows = append(rows, riEntry(3, uint64(i)+1, uint64(i%83+1), uint32(i%20), format.ChangeDelete))
	}
	return rows
}

// compareRowShards asserts two views expose identical per-table row shards
// (rowIDs, item ordinals, change types, and block-run decomposition).
func compareRowShards(t *testing.T, a, b *View) {
	t.Helper()
	tables := a.RowTables(1)
	btables := b.RowTables(1)
	if len(tables) != len(btables) {
		t.Fatalf("table count %d != %d", len(tables), len(btables))
	}
	for i := range tables {
		if tables[i] != btables[i] {
			t.Fatalf("table %d != %d", tables[i], btables[i])
		}
		sa := a.rows[1][tables[i]]
		sb := b.rows[1][btables[i]]
		if (sa == nil) != (sb == nil) {
			t.Fatalf("table %d nil mismatch", tables[i])
		}
		if sa == nil {
			continue
		}
		if len(sa.rowIDs) != len(sb.rowIDs) {
			t.Fatalf("table %d row count %d != %d", tables[i], len(sa.rowIDs), len(sb.rowIDs))
		}
		for j := range sa.rowIDs {
			if sa.rowIDs[j] != sb.rowIDs[j] {
				t.Fatalf("table %d row %d id %d != %d", tables[i], j, sa.rowIDs[j], sb.rowIDs[j])
			}
			if sa.ordinals[j] != sb.ordinals[j] {
				t.Fatalf("table %d row %d ordinal %d != %d", tables[i], j, sa.ordinals[j], sb.ordinals[j])
			}
			if sa.changes[j] != sb.changes[j] {
				t.Fatalf("table %d row %d change %d != %d", tables[i], j, sa.changes[j], sb.changes[j])
			}
		}
		if len(sa.runStart) != len(sb.runStart) || len(sa.blockIDs) != len(sb.blockIDs) {
			t.Fatalf("table %d run directory %d/%d != %d/%d", tables[i], len(sa.runStart), len(sa.blockIDs), len(sb.runStart), len(sb.blockIDs))
		}
		for j := range sa.blockIDs {
			if sa.blockIDs[j] != sb.blockIDs[j] {
				t.Fatalf("table %d run %d block %d != %d", tables[i], j, sa.blockIDs[j], sb.blockIDs[j])
			}
			if sa.runStart[j] != sb.runStart[j] {
				t.Fatalf("table %d run %d start %d != %d", tables[i], j, sa.runStart[j], sb.runStart[j])
			}
		}
		if sa.runStart[len(sa.runStart)-1] != uint32(len(sa.rowIDs)) {
			t.Fatalf("table %d terminal run start %d != %d", tables[i], sa.runStart[len(sa.runStart)-1], len(sa.rowIDs))
		}
	}
}
