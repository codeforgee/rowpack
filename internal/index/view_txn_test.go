package index

import (
	"encoding/binary"

	"bytes"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
)

func snapEntry(id, parent uint64, typ format.SnapshotType) format.SnapshotIndexEntry {
	return format.SnapshotIndexEntry{
		SnapshotID:       id,
		ParentSnapshotID: parent,
		SnapshotType:     typ,
		BlockCount:       1,
		RowRecordCount:   4,
		DataStart:        100,
		DataEnd:          200,
		CreatedUnixNano:  12345,
		DataFooterCRC32C: 77,
	}
}

func blockEntry(id, snap uint64, table uint32) format.BlockIndexEntry {
	return format.BlockIndexEntry{
		BlockID:     id,
		SnapshotID:  snap,
		TableID:     table,
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		DataOffset:  128,
		RawSize:     4096,
		StoredSize:  1024,
		ItemCount:   4,
		RawCRC32C:   99,
	}
}

func metaEntry(snap, obj uint64, rectype uint32) format.MetadataIndexEntry {
	return format.MetadataIndexEntry{
		SnapshotID:  snap,
		ObjectID:    obj,
		Revision:    2,
		RecordType:  rectype,
		BlockID:     1,
		ItemOrdinal: 3,
		Operation:   format.OperationUpsert,
		Critical:    true,
	}
}

func TestBuilderSetSnapshotValidation(t *testing.T) {
	b := NewBuilder(1)
	require.Error(t, b.SetSnapshot(snapEntry(0, 0, format.SnapshotFull)), "zero snapshot id should error")
	require.Error(t, b.SetSnapshot(snapEntry(1, 0, format.SnapshotType(9))), "bad snapshot type should error")
	err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull))
	require.NoError(t, err, "valid snapshot")
	require.Error(t, b.SetSnapshot(snapEntry(2, 0, format.SnapshotFull)), "second SetSnapshot should error")
}

func TestBuilderEntryOrderingAndMismatch(t *testing.T) {
	b := NewBuilder(1)
	require.Error(t, b.AddMetadata(metaEntry(1, 9, 1)), "AddMetadata before SetSnapshot should error")
	require.Error(t, b.AddBlock(blockEntry(1, 1, 1)), "AddBlock before SetSnapshot should error")
	require.Error(t, b.AddRow(riEntry(1, 1, 1, 0, format.ChangeInsert)), "AddRow before SetSnapshot should error")
	if err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	require.Error(t, b.AddMetadata(metaEntry(2, 9, 1)), "metadata snapshot mismatch should error")
	require.Error(t, b.AddBlock(blockEntry(1, 2, 1)), "block snapshot mismatch should error")
	require.Error(t, b.AddRow(format.RowIndexEntry{SnapshotID: 2}), "row snapshot mismatch should error")
	err := b.AddMetadata(metaEntry(1, 9, 1))
	require.NoError(t, err, "valid metadata entry")
	err = b.AddBlock(blockEntry(1, 1, 1))
	require.NoError(t, err, "valid block entry")
}

func TestBuilderRowDedup(t *testing.T) {
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	row := func() format.RowIndexEntry {
		return format.RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 7}
	}
	if err := b.AddRow(row()); err != nil {
		t.Fatal(err)
	}
	require.Error(t, b.AddRow(row()), "duplicate (table, row) should error with dedup on")
	b2 := NewBuilder(1)
	b2.SetRowDedup(false)
	if err := b2.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b2.AddRow(row()); err != nil {
		t.Fatal(err)
	}
	err := b2.AddRow(row())
	require.NoError(t, err, "duplicate should pass with dedup off")
}

