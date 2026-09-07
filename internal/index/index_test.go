package index

import (
	"bytes"
	"fmt"
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

type fakeFooterReader struct {
	crcs map[uint64]uint32
}

func (f *fakeFooterReader) DataFooterCRC(snapshotID uint64, _, _ uint64) (uint32, error) {
	if c, ok := f.crcs[snapshotID]; ok {
		return c, nil
	}
	return 0, fmt.Errorf("no footer for %d", snapshotID)
}

func TestReplay(t *testing.T) {
	// FULL S1, DELTA S2 <- S1.
	t1 := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, []fileformat.BlockIndexEntry{
		{BlockID: 1, SnapshotID: 1, TableID: 1, BlockKind: fileformat.BlockKindRows, Compression: fileformat.CompressionZstd, DataOffset: 224, RawSize: 1000, StoredSize: 500, ItemCount: 2, RawCRC32C: 1},
	}, []fileformat.RowIndexEntry{
		{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 10, BlockID: 1, ItemOrdinal: 0},
	}, 4096, 0xAA)
	t2 := buildTxn(t, 2, deltaSnap(2, 1, 4192, 8192), nil, []fileformat.BlockIndexEntry{
		{BlockID: 2, SnapshotID: 2, TableID: 1, BlockKind: fileformat.BlockKindRows, Compression: fileformat.CompressionZstd, DataOffset: 4288, RawSize: 500, StoredSize: 200, ItemCount: 1, RawCRC32C: 2},
	}, []fileformat.RowIndexEntry{
		{SnapshotID: 2, TableID: 1, ChangeType: fileformat.ChangeUpdate, RowID: 10, BlockID: 2, ItemOrdinal: 0},
	}, 8192, 0xBB)

	hdr := make([]byte, 128)
	data := append(append(append([]byte(nil), hdr...), t1...), t2...)
	data = append(data, make([]byte, 7)...) // trailing partial

	res, err := Replay(data, 128, 4096, &fakeFooterReader{crcs: map[uint64]uint32{1: 0xAA, 2: 0xBB}})
	require.NoError(t, err)
	require.Equal(t, 2, res.Txns, "replayed %d txns", res.Txns)
	require.Equal(t, int64(7), res.TailIgnored, "tail ignored %d, want 7", res.TailIgnored)
	v := res.View
	require.NotNil(t, v.Snapshot(1), "snapshots missing")
	require.NotNil(t, v.Snapshot(2), "snapshots missing")
	require.Equal(t, uint32(2), v.Snapshot(2).Depth, "depth = %d, want 2", v.Snapshot(2).Depth)
	if r := v.Row(2, 1, 10); r == nil || r.ChangeType != fileformat.ChangeUpdate {
		require.Fail(t, "row resolution failed")
	}
	if r := v.Row(1, 1, 10); r == nil || r.ChangeType != fileformat.ChangeInsert {
		require.Fail(t, "snapshot 1 row wrong")
	}
	if b := v.Block(1); b == nil || b.DataOffset != 224 {
		require.Fail(t, "block resolution failed")
	}
	if got := v.RowKeys(1, 1); len(got) != 1 || got[0].RowID != 10 {
		require.Fail(t, "row keys failed")
	}
	require.NotZero(t, v.MemoryBytes(), "memory stats not tracked")
}

func TestReplayParentChainValidation(t *testing.T) {
	// DELTA without its parent must stop replay.
	t1 := buildTxn(t, 1, deltaSnap(2, 1, 128, 4096), nil, nil, nil, 4096, 0)
	hdr := make([]byte, 128)
	data := append(hdr, t1...)
	res, err := Replay(data, 128, 4096, nil)
	require.NoError(t, err)
	require.Zero(t, res.Txns, "orphan delta replayed %d txns", res.Txns)
	require.Nil(t, res.View.LatestSnapshot(), "view should be empty")

	// Depth limit.
	depth := uint32(2)
	view := EmptyView()
	seq := uint64(1)
	reached := false
	for i := uint64(1); i <= 4; i++ {
		var snap fileformat.SnapshotIndexEntry
		if i == 1 {
			snap = fullSnap(i, i*100, i*100+100)
		} else {
			snap = deltaSnap(i, i-1, i*100, i*100+100)
		}
		tx := buildTxn(t, seq, snap, nil, nil, nil, i*100+100, 0)
		parsed, err := ParseTxn(tx)
		require.NoError(t, err)
		nv, err := view.Apply(parsed, depth)
		if err != nil {
			require.Equal(t, uint64(3), i, "depth limit hit at depth %d", i)
			reached = true
			break
		}
		view = nv
		seq++
	}
	require.True(t, reached, "depth limit not enforced")
}

func TestReplayDuplicateSnapshotStops(t *testing.T) {
	t1 := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, nil, nil, 4096, 0)
	t2 := buildTxn(t, 2, fullSnap(1, 8192, 12288), nil, nil, nil, 12288, 0) // duplicate snapshot 1
	hdr := make([]byte, 128)
	data := append(append(hdr, t1...), t2...)
	res, err := Replay(data, 128, 4096, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Txns, "replayed %d txns, want 1 (duplicate stops)", res.Txns)
	require.NotNil(t, res.View.Snapshot(1), "first snapshot missing")
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
