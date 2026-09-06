package index

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func buildTxn(t *testing.T, seq uint64, snap fileformat.SnapshotIndexEntry, meta []fileformat.MetadataIndexEntry, blocks []fileformat.BlockIndexEntry, rows []fileformat.RowIndexEntry, dataEnd uint64, footerCRC uint32) []byte {
	t.Helper()
	b := NewBuilder(seq)
	if err := b.SetSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	for _, e := range meta {
		if err := b.AddMetadata(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range blocks {
		if err := b.AddBlock(e); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range rows {
		if err := b.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	out, err := b.Build(snap.DataStart, dataEnd, footerCRC)
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if txn.Snapshot.SnapshotID != 1 || len(txn.Rows) != 2 || len(txn.Blocks) != 1 {
		t.Fatalf("parsed txn wrong: %+v", txn)
	}
	// Deterministic.
	data2 := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, []fileformat.BlockIndexEntry{
		{BlockID: 1, SnapshotID: 1, TableID: 1, BlockKind: fileformat.BlockKindRows, Compression: fileformat.CompressionZstd, DataOffset: 224, RawSize: 1000, StoredSize: 500, ItemCount: 2, RawCRC32C: 1},
	}, rows, 4096, 0xABCD)
	if !bytes.Equal(data, data2) {
		t.Fatal("txn build not deterministic")
	}
}

func TestParseTxnRejects(t *testing.T) {
	good := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, nil, nil, 4096, 0)
	// Truncations.
	for n := 0; n < len(good); n++ {
		if _, err := ParseTxn(good[:n]); err == nil {
			t.Fatalf("accepted truncated txn %d/%d", n, len(good))
		}
	}
	// Trailing bytes.
	if _, err := ParseTxn(append(good, 1)); err == nil {
		t.Fatal("accepted trailing bytes")
	}
	// Body CRC corruption.
	bad := append([]byte(nil), good...)
	bad[len(bad)-10] ^= 0xFF
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("accepted bad body CRC")
	}
	// Snapshot entry CRC corruption (inside body).
	bad = append([]byte(nil), good...)
	bad[80+60] ^= 0xFF // snapshot entry CRC area
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("accepted bad snapshot entry")
	}
	// Footer CRC corruption.
	bad = append([]byte(nil), good...)
	bad[len(bad)-5] ^= 0xFF
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("accepted bad footer")
	}
}

func TestDuplicateRowRejected(t *testing.T) {
	b := NewBuilder(1)
	if err := b.SetSnapshot(fullSnap(1, 128, 4096)); err != nil {
		t.Fatal(err)
	}
	r1 := fileformat.RowIndexEntry{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 5, BlockID: 1, ItemOrdinal: 0}
	if err := b.AddRow(r1); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(r1); err == nil {
		t.Fatal("duplicate (table,row) accepted")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if res.Txns != 2 {
		t.Fatalf("replayed %d txns", res.Txns)
	}
	if res.TailIgnored != 7 {
		t.Fatalf("tail ignored %d, want 7", res.TailIgnored)
	}
	v := res.View
	if v.Snapshot(1) == nil || v.Snapshot(2) == nil {
		t.Fatal("snapshots missing")
	}
	if v.Snapshot(2).Depth != 2 {
		t.Fatalf("depth = %d, want 2", v.Snapshot(2).Depth)
	}
	if r := v.Row(2, 1, 10); r == nil || r.ChangeType != fileformat.ChangeUpdate {
		t.Fatal("row resolution failed")
	}
	if r := v.Row(1, 1, 10); r == nil || r.ChangeType != fileformat.ChangeInsert {
		t.Fatal("snapshot 1 row wrong")
	}
	if b := v.Block(1); b == nil || b.DataOffset != 224 {
		t.Fatal("block resolution failed")
	}
	if got := v.RowKeys(1, 1); len(got) != 1 || got[0].RowID != 10 {
		t.Fatal("row keys failed")
	}
	if v.MemoryBytes() == 0 {
		t.Fatal("memory stats not tracked")
	}
}

func TestReplayParentChainValidation(t *testing.T) {
	// DELTA without its parent must stop replay.
	t1 := buildTxn(t, 1, deltaSnap(2, 1, 128, 4096), nil, nil, nil, 4096, 0)
	hdr := make([]byte, 128)
	data := append(hdr, t1...)
	res, err := Replay(data, 128, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Txns != 0 {
		t.Fatalf("orphan delta replayed %d txns", res.Txns)
	}
	if res.View.LatestSnapshot() != nil {
		t.Fatal("view should be empty")
	}

	// Depth limit.
	depth := uint32(2)
	view := EmptyView()
	seq := uint64(1)
	for i := uint64(1); i <= 4; i++ {
		var snap fileformat.SnapshotIndexEntry
		if i == 1 {
			snap = fullSnap(i, i*100, i*100+100)
		} else {
			snap = deltaSnap(i, i-1, i*100, i*100+100)
		}
		tx := buildTxn(t, seq, snap, nil, nil, nil, i*100+100, 0)
		parsed, err := ParseTxn(tx)
		if err != nil {
			t.Fatal(err)
		}
		nv, err := view.Apply(parsed, depth)
		if err != nil {
			if i == 3 { // depth limit hit at depth 3
				return
			}
			t.Fatalf("apply snapshot %d: %v", i, err)
		}
		view = nv
		seq++
	}
	t.Fatal("depth limit not enforced")
}

func TestReplayDuplicateSnapshotStops(t *testing.T) {
	t1 := buildTxn(t, 1, fullSnap(1, 128, 4096), nil, nil, nil, 4096, 0)
	t2 := buildTxn(t, 2, fullSnap(1, 8192, 12288), nil, nil, nil, 12288, 0) // duplicate snapshot 1
	hdr := make([]byte, 128)
	data := append(append(hdr, t1...), t2...)
	res, err := Replay(data, 128, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Txns != 1 {
		t.Fatalf("replayed %d txns, want 1 (duplicate stops)", res.Txns)
	}
	if res.View.Snapshot(1) == nil {
		t.Fatal("first snapshot missing")
	}
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
	if err != nil {
		t.Fatal(err)
	}
	v2, err := v1.Apply(t2, 4096)
	if err != nil {
		t.Fatal(err)
	}
	// v1 must not see snapshot 2.
	if v1.Snapshot(2) != nil {
		t.Fatal("v1 leaked snapshot 2 (shared map mutation)")
	}
	if v2.Snapshot(2) == nil {
		t.Fatal("v2 missing snapshot 2")
	}
	if v2.Row(1, 1, 1) == nil || v2.Row(2, 1, 2) == nil {
		t.Fatal("v2 row resolution failed")
	}
}
