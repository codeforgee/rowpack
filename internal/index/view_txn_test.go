package index

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// --- fixtures ---------------------------------------------------------------

func snapEntry(id, parent uint64, typ fileformat.SnapshotType) fileformat.SnapshotIndexEntry {
	return fileformat.SnapshotIndexEntry{
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

func blockEntry(id, snap uint64, table uint32) fileformat.BlockIndexEntry {
	return fileformat.BlockIndexEntry{
		BlockID:     id,
		SnapshotID:  snap,
		TableID:     table,
		BlockKind:   fileformat.BlockKindRows,
		Compression: fileformat.CompressionZstd,
		DataOffset:  128,
		RawSize:     4096,
		StoredSize:  1024,
		ItemCount:   4,
		RawCRC32C:   99,
	}
}

func metaEntry(snap, obj uint64, rectype uint32) fileformat.MetadataIndexEntry {
	return fileformat.MetadataIndexEntry{
		SnapshotID:  snap,
		ObjectID:    obj,
		Revision:    2,
		RecordType:  rectype,
		BlockID:     1,
		ItemOrdinal: 3,
		Operation:   fileformat.OperationUpsert,
		Critical:    true,
	}
}

func snapWithRows(id, parent uint64, rows []fileformat.RowIndexEntry) *Txn {
	b := NewBuilder(id)
	b.SetRowDedup(false)
	if err := b.SetSnapshot(snapEntry(id, parent, fileformat.SnapshotFull)); err != nil {
		panic(err)
	}
	for _, r := range rows {
		e := r
		e.SnapshotID = id
		if err := b.AddRow(e); err != nil {
			panic(err)
		}
	}
	_, txn, err := b.Build(0, 0, 0, 0, 0)
	if err != nil {
		panic(err)
	}
	return txn
}

// --- Builder validation ------------------------------------------------------

func TestBuilderSetSnapshotValidation(t *testing.T) {
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(0, 0, fileformat.SnapshotFull)); err == nil {
		t.Fatal("zero snapshot id should error")
	}
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotType(9))); err == nil {
		t.Fatal("bad snapshot type should error")
	}
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
		t.Fatalf("valid snapshot: %v", err)
	}
	if err := b.SetSnapshot(snapEntry(2, 0, fileformat.SnapshotFull)); err == nil {
		t.Fatal("second SetSnapshot should error")
	}
}

func TestBuilderEntryOrderingAndMismatch(t *testing.T) {
	b := NewBuilder(1)
	if err := b.AddMetadata(metaEntry(1, 9, 1)); err == nil {
		t.Fatal("AddMetadata before SetSnapshot should error")
	}
	if err := b.AddBlock(blockEntry(1, 1, 1)); err == nil {
		t.Fatal("AddBlock before SetSnapshot should error")
	}
	if err := b.AddRow(riEntry(1, 1, 1, 0, fileformat.ChangeInsert)); err == nil {
		t.Fatal("AddRow before SetSnapshot should error")
	}
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(metaEntry(2, 9, 1)); err == nil {
		t.Fatal("metadata snapshot mismatch should error")
	}
	if err := b.AddBlock(blockEntry(1, 2, 1)); err == nil {
		t.Fatal("block snapshot mismatch should error")
	}
	if err := b.AddRow(fileformat.RowIndexEntry{SnapshotID: 2}); err == nil {
		t.Fatal("row snapshot mismatch should error")
	}
	if err := b.AddMetadata(metaEntry(1, 9, 1)); err != nil {
		t.Fatalf("valid metadata entry: %v", err)
	}
	if err := b.AddBlock(blockEntry(1, 1, 1)); err != nil {
		t.Fatalf("valid block entry: %v", err)
	}
}