func TestBuilderCountsAndReserve(t *testing.T) {
	b := NewBuilder(1)
	b.SetRowDedup(false)
	if err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	b.Reserve(4, 4, 4) // pre-allocation must not change observable counts
	meta, blocks, rows := b.Counts()
	if meta != 0 || blocks != 0 || rows != 0 {
		t.Fatalf("fresh counts = %d/%d/%d, want 0/0/0", meta, blocks, rows)
	}
	if err := b.AddMetadata(metaEntry(1, 9, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(blockEntry(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(format.RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 1}); err != nil {
		t.Fatal(err)
	}
	meta, blocks, rows = b.Counts()
	if meta != 1 || blocks != 1 || rows != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/1", meta, blocks, rows)
	}
}

func TestBuildParseTxnRoundtrip(t *testing.T) {
	b := NewBuilder(7)
	if err := b.SetSnapshot(snapEntry(3, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(metaEntry(3, 900, 5)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(blockEntry(11, 3, 1)); err != nil {
		t.Fatal(err)
	}
	for _, r := range riSeq(40, 10) {
		e := r
		e.SnapshotID = 3
		if err := b.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	data, txn, err := b.Build(BodyBounds{DataStart: 500, DataEnd: 900, TxnStart: 1000, TxnEnd: 2000}, 42)
	require.NoError(t, err, "Build")
	if txn.Header.SnapshotID != 3 || txn.Header.TxnSequence != 7 {
		t.Fatalf("header mismatch: %+v", txn.Header)
	}
	if txn.Header.MetadataEntryCount != 1 || txn.Header.BlockEntryCount != 1 || txn.Header.RowEntryCount != 40 {
		t.Fatalf("header counts: %+v", txn.Header)
	}
	if txn.Footer.DataFooterCRC32C != 42 || txn.Footer.TxnStartOffset != 1000 || txn.Footer.TxnEndOffset != 2000 {
		t.Fatalf("footer mismatch: %+v", txn.Footer)
	}

	parsed, err := ParseTxn(data, nil)
	require.NoError(t, err, "ParseTxn")
	if parsed.Header.TxnSequence != 7 || parsed.Header.SnapshotID != 3 {
		t.Fatalf("parsed header: %+v", parsed.Header)
	}
	if len(parsed.Metadata) != 1 || parsed.Metadata[0].ObjectID != 900 {
		t.Fatalf("parsed metadata: %+v", parsed.Metadata)
	}
	if len(parsed.Blocks) != 1 || parsed.Blocks[0].BlockID != 11 {
		t.Fatalf("parsed blocks: %+v", parsed.Blocks)
	}
	if len(parsed.Rows) != 40 {
		t.Fatalf("parsed rows: %d", len(parsed.Rows))
	}
	if parsed.Snapshot.SnapshotID != 3 || parsed.Snapshot.BlockCount != 1 {
		t.Fatalf("parsed snapshot: %+v", parsed.Snapshot)
	}
	if parsed.Footer.TxnSequence != 7 || parsed.Footer.BodyCRC32C == 0 {
		t.Fatalf("parsed footer: %+v", parsed.Footer)
	}

	// Parsing the same bytes again must be deterministic.
	parsed2, err := ParseTxn(data, nil)
	require.NoError(t, err, "ParseTxn")
	if !bytes.Equal(rowBytes(parsed.Rows[0]), rowBytes(parsed2.Rows[0])) {
		t.Fatal("repeated ParseTxn diverges")
	}
}

// rowBytes serializes a RowIndexEntry for test comparison.
func rowBytes(e format.RowIndexEntry) []byte {
	out := make([]byte, format.RowIndexEntrySize)
	if err := e.MarshalTo(out); err != nil {
		panic(err)
	}
	return out
}

func TestParseTxnErrors(t *testing.T) {
	build := func() []byte {
		t.Helper()
		b := NewBuilder(1)
		if err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
			t.Fatal(err)
		}
		for _, r := range riSeq(5, 5) {
			e := r
			e.SnapshotID = 1
			if err := b.AddRow(e); err != nil {
				t.Fatal(err)
			}
		}
		data, _, err := b.Build(BodyBounds{}, 0)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := build()

	if _, err := ParseTxn(nil, nil); err == nil {
		t.Fatal("nil input should error")
	}
	if _, err := ParseTxn(data[:format.IndexTxnHeaderSize+format.IndexTxnFooterSize-1], nil); err == nil {
		t.Fatal("short input should error")
	}
	if _, err := ParseTxn(append(append([]byte(nil), data...), 0x00), nil); err == nil {
		t.Fatal("trailing bytes should error")
	}

	// Bad header magic.
	bad := append([]byte(nil), data...)
	bad[0] ^= 0xFF
	if _, err := ParseTxn(bad, nil); err == nil {
		t.Fatal("bad header magic should error")
	}

	// Forged BodyBytes beyond the input.
	bad = append([]byte(nil), data...)
	var h format.IndexTxnHeader
	if err := h.Unmarshal(bad); err != nil {
		t.Fatal(err)
	}
	h.BodyBytes = 1 << 20
	h.MarshalTo(bad)
	if _, err := ParseTxn(bad, nil); err == nil {
		t.Fatal("body exceeding input should error")
	}

	// Header/footer snapshot id mismatch: re-marshal the footer with a
	// different snapshot id.
	bad = append([]byte(nil), data...)
	ftr := bad[len(bad)-format.IndexTxnFooterSize:]
	var f format.IndexTxnFooter
	if err := f.Unmarshal(ftr); err != nil {
		t.Fatal(err)
	}
	f.SnapshotID = 999
	if err := f.MarshalTo(ftr); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTxn(bad, nil); err == nil {
		t.Fatal("header/footer id mismatch should error")
	}

	// Corrupted body byte must break a chunk payload check.
	bad = append([]byte(nil), data...)
	bad[format.IndexTxnHeaderSize+10] ^= 0xFF
	if _, err := ParseTxn(bad, nil); err == nil {
		t.Fatal("corrupt body should error")
	}
}

func TestBuildStoredPlainMatchesBuild(t *testing.T) {
	b := NewBuilder(2)
	if err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(format.RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 1, BlockID: 1, ChangeType: format.ChangeInsert}); err != nil {
		t.Fatal(err)
	}
	var gotBodyLen int
	data, txn, err := b.BuildStored(nil, 0, func(bodyLen int) BodyBounds {
		gotBodyLen = bodyLen
		return BodyBounds{DataStart: 10, DataEnd: 20, TxnStart: 30, TxnEnd: 40}
	}, 55, 0)
	require.NoError(t, err, "BuildStored")
	if gotBodyLen <= 0 {
		t.Fatalf("resolveBounds bodyLen = %d", gotBodyLen)
	}
	if txn.dataStart != 10 || txn.dataEnd != 20 || txn.txnStart != 30 || txn.txnEnd != 40 {
		t.Fatalf("resolved bounds not captured: %+v", txn)
	}
	parsed, err := ParseTxn(data, nil)
	require.NoError(t, err, "ParseTxn")
	if parsed.Footer.TxnStartOffset != 30 || parsed.Footer.TxnEndOffset != 40 {
		t.Fatalf("footer bounds: %+v", parsed.Footer)
	}
	if parsed.Header.DataSnapshotStart != 10 || parsed.Header.DataSnapshotEnd != 20 {
		t.Fatalf("header bounds: %+v", parsed.Header)
	}
	require.Equal(t, uint32(55), parsed.Footer.DataFooterCRC32C, "data footer CRC: %+v", parsed.Footer)
}

// --- View accessors ----------------------------------------------------------

func TestViewAccessorsAndChain(t *testing.T) {
	// Snapshot 1 (FULL): table 1 rows 1..4 all inserts, block 1, meta obj 500.
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(1, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(blockEntry(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(metaEntry(1, 500, 9)); err != nil {
		t.Fatal(err)
	}
	for _, r := range []format.RowIndexEntry{
		riEntry(1, 1, 1, 0, format.ChangeInsert),
		riEntry(1, 2, 1, 1, format.ChangeInsert),
		riEntry(1, 3, 1, 2, format.ChangeInsert),
		riEntry(1, 4, 1, 3, format.ChangeInsert),
	} {
		e := r
		e.SnapshotID = 1
		if err := b.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	_, txn1, err := b.Build(BodyBounds{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := EmptyView().Apply(txn1, 32)
	require.NoError(t, err, "apply snap 1 full")

	// Snapshot 2 (DELTA, parent 1): delete row 3, insert row 5, block 2.
	b2 := NewBuilder(2)
	se := snapEntry(2, 1, format.SnapshotDelta)
	if err := b2.SetSnapshot(se); err != nil {
		t.Fatal(err)
	}
	if err := b2.AddBlock(blockEntry(2, 2, 1)); err != nil {
		t.Fatal(err)
	}
	for _, r := range []format.RowIndexEntry{
		riEntry(1, 3, 2, 0, format.ChangeDelete),
		riEntry(1, 5, 2, 1, format.ChangeInsert),
	} {
		e := r
		e.SnapshotID = 2
		if err := b2.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	_, txn2, err := b2.Build(BodyBounds{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := v1.Apply(txn2, 32)
	require.NoError(t, err, "apply snap 2")

	if v1.Snapshot(1) == nil || v1.Snapshot(99) != nil {
		t.Fatal("Snapshot lookup broken")
	}
	if got := v2.LatestSnapshot(); got == nil || got.ID != 2 {
		t.Fatalf("LatestSnapshot = %+v", got)
	}
	snaps := v2.Snapshots()
	if len(snaps) != 2 || snaps[0].ID != 1 || snaps[1].ID != 2 {
		t.Fatalf("Snapshots not sorted: %+v", snaps)
	}
	if v2.Block(1) == nil || v2.Block(2) == nil || v2.Block(3) != nil {
		t.Fatal("Block lookup broken")
	}
	if len(v2.Blocks()) != 2 {
		t.Fatalf("Blocks len %d", len(v2.Blocks()))
	}
	if v2.MemoryBytes() == 0 {
		t.Fatal("MemoryBytes must be positive after apply")
	}

	// Row lookups: snapshot 2 sees its own rows; tombstone surfaces as DELETE.
	if loc, ok := v2.Row(2, 1, 3); !ok || loc.ChangeType != format.ChangeDelete {
		t.Fatalf("Row(2,1,3) = %+v %v", loc, ok)
	}
	if _, ok := v2.Row(2, 1, 1); ok {
		t.Fatal("row 1 must not live in snapshot 2's own layer")
	}
	// ResolveRow walks the parent chain.
	loc, ok := v2.ResolveRow(2, 1, 1)
	if !ok || loc.BlockID != 1 || loc.ItemOrdinal != 0 {
		t.Fatalf("ResolveRow(2,1,1) = %+v %v", loc, ok)
	}
	if _, ok := v2.ResolveRow(2, 1, 99); ok {
		t.Fatal("ResolveRow of unknown row should miss")
	}
	if _, ok := v1.ResolveRow(1, 1, 99); ok {
		t.Fatal("ResolveRow at detached snapshot should miss")
	}

	// Metadata: entry belongs to snapshot 1's txn and is inherited by the
	// copied map; unknown objects miss.
	if m := v2.Metadata(1, 500); m == nil || m.ObjectID != 500 || m.RecordType != 9 {
		t.Fatalf("Metadata = %+v", m)
	}
	if v2.Metadata(2, 501) != nil {
		t.Fatal("Metadata of unknown object should be nil")
	}
	if ids := v2.MetadataByType(1, 9); len(ids) != 1 || ids[0] != 500 {
		t.Fatalf("MetadataByType = %v", ids)
	}

	// RowIter over snapshot 2's own layer: rows 3 (del) and 5.
	it := v2.RowIter(2, 1)
	if it == nil {
		t.Fatal("RowIter(2,1) should not be nil")
	}
	if it.Len() != 2 {
		t.Fatalf("iter len %d, want 2", it.Len())
	}
	var got []uint64
	for ; !it.Done(); it.Next() {
		got = append(got, it.RowID())
		_ = it.Loc()
	}
	if len(got) != 2 || got[0] != 3 || got[1] != 5 {
		t.Fatalf("iteration order %v, want [3 5]", got)
	}
	if v2.RowIter(2, 999) != nil {
		t.Fatal("RowIter of unknown table should be nil")
	}

	// LogicalRowCount: snapshot 2 = rows {1,2,4} from parent + {5} - tombstone(3).
	require.Equal(t, uint64(4), v2.LogicalRowCount(2, 1), "LogicalRowCount = %d, want 4", v2.LogicalRowCount(2, 1))
	require.Equal(t, uint64(4), v1.LogicalRowCount(1, 1), "LogicalRowCount snap1 = %d, want 4", v1.LogicalRowCount(1, 1))
	require.Equal(t, uint64(0), v2.LogicalRowCount(2, 999), "LogicalRowCount unknown table = %d", v2.LogicalRowCount(2, 999))

	// Original views stay immutable: snapshot 1's layer is untouched by the
	// second apply (row 3 remains a plain INSERT there).
	if loc, ok := v1.Row(1, 1, 3); !ok || loc.ChangeType != format.ChangeInsert {
		t.Fatalf("v1 mutated: Row(1,1,3) = %+v %v", loc, ok)
	}
}

func TestApplyStreamingWithBlocksAndMeta(t *testing.T) {
	// A txn carrying block + metadata entries through the streaming apply
	// path must produce the same view as the buffered apply.
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(4, 0, format.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(blockEntry(21, 4, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(metaEntry(4, 700, 3)); err != nil {
		t.Fatal(err)
	}
	for _, r := range riSeq(10, 5) {
		e := r
		e.SnapshotID = 4
		if err := b.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	data, txn, err := b.Build(BodyBounds{}, 0)
	if err != nil {
		t.Fatal(err)
	}

	buffered, err := EmptyView().Apply(txn, 32)
	require.NoError(t, err, "buffered apply")
	streamed, err := EmptyView().ApplyStreaming(data, nil, 32)
	require.NoError(t, err, "streaming apply")

	if streamed.Block(21) == nil || streamed.Metadata(4, 700) == nil {
		t.Fatalf("streaming apply lost block/meta entries: block=%v meta=%v",
			streamed.Block(21) != nil, streamed.Metadata(4, 700) != nil)
	}
	compareRowShards(t, buffered, streamed)
	require.Equal(t, uint64(10), streamed.LogicalRowCount(4, 1), "streaming logical rows = %d, want 10", streamed.LogicalRowCount(4, 1))
}

// --- misc helpers ------------------------------------------------------------

func TestSortHelpers(t *testing.T) {
	u := []uint64{3, 1, 2}
	sortU64s(u)
	if u[0] != 1 || u[1] != 2 || u[2] != 3 {
		t.Fatalf("sortU64s = %v", u)
	}

	locs := []RowKeyLoc{
		{RowID: 3}, {RowID: 1}, {RowID: 2},
	}
	sortKeys(locs)
	if locs[0].RowID != 1 || locs[2].RowID != 3 {
		t.Fatalf("sortKeys = %v", locs)
	}

	metas := []*SnapshotMeta{{ID: 5}, {ID: 2}, {ID: 9}}
	sortSnapshots(metas)
	if metas[0].ID != 2 || metas[2].ID != 9 {
		t.Fatalf("sortSnapshots = %v", metas)
	}
}

func TestDecodeIndexPage(t *testing.T) {
	rows := riSeq(64, 8)
	page, n, _, _, err := encodePage(rows, indexPageEntryCount)
	require.NoError(t, err, "encode")
	if n != len(rows) {
		t.Fatalf("entryCount = %d, want %d", n, len(rows))
	}
	got, err := decodePage(page)
	require.NoError(t, err, "decodePage")
	if !rowsEq(got, rows) {
		t.Fatalf("roundtrip mismatch: got %d rows, want %d", len(got), len(rows))
	}

	if _, err := decodePage(nil); err == nil {
		t.Fatal("nil page should error")
	}
	if _, err := decodePage([]byte{1, 2, 3}); err == nil {
		t.Fatal("garbage page should error")
	}
}

// TestParseChunkStoredBytesOverrunRejected: a chunk header whose StoredBytes
// reach past the end of the txn body must be rejected with an error, not
// panic on the payload slice.
func TestParseChunkStoredBytesOverrunRejected(t *testing.T) {
	b := NewBuilder(7)
	if err := b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(format.MetadataIndexEntry{SnapshotID: 1, ObjectID: 3, Revision: 1, RecordType: 2}); err != nil {
		t.Fatal(err)
	}
	out, _, err := b.Build(BodyBounds{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Patch the second chunk (metadata, zstd-compressed so CheckLimits does
	// not pin StoredBytes == RawBytes) with a StoredBytes past the body end,
	// then recompute the chunk header CRC (covers the 64-byte header with
	// bytes 44..47 zeroed).
	pos := format.IndexTxnHeaderSize
	for i := 0; i < 1; i++ {
		var h format.IndexChunkHeader
		if err := h.Unmarshal(out[pos:]); err != nil {
			t.Fatal(err)
		}
		pos += format.IndexChunkHeaderSize + int(h.StoredBytes)
	}
	binary.LittleEndian.PutUint32(out[pos+32:], 1<<16)
	for j := 48; j < 64; j++ {
		out[pos+j] = 0
	}
	c := format.CRC32C(out[pos : pos+64])
	binary.LittleEndian.PutUint32(out[pos+44:], c)
	if _, err := ParseTxn(out, nil); err == nil {
		t.Fatal("chunk stored bytes overrun = nil error")
	}
}
