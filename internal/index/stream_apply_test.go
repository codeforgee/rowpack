package index

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// stream_apply_test.go holds the fixtures shared by the streaming-apply
// equivalence checks:
//   - mixedRows: a deterministic multi-table set spanning several index pages.
//   - compareRowShards: the assertion that two views expose identical shards.
//
// The shapes themselves (empty / single-table / multi-table / multi-page /
// block-run-restart) are exercised by parse_dispatch_test.go
// TestParseSinkDispatchPathsEquivalence, which additionally drives the entry
// and rows fallback sinks — a strict superset of the old per-shape test here.

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
		require.Equal(t, btables[i], tables[i], "table %d != %d", tables[i], btables[i])
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
			require.Equal(t, sb.rowIDs[j], sa.rowIDs[j], "table %d row %d id %d != %d", tables[i], j, sa.rowIDs[j], sb.rowIDs[j])
			require.Equal(t, sb.ordinals[j], sa.ordinals[j], "table %d row %d ordinal %d != %d", tables[i], j, sa.ordinals[j], sb.ordinals[j])
			require.Equal(t, sb.changes[j], sa.changes[j], "table %d row %d change %d != %d", tables[i], j, sa.changes[j], sb.changes[j])
		}
		if len(sa.runStart) != len(sb.runStart) || len(sa.blockIDs) != len(sb.blockIDs) {
			t.Fatalf("table %d run directory %d/%d != %d/%d", tables[i], len(sa.runStart), len(sa.blockIDs), len(sb.runStart), len(sb.blockIDs))
		}
		for j := range sa.blockIDs {
			require.Equal(t, sb.blockIDs[j], sa.blockIDs[j], "table %d run %d block %d != %d", tables[i], j, sa.blockIDs[j], sb.blockIDs[j])
			require.Equal(t, sb.runStart[j], sa.runStart[j], "table %d run %d start %d != %d", tables[i], j, sa.runStart[j], sb.runStart[j])
		}
		if sa.runStart[len(sa.runStart)-1] != uint32(len(sa.rowIDs)) {
			t.Fatalf("table %d terminal run start %d != %d", tables[i], sa.runStart[len(sa.runStart)-1], len(sa.rowIDs))
		}
	}
}