func TestBuilderRowDedup(t *testing.T) {
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	row := func() fileformat.RowIndexEntry {
		return fileformat.RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 7}
	}
	if err := b.AddRow(row()); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(row()); err == nil {
		t.Fatal("duplicate (table, row) should error with dedup on")
	}
	b2 := NewBuilder(1)
	b2.SetRowDedup(false)
	if err := b2.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b2.AddRow(row()); err != nil {
		t.Fatal(err)
	}
	if err := b2.AddRow(row()); err != nil {
		t.Fatalf("duplicate should pass with dedup off: %v", err)
	}
}

func TestBuilderCountsAndReserve(t *testing.T) {
	b := NewBuilder(1)
	b.SetRowDedup(false)
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
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
	if err := b.AddRow(fileformat.RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 1}); err != nil {
		t.Fatal(err)
	}
	meta, blocks, rows = b.Counts()
	if meta != 1 || blocks != 1 || rows != 1 {
		t.Fatalf("counts = %d/%d/%d, want 1/1/1", meta, blocks, rows)
	}
}

// --- Build / ParseTxn --------------------------------------------------------

func TestBuildParseTxnRoundtrip(t *testing.T) {
	b := NewBuilder(7)
	if err := b.SetSnapshot(snapEntry(3, 0, fileformat.SnapshotFull)); err != nil {
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
	data, txn, err := b.Build(500, 900, 42, 1000, 2000)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if txn.Header.SnapshotID != 3 || txn.Header.TxnSequence != 7 {
		t.Fatalf("header mismatch: %+v", txn.Header)
	}
	if txn.Header.MetadataEntryCount != 1 || txn.Header.BlockEntryCount != 1 || txn.Header.RowEntryCount != 40 {
		t.Fatalf("header counts: %+v", txn.Header)
	}
	if txn.Footer.DataFooterCRC32C != 42 || txn.Footer.TxnStartOffset != 1000 || txn.Footer.TxnEndOffset != 2000 {
		t.Fatalf("footer mismatch: %+v", txn.Footer)
	}

	parsed, err := ParseTxn(data)
	if err != nil {
		t.Fatalf("ParseTxn: %v", err)
	}
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

	// ParseTxnChunked without crypto must behave identically.
	parsed2, err := ParseTxnChunked(data, nil)
	if err != nil {
		t.Fatalf("ParseTxnChunked: %v", err)
	}
	if !bytes.Equal(rowBytes(parsed.Rows[0]), rowBytes(parsed2.Rows[0])) {
		t.Fatal("ParseTxnChunked result diverges from ParseTxn")
	}
}

// rowBytes serializes a RowIndexEntry for test comparison.
func rowBytes(e fileformat.RowIndexEntry) []byte {
	out := make([]byte, fileformat.RowIndexEntrySize)
	if err := e.MarshalTo(out); err != nil {
		panic(err)
	}
	return out
}

func TestParseTxnErrors(t *testing.T) {
	build := func() []byte {
		t.Helper()
		b := NewBuilder(1)
		if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
			t.Fatal(err)
		}
		for _, r := range riSeq(5, 5) {
			e := r
			e.SnapshotID = 1
			if err := b.AddRow(e); err != nil {
				t.Fatal(err)
			}
		}
		data, _, err := b.Build(0, 0, 0, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	data := build()

	if _, err := ParseTxn(nil); err == nil {
		t.Fatal("nil input should error")
	}
	if _, err := ParseTxn(data[:fileformat.IndexTxnHeaderSize+fileformat.IndexTxnFooterSize-1]); err == nil {
		t.Fatal("short input should error")
	}
	if _, err := ParseTxn(append(append([]byte(nil), data...), 0x00)); err == nil {
		t.Fatal("trailing bytes should error")
	}

	// Bad header magic.
	bad := append([]byte(nil), data...)
	bad[0] ^= 0xFF
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("bad header magic should error")
	}

	// Forged BodyBytes beyond the input.
	bad = append([]byte(nil), data...)
	var h fileformat.IndexTxnHeader
	if err := h.Unmarshal(bad); err != nil {
		t.Fatal(err)
	}
	h.BodyBytes = 1 << 20
	h.MarshalTo(bad)
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("body exceeding input should error")
	}

	// Header/footer snapshot id mismatch: re-marshal the footer with a
	// different snapshot id.
	bad = append([]byte(nil), data...)
	ftr := bad[len(bad)-fileformat.IndexTxnFooterSize:]
	var f fileformat.IndexTxnFooter
	if err := f.Unmarshal(ftr); err != nil {
		t.Fatal(err)
	}
	f.SnapshotID = 999
	if err := f.MarshalTo(ftr); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("header/footer id mismatch should error")
	}

	// Corrupted body byte must break a chunk payload check.
	bad = append([]byte(nil), data...)
	bad[fileformat.IndexTxnHeaderSize+10] ^= 0xFF
	if _, err := ParseTxn(bad); err == nil {
		t.Fatal("corrupt body should error")
	}
}

