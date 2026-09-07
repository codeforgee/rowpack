package index

import (
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

func BenchmarkViewApply1M(b *testing.B) {
	const n = 100000
	rows := make([]fileformat.RowIndexEntry, n)
	for i := 0; i < n; i++ {
		rows[i] = fileformat.RowIndexEntry{
			SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert,
			RowID: uint64(i + 1), BlockID: uint64(i / 1280), ItemOrdinal: uint32(i % 1280),
		}
	}
	txn := &Txn{Rows: rows}
	txn.Snapshot = fileformat.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: fileformat.SnapshotFull}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		v, err := EmptyView().Apply(txn, 100)
		require.NoError(b, err)
		_ = v
	}
}
