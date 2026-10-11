package index

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
)

// TestApplyBlocksAllocs is a measurement probe: allocations of applying a
// small txn on top of a view with many existing blocks. The base+delta block
// table must not re-hash every block per commit.
func TestApplyBlocksAllocs(t *testing.T) {
	if testing.Short() {
		t.Skip("probe: long")
	}
	for _, nBlocks := range []int{10_000, 100_000} {
		nBlocks := nBlocks
		t.Run(fmt.Sprintf("blocks=%d", nBlocks), func(t *testing.T) {
			v := EmptyView()
			b := NewBuilder(1)
			b.SetRowDedup(false)
			_ = b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull, CreatedUnixNano: 1})
			for i := 1; i <= nBlocks; i++ {
				_ = b.AddBlock(blockEntry(uint64(i), 1, 1))
			}
			txn := &Txn{Snapshot: *b.snapshot, Metadata: b.metadata, Blocks: b.blocks, Rows: b.rows}
			v, err := v.Apply(txn, 64)
			if err != nil {
				t.Fatal(err)
			}
			// A small follow-up txn: 8 blocks.
			b2 := NewBuilder(2)
			b2.SetRowDedup(false)
			_ = b2.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 2, ParentSnapshotID: 1, SnapshotType: format.SnapshotDelta, CreatedUnixNano: 2})
			for i := 1; i <= 8; i++ {
				_ = b2.AddBlock(blockEntry(uint64(nBlocks+i), 2, 1))
			}
			txn2 := &Txn{Snapshot: *b2.snapshot, Metadata: b2.metadata, Blocks: b2.blocks, Rows: b2.rows}
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			nv, err := v.Apply(txn2, 64)
			if err != nil {
				t.Fatal(err)
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			mb := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
			t.Logf("blocks=%d small-commit-allocs=%.2f MB", nBlocks, mb)
			_ = nv
		})
	}
}
