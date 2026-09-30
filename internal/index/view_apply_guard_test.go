package index

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// This file pins View.Apply's validation arms with hand-built Txn values:
// chain invariants (uniqueness, parent existence and ordering, depth, type),
// block/metadata ownership, and the row-shard ownership check. Everything is
// in memory — no file involved.

// mustBaseView builds a committed FULL snapshot (id 1) holding one block and
// one row, returning the view after the apply.
func mustBaseView(t *testing.T) *View {
	t.Helper()
	v, err := EmptyView().Apply(mustFullTxn(t, 1), 32)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustFullTxn(t *testing.T, id uint64) *Txn {
	t.Helper()
	b := NewBuilder(id)
	if err := b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID: id, SnapshotType: format.SnapshotFull, BlockCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(format.BlockIndexEntry{
		BlockID: id * 100, SnapshotID: id, TableID: 1, BlockKind: format.BlockKindRows,
		RawSize: 64, StoredSize: 64, ItemCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(format.RowIndexEntry{
		SnapshotID: id, TableID: 1, RowID: 1, BlockID: id * 100, ChangeType: format.ChangeInsert,
	}); err != nil {
		t.Fatal(err)
	}
	_, txn, err := b.Build(BodyBounds{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return txn
}

// deltaTxn is a hand-built DELTA txn on parent 1.
func deltaTxn(id, parent uint64) *Txn {
	return &Txn{
		Snapshot: format.SnapshotIndexEntry{
			SnapshotID: id, ParentSnapshotID: parent, SnapshotType: format.SnapshotDelta,
		},
		Rows: []format.RowIndexEntry{{
			SnapshotID: id, TableID: 1, RowID: 2, BlockID: 100, ChangeType: format.ChangeInsert,
		}},
	}
}

func TestApplyRejectsInvalidTxns(t *testing.T) {
	v1 := mustBaseView(t)

	cases := []struct {
		name string
		run  func() (*View, error)
		want string
	}{
		{"nil txn", func() (*View, error) { return v1.Apply(nil, 32) }, "nil txn"},
		{"already committed", func() (*View, error) { return v1.Apply(mustFullTxn(t, 1), 32) }, "snapshot 1 already committed"},
		{"parent not committed", func() (*View, error) { return v1.Apply(deltaTxn(9, 42), 32) }, "snapshot 9 parent 42 not committed"},
		{"parent not smaller", func() (*View, error) {
			tx := deltaTxn(0, 1)
			return v1.Apply(tx, 32)
		}, "parent 1 not smaller"},
		{"depth exceeded", func() (*View, error) {
			tx := deltaTxn(2, 1)
			return v1.Apply(tx, 1) // child depth 2 > limit 1
		}, "depth 2 exceeds limit 1"},
		{"bad type", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Snapshot.SnapshotType = format.SnapshotType(99)
			tx.Rows = nil
			return v1.Apply(tx, 32)
		}, "snapshot 2 bad type 99"},
		{"full with parent", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Snapshot.SnapshotType = format.SnapshotFull
			tx.Rows = nil
			return v1.Apply(tx, 32)
		}, "FULL snapshot 2 has parent 1"},
		{"block belongs to other snapshot", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Rows = nil
			tx.Blocks = []format.BlockIndexEntry{{
				BlockID: 200, SnapshotID: 99, TableID: 1, BlockKind: format.BlockKindRows,
			}}
			return v1.Apply(tx, 32)
		}, "block 200 belongs to snapshot 99, want 2"},
		{"block already exists", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Rows = nil
			tx.Blocks = []format.BlockIndexEntry{{
				BlockID: 100, SnapshotID: 2, TableID: 1, BlockKind: format.BlockKindRows,
			}}
			return v1.Apply(tx, 32)
		}, "block 100 already exists"},
		{"metadata wrong snapshot", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Rows = nil
			tx.Metadata = []format.MetadataIndexEntry{{SnapshotID: 99, ObjectID: 7}}
			return v1.Apply(tx, 32)
		}, "metadata entry 7 wrong snapshot"},
		{"metadata duplicated", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Rows = nil
			tx.Metadata = []format.MetadataIndexEntry{
				{SnapshotID: 2, ObjectID: 7}, {SnapshotID: 2, ObjectID: 7},
			}
			return v1.Apply(tx, 32)
		}, "metadata object 7 duplicated in snapshot 2"},
		{"row entry wrong snapshot", func() (*View, error) {
			tx := deltaTxn(2, 1)
			tx.Rows = []format.RowIndexEntry{{SnapshotID: 99, TableID: 1, RowID: 2}}
			return v1.Apply(tx, 32)
		}, "row entry wrong snapshot"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.run()
			require.Error(t, err, "accepted")
			require.Contains(t, err.Error(), tc.want, "error %q, want substring %q", err, tc.want)
			// The receiver must stay unmodified on failure.
			require.Nil(t, v, "failed apply returned a non-nil view")
		})
	}
}

// TestApplyMultiTableShardOwnership covers the multi-table branch of
// buildShards with an owned row per table plus one wrong-snapshot entry.
func TestApplyMultiTableShardOwnership(t *testing.T) {
	v1 := mustBaseView(t)
	tx := deltaTxn(2, 1)
	tx.Rows = []format.RowIndexEntry{
		{SnapshotID: 2, TableID: 1, RowID: 2, BlockID: 100},
		{SnapshotID: 2, TableID: 2, RowID: 3, BlockID: 100},
	}
	v2, err := v1.Apply(tx, 32)
	if err != nil {
		t.Fatal(err)
	}
	if v2.Snapshot(2) == nil {
		t.Fatal("snapshot 2 missing after multi-table apply")
	}

	bad := deltaTxn(3, 2)
	bad.Rows = []format.RowIndexEntry{
		{SnapshotID: 3, TableID: 1, RowID: 4},
		{SnapshotID: 77, TableID: 2, RowID: 5},
	}
	if _, err := v2.Apply(bad, 32); err == nil || !strings.Contains(err.Error(), "row entry wrong snapshot") {
		t.Fatalf("multi-table ownership not enforced: %v", err)
	}
}