func TestBuildStoredPlainMatchesBuild(t *testing.T) {
	b := NewBuilder(2)
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddRow(fileformat.RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 1, BlockID: 1, ChangeType: fileformat.ChangeInsert}); err != nil {
		t.Fatal(err)
	}
	var gotBodyLen int
	data, txn, err := b.BuildStored(nil, 0, func(bodyLen int) (uint64, uint64, int64, int64) {
		gotBodyLen = bodyLen
		return 10, 20, 30, 40
	}, 55, 0)
	if err != nil {
		t.Fatalf("BuildStored: %v", err)
	}
	if gotBodyLen <= 0 {
		t.Fatalf("resolveBounds bodyLen = %d", gotBodyLen)
	}
	if txn.dataStart != 10 || txn.dataEnd != 20 || txn.txnStart != 30 || txn.txnEnd != 40 {
		t.Fatalf("resolved bounds not captured: %+v", txn)
	}
	parsed, err := ParseTxn(data)
	if err != nil {
		t.Fatalf("ParseTxn: %v", err)
	}
	if parsed.Footer.TxnStartOffset != 30 || parsed.Footer.TxnEndOffset != 40 {
		t.Fatalf("footer bounds: %+v", parsed.Footer)
	}
	if parsed.Header.DataSnapshotStart != 10 || parsed.Header.DataSnapshotEnd != 20 {
		t.Fatalf("header bounds: %+v", parsed.Header)
	}
	if parsed.Footer.DataFooterCRC32C != 55 {
		t.Fatalf("data footer CRC: %+v", parsed.Footer)
	}
}

// --- View accessors ----------------------------------------------------------

