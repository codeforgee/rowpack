package index

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

func buildTxn(t *testing.T, seq uint64, snap fileformat.SnapshotIndexEntry, meta []fileformat.MetadataIndexEntry, blocks []fileformat.BlockIndexEntry, rows []fileformat.RowIndexEntry, dataEnd uint64, footerCRC uint32) []byte {
	t.Helper()
	b := NewBuilder(seq)
	require.NoError(t, b.SetSnapshot(snap))
	for _, e := range meta {
		require.NoError(t, b.AddMetadata(e))
	}
	for _, e := range blocks {
		require.NoError(t, b.AddBlock(e))
	}
	for _, e := range rows {
		require.NoError(t, b.AddRow(e))
	}
	out, _, err := b.Build(snap.DataStart, dataEnd, footerCRC, 0, 0)
	require.NoError(t, err)
	return out
}

func fullSnap(id uint64, start, end uint64) fileformat.SnapshotIndexEntry {
	return fileformat.SnapshotIndexEntry{
		SnapshotID: id, SnapshotType: fileformat.SnapshotFull,
		BlockCount: 1, RowRecordCount: 2, DataStart: start, DataEnd: end,
		CreatedUnixNano: 1700000000000000000,
	}
}

func deltaSnap(id, parent, start, end uint64) fileformat.SnapshotIndexEntry {
	return fileformat.SnapshotIndexEntry{
		SnapshotID: id, ParentSnapshotID: parent, SnapshotType: fileformat.SnapshotDelta,
		BlockCount: 1, RowRecordCount: 1, DataStart: start, DataEnd: end,
		CreatedUnixNano: 1700000000000000000,
	}
}

func TestTxnRoundTrip(t *testing.T) {
	rows := []fileformat.RowIndexEntry{
		{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 10, BlockID: 1, ItemOrdinal: 0},
		{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 11, BlockID: 1, ItemOrdinal: 1},
	}
	data := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, []fileformat.BlockIndexEntry{
		{BlockID: 1, SnapshotID: 1, TableID: 1, BlockKind: fileformat.BlockKindRows, Compression: fileformat.CompressionZstd, DataOffset: 224, RawSize: 1000, StoredSize: 500, ItemCount: 2, RawCRC32C: 1},
	}, rows, 4096, 0xABCD)

	txn, err := ParseTxn(data)
	require.NoError(t, err)
	require.Equal(t, uint64(1), txn.Snapshot.SnapshotID, "parsed txn wrong: %+v", txn)
	require.Len(t, txn.Rows, 2, "parsed txn wrong: %+v", txn)
	require.Len(t, txn.Blocks, 1, "parsed txn wrong: %+v", txn)
	// Deterministic.
	data2 := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, []fileformat.BlockIndexEntry{
		{BlockID: 1, SnapshotID: 1, TableID: 1, BlockKind: fileformat.BlockKindRows, Compression: fileformat.CompressionZstd, DataOffset: 224, RawSize: 1000, StoredSize: 500, ItemCount: 2, RawCRC32C: 1},
	}, rows, 4096, 0xABCD)
	require.True(t, bytes.Equal(data, data2), "txn build not deterministic")
}

func TestParseTxnRejects(t *testing.T) {
	good := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, nil, nil, 4096, 0)
	// Truncations.
	for n := 0; n < len(good); n++ {
		_, err := ParseTxn(good[:n])
		require.Error(t, err, "accepted truncated txn %d/%d", n, len(good))
	}
	// Trailing bytes.
	_, err := ParseTxn(append(good, 1))
	require.Error(t, err, "accepted trailing bytes")
	// Body CRC corruption.
	bad := append([]byte(nil), good...)
	bad[len(bad)-10] ^= 0xFF
	_, err = ParseTxn(bad)
	require.Error(t, err, "accepted bad body CRC")
	// Snapshot entry CRC corruption (inside body).
	bad = append([]byte(nil), good...)
	bad[80+60] ^= 0xFF // snapshot entry CRC area
	_, err = ParseTxn(bad)
	require.Error(t, err, "accepted bad snapshot entry")
	// Footer CRC corruption.
	bad = append([]byte(nil), good...)
	bad[len(bad)-5] ^= 0xFF
	_, err = ParseTxn(bad)
	require.Error(t, err, "accepted bad footer")
}

func TestDuplicateRowRejected(t *testing.T) {
	b := NewBuilder(1)
	require.NoError(t, b.SetSnapshot(fullSnap(1, 128, 4096)))
	r1 := fileformat.RowIndexEntry{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 5, BlockID: 1, ItemOrdinal: 0}
	require.NoError(t, b.AddRow(r1))
	require.Error(t, b.AddRow(r1), "duplicate (table,row) accepted")
}

func TestViewImmutability(t *testing.T) {
	view := EmptyView()
	t1, _ := ParseTxn(buildTxn(t, 1, fullSnap(1, 128, 4096), nil, nil, []fileformat.RowIndexEntry{
		{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 1, BlockID: 1, ItemOrdinal: 0},
	}, 4096, 0))
	t2, _ := ParseTxn(buildTxn(t, 2, deltaSnap(2, 1, 4192, 8192), nil, nil, []fileformat.RowIndexEntry{
		{SnapshotID: 2, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 2, BlockID: 2, ItemOrdinal: 0},
	}, 8192, 0))

	v1, err := view.Apply(t1, 4096)
	require.NoError(t, err)
	v2, err := v1.Apply(t2, 4096)
	require.NoError(t, err)
	// v1 must not see snapshot 2.
	require.Nil(t, v1.Snapshot(2), "v1 leaked snapshot 2 (shared map mutation)")
	require.NotNil(t, v2.Snapshot(2), "v2 missing snapshot 2")
	require.NotNil(t, v2.Row(1, 1, 1), "v2 row resolution failed")
	require.NotNil(t, v2.Row(2, 1, 2), "v2 row resolution failed")
}