func TestViewAccessorsAndChain(t *testing.T) {
	// Snapshot 1 (FULL): table 1 rows 1..4 all inserts, block 1, meta obj 500.
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(1, 0, fileformat.SnapshotFull)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(blockEntry(1, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(metaEntry(1, 500, 9)); err != nil {
		t.Fatal(err)
	}
	for _, r := range []fileformat.RowIndexEntry{
		riEntry(1, 1, 1, 0, fileformat.ChangeInsert),
		riEntry(1, 2, 1, 1, fileformat.ChangeInsert),
		riEntry(1, 3, 1, 2, fileformat.ChangeInsert),
		riEntry(1, 4, 1, 3, fileformat.ChangeInsert),
	} {
		e := r
		e.SnapshotID = 1
		if err := b.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	_, txn1, err := b.Build(0, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := EmptyView().Apply(txn1, 32)
	if err != nil {
		t.Fatalf("apply snap 1 full: %v", err)
	}

	// Snapshot 2 (DELTA, parent 1): delete row 3, insert row 5, block 2.
	b2 := NewBuilder(2)
	se := snapEntry(2, 1, fileformat.SnapshotDelta)
	if err := b2.SetSnapshot(se); err != nil {
		t.Fatal(err)
	}
	if err := b2.AddBlock(blockEntry(2, 2, 1)); err != nil {
		t.Fatal(err)
	}
	for _, r := range []fileformat.RowIndexEntry{
		riEntry(1, 3, 2, 0, fileformat.ChangeDelete),
		riEntry(1, 5, 2, 1, fileformat.ChangeInsert),
	} {
		e := r
		e.SnapshotID = 2
		if err := b2.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	_, txn2, err := b2.Build(0, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := v1.Apply(txn2, 32)
	if err != nil {
		t.Fatalf("apply snap 2: %v", err)
	}

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
	if loc, ok := v2.Row(2, 1, 3); !ok || loc.ChangeType != fileformat.ChangeDelete {
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
	if n := v2.LogicalRowCount(2, 1); n != 4 {
		t.Fatalf("LogicalRowCount = %d, want 4", n)
	}
	if n := v1.LogicalRowCount(1, 1); n != 4 {
		t.Fatalf("LogicalRowCount snap1 = %d, want 4", n)
	}
	if n := v2.LogicalRowCount(2, 999); n != 0 {
		t.Fatalf("LogicalRowCount unknown table = %d", n)
	}

	// Original views stay immutable: snapshot 1's layer is untouched by the
	// second apply (row 3 remains a plain INSERT there).
	if loc, ok := v1.Row(1, 1, 3); !ok || loc.ChangeType != fileformat.ChangeInsert {
		t.Fatalf("v1 mutated: Row(1,1,3) = %+v %v", loc, ok)
	}
}

func TestApplyStreamingWithBlocksAndMeta(t *testing.T) {
	// A txn carrying block + metadata entries through the streaming apply
	// path must produce the same view as the buffered apply.
	b := NewBuilder(1)
	if err := b.SetSnapshot(snapEntry(4, 0, fileformat.SnapshotFull)); err != nil {
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
	data, txn, err := b.Build(0, 0, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	buffered, err := EmptyView().Apply(txn, 32)
	if err != nil {
		t.Fatalf("buffered apply: %v", err)
	}
	streamed, err := EmptyView().ApplyStreaming(data, nil, 32)
	if err != nil {
		t.Fatalf("streaming apply: %v", err)
	}

	if streamed.Block(21) == nil || streamed.Metadata(4, 700) == nil {
		t.Fatalf("streaming apply lost block/meta entries: block=%v meta=%v",
			streamed.Block(21) != nil, streamed.Metadata(4, 700) != nil)
	}
	compareRowShards(t, buffered, streamed)
	if n := streamed.LogicalRowCount(4, 1); n != 10 {
		t.Fatalf("streaming logical rows = %d, want 10", n)
	}
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
	sortRowKeyLocs(locs)
	if locs[0].RowID != 1 || locs[2].RowID != 3 {
		t.Fatalf("sortRowKeyLocs = %v", locs)
	}

	metas := []*SnapshotMeta{{ID: 5}, {ID: 2}, {ID: 9}}
	sortSnapshotMetas(metas)
	if metas[0].ID != 2 || metas[2].ID != 9 {
		t.Fatalf("sortSnapshotMetas = %v", metas)
	}
}

func TestDecodeIndexPageExported(t *testing.T) {
	rows := riSeq(64, 8)
	page, n, _, _, err := encodeRowIndexPage(rows, indexPageEntryCount)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if n != len(rows) {
		t.Fatalf("entryCount = %d, want %d", n, len(rows))
	}
	got, err := DecodeIndexPage(page)
	if err != nil {
		t.Fatalf("DecodeIndexPage: %v", err)
	}
	if !rowsEq(got, rows) {
		t.Fatalf("roundtrip mismatch: got %d rows, want %d", len(got), len(rows))
	}

	if _, err := DecodeIndexPage(nil); err == nil {
		t.Fatal("nil page should error")
	}
	if _, err := DecodeIndexPage([]byte{1, 2, 3}); err == nil {
		t.Fatal("garbage page should error")
	}
}
