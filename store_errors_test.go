package rowpack

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// ---- Create / Open error paths ----

func TestCreateOpenPathErrors(t *testing.T) {
	dir := tmpdb(t)
	_, err := Create(dir+"/x.rpk", Options{})
	require.ErrorIs(t, err, ErrInvalidPath, "Create with .rpk suffix: %v", err)
	_, err = Create(dir+"/x.rpi", Options{})
	require.ErrorIs(t, err, ErrInvalidPath, "Create with .rpi suffix: %v", err)
	_, err = Open(dir+"/missing", Options{})
	require.ErrorIs(t, err, ErrNotFound, "Open missing: %v", err)
	// Create never overwrites.
	db, err := Create(dir+"/dup", Options{})
	require.NoError(t, err)
	db.Close()
	_, err = Create(dir+"/dup", Options{})
	require.Error(t, err, "Create over existing files succeeded")
	// Reopening a healthy store exposes its identity.
	db2, err := Open(dir+"/dup", Options{})
	require.NoError(t, err)
	defer db2.Close()
	var nonzero bool
	for _, b := range db2.UUID() {
		if b != 0 {
			nonzero = true
		}
	}
	require.True(t, nonzero, "UUID should be non-zero after open")
}

func TestOpenStoreMismatch(t *testing.T) {
	dir := tmpdb(t)
	a, err := Create(dir+"/a", Options{})
	require.NoError(t, err)
	b, err := Create(dir+"/b", Options{})
	require.NoError(t, err)
	a.Close()
	b.Close()
	// Pair a's data with b's index: UUIDs no longer match.
	data, err := os.ReadFile(dir + "/a.rpi")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dir+"/b.rpi", data, 0o644))
	_, err = Open(dir+"/b", Options{})
	require.ErrorIs(t, err, ErrStoreMismatch, "mismatched pair: %v", err)
}

func TestOpenCorruptHeader(t *testing.T) {
	base := tmpdb(t) + "/c"
	db, err := Create(base, Options{})
	require.NoError(t, err)
	db.Close()

	// Corrupt the data header magic.
	raw, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	raw[0] = 'X'
	require.NoError(t, os.WriteFile(base+".rpk", raw, 0o644))
	_, err = Open(base, Options{})
	require.Error(t, err, "Open accepted a corrupt data header")
}

// ---- Read API error paths ----

func TestReadAPINotFoundErrors(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 3)

	// Unknown snapshot.
	_, err := db.Get(context.Background(), 42, 1, 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "Get unknown snapshot: %v", err)
	_, err = db.Snapshot(context.Background(), 42)
	require.ErrorIs(t, err, ErrNotFound, "Snapshot unknown: %v", err)
	_, err = db.Scan(context.Background(), 42, 1, ScanOptions{})
	require.ErrorIs(t, err, ErrNotFound, "Scan unknown snapshot: %v", err)
	_, err = db.Schema(context.Background(), 42, 1, 1)
	require.ErrorIs(t, err, ErrNotFound, "Schema unknown snapshot: %v", err)
	_, err = db.LatestSchema(context.Background(), 42, 1)
	require.ErrorIs(t, err, ErrNotFound, "LatestSchema unknown snapshot: %v", err)
	// Unknown row / table / schema version.
	_, err = db.Get(context.Background(), full, 1, 99, nil)
	require.ErrorIs(t, err, ErrNotFound, "Get unknown row: %v", err)
	_, err = db.Get(context.Background(), full, 9, 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "Get unknown table: %v", err)
	_, err = db.Schema(context.Background(), full, 1, 7)
	require.ErrorIs(t, err, ErrSchemaMismatch, "Schema unknown version: %v", err)
	_, err = db.LatestSchema(context.Background(), full, 9)
	require.ErrorIs(t, err, ErrNotFound, "LatestSchema unknown table: %v", err)
	// Invalid scan range.
	_, err = db.Scan(context.Background(), full, 1, ScanOptions{StartRowID: 5, EndRowID: 5})
	require.ErrorIs(t, err, ErrInvalidArgument, "Scan empty range: %v", err)
}

func TestReadAPITombstonesAndTables(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 3)

	w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	require.NoError(t, w.Delete(context.Background(), 1, 2))
	require.NoError(t, w.Insert(context.Background(), 1, 10, 1, Row{Uint64(10), String("new")}))
	delta, err := w.Commit(context.Background())
	require.NoError(t, err)

	// Tombstoned row: Get fails, Exists is false, and the parent still sees it.
	_, err = db.Get(context.Background(), delta.ID, 1, 2, nil)
	require.ErrorIs(t, err, ErrNotFound, "Get deleted row: %v", err)
	ok, err := db.Exists(context.Background(), delta.ID, 1, 2)
	require.NoError(t, err)
	require.False(t, ok, "Exists deleted row = %v, %v", ok, err)
	ok, err = db.Exists(context.Background(), full, 1, 2)
	require.NoError(t, err)
	require.True(t, ok, "Exists at parent = %v, %v", ok, err)
	ok, err = db.Exists(context.Background(), delta.ID, 1, 99)
	require.NoError(t, err)
	require.False(t, ok, "Exists absent row = %v, %v", ok, err)
	// Tables resolve along the parent chain.
	tables, err := db.Tables(context.Background(), delta.ID)
	require.NoError(t, err)
	require.Len(t, tables, 1)
	require.Equal(t, uint32(1), tables[0].ID)
	require.Equal(t, "t", tables[0].Name)
	require.Equal(t, uint32(1), tables[0].LatestVersion)
}

func TestClosedStoreErrors(t *testing.T) {
	db := newEmptyStore(t)
	commitOneFull(t, db, 1)
	db.Close()

	_, err := db.Get(context.Background(), 1, 1, 1, nil)
	require.ErrorIs(t, err, ErrClosed, "Get closed: %v", err)
	_, err = db.Snapshot(context.Background(), 1)
	require.ErrorIs(t, err, ErrClosed, "Snapshot closed: %v", err)
	_, err = db.Scan(context.Background(), 1, 1, ScanOptions{})
	require.ErrorIs(t, err, ErrClosed, "Scan closed: %v", err)
	_, err = db.Verify(context.Background(), VerifyQuick)
	require.ErrorIs(t, err, ErrClosed, "Verify closed: %v", err)
	st := db.Stats()
	require.Zero(t, st.Snapshots, "Stats on closed store = %+v", st)
	require.Zero(t, st.Blocks, "Stats on closed store = %+v", st)
	// Close is idempotent.
	require.NoError(t, db.Close(), "double close: %v", err)
}

func TestIteratorLifecycle(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 10)

	it, err := db.Scan(context.Background(), full, 1, ScanOptions{})
	require.NoError(t, err)
	count := 0
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		id, _ := row[0].Uint64()
		require.Equal(t, uint64(count+1), id, "row %d = %d", count, id)
		require.Equal(t, uint64(count+1), uint64(it.RowID()), "RowID = %d", it.RowID())
		count++
	}
	require.Equal(t, 10, count, "scan count = %d err = %v", count, it.Err())
	require.NoError(t, it.Err(), "scan count = %d err = %v", count, it.Err())
	// Next after exhaustion and after Close stays false.
	_, ok := it.Next()
	require.False(t, ok, "Next after end returned a row")
	require.NoError(t, it.Close())
	require.NoError(t, it.Close(), "double close: %v", err)
	_, ok = it.Next()
	require.False(t, ok, "Next after Close returned a row")

	// Range-bounded scan: EndRowID is exclusive.
	it2, err := db.Scan(context.Background(), full, 1, ScanOptions{StartRowID: 3, EndRowID: 6})
	require.NoError(t, err)
	defer it2.Close()
	var ids []RowID
	for {
		row, ok := it2.Next()
		if !ok {
			break
		}
		id, _ := row[0].Uint64()
		ids = append(ids, RowID(id))
	}
	require.Len(t, ids, 3, "ranged scan = %v", ids)
	require.Equal(t, RowID(3), ids[0])
	require.Equal(t, RowID(5), ids[2])

	// A cancelled context surfaces through Err().
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it3, err := db.Scan(ctx, full, 1, ScanOptions{})
	require.NoError(t, err)
	defer it3.Close()
	_, ok = it3.Next()
	require.False(t, ok, "cancelled scan returned a row")
	require.ErrorIs(t, it3.Err(), context.Canceled, "cancelled scan Err = %v", it3.Err())
}

// ---- Recovery internals ----

// appendFile appends raw bytes to path (store must be closed).
func appendFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(raw)
	require.NoError(t, err)
}

func craftedSnapshotPair(t *testing.T, id uint64) []byte {
	t.Helper()
	var hdr, ftr [fileformat.SnapshotHeaderSize]byte
	sh := fileformat.SnapshotHeader{SnapshotType: fileformat.SnapshotFull, SnapshotID: id}
	require.NoError(t, sh.MarshalTo(hdr[:]))
	sf := fileformat.SnapshotFooter{SnapshotType: fileformat.SnapshotFull, SnapshotID: id}
	require.NoError(t, sf.MarshalTo(ftr[:]))
	return append(hdr[:], ftr[:]...)
}

func TestRecoveryGarbageTail(t *testing.T) {
	base := tmpdb(t) + "/g"
	db := newEmptyStoreAt(t, base)
	commitOneFull(t, db, 5)
	db.Close()

	sizeBefore := fileSize(t, base+".rpk")
	appendFile(t, base+".rpk", bytes.Repeat([]byte{0xA5}, 512))

	db2, err := Open(base, Options{})
	require.NoError(t, err, "reopen with garbage tail: %v", err)
	defer db2.Close()
	st := db2.Stats()
	require.True(t, st.Recovery.Performed, "recovery stats = %+v", st.Recovery)
	require.Equal(t, uint64(512), st.Recovery.DataTailIgnored, "recovery stats = %+v", st.Recovery)
	require.Equal(t, sizeBefore, fileSize(t, base+".rpk"), "tail not truncated: %d -> %d", sizeBefore, fileSize(t, base+".rpk"))
	// Data is intact.
	_, err = db2.Get(context.Background(), 1, 1, 5, nil)
	require.NoError(t, err, "row 5 after recovery: %v", err)
}

func TestRecoveryRebuildGhostSnapshot(t *testing.T) {
	base := tmpdb(t) + "/ghost"
	db := newEmptyStoreAt(t, base)
	commitOneFull(t, db, 5)
	db.Close()

	// A data file with a committed snapshot the index never saw (e.g. the
	// index append was lost): header+footer pair with no blocks.
	appendFile(t, base+".rpk", craftedSnapshotPair(t, 99))

	db2, err := Open(base, Options{})
	require.NoError(t, err, "reopen: %v", err)
	defer db2.Close()
	require.Equal(t, uint64(1), db2.Stats().Recovery.SnapshotsRebuilt, "SnapshotsRebuilt = %d, want 1", db2.Stats().Recovery.SnapshotsRebuilt)
	snaps, err := db2.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Len(t, snaps, 2, "snapshots after rebuild = %+v", snaps)
	require.Equal(t, uint64(1), snaps[0].ID)
	require.Equal(t, uint64(99), snaps[1].ID)
	_, err = db2.Get(context.Background(), 1, 1, 5, nil)
	require.NoError(t, err, "row 5: %v", err)
	// The rebuilt txn is persisted: a second reopen is stable and idempotent.
	db2.Close()
	db3, err := Open(base, Options{})
	require.NoError(t, err, "second reopen: %v", err)
	defer db3.Close()
	require.Zero(t, db3.Stats().Recovery.SnapshotsRebuilt, "second rebuild = %d, want 0", db3.Stats().Recovery.SnapshotsRebuilt)
}

func TestRecoveryMidFileCorruption(t *testing.T) {
	base := tmpdb(t) + "/mid"
	db := newEmptyStoreAt(t, base)
	commitOneFull(t, db, 5)
	db.Close()

	// Truncated snapshot at offset X, followed by a complete one: the
	// incomplete region is mid-file corruption, never silently skipped.
	raw := craftedSnapshotPair(t, 5) // valid header (walk starts), 96-byte footer acts as garbage
	broken := raw[:fileformat.SnapshotHeaderSize]
	broken = append(broken, bytes.Repeat([]byte{0xA5}, 256)...)
	broken = append(broken, craftedSnapshotPair(t, 99)...)
	appendFile(t, base+".rpk", broken)

	_, err := Open(base, Options{})
	require.Error(t, err, "Open error = %v, want mid-file corruption", err)
	require.Contains(t, err.Error(), "mid-file corruption", "Open error = %v, want mid-file corruption", err)
}

// ---- Verify ----

func TestVerifyModes(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 50)

	for _, mode := range []VerifyMode{VerifyQuick, VerifyFull} {
		rep, err := db.Verify(context.Background(), mode)
		require.NoError(t, err, "Verify(%v): %v", mode, err)
		require.Equal(t, uint64(1), rep.SnapshotsChecked, "SnapshotsChecked = %d", rep.SnapshotsChecked)
		require.NotZero(t, rep.BlocksChecked, "BlocksChecked = %d", rep.BlocksChecked)
		if mode == VerifyFull {
			require.Equal(t, uint64(50), rep.RowsChecked, "RowsChecked = %d, want 50", rep.RowsChecked)
		}
	}
	_ = full
}

// ---- RebuildIndex error paths ----

func TestRebuildIndexErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := RebuildIndex(ctx, tmpdb(t)+"/x", RebuildOptions{})
	require.ErrorIs(t, err, context.Canceled, "cancelled rebuild: %v", err)
	err = RebuildIndex(context.Background(), tmpdb(t)+"/x.rpk", RebuildOptions{})
	require.ErrorIs(t, err, ErrInvalidPath, "bad path rebuild: %v", err)
	err = RebuildIndex(context.Background(), tmpdb(t)+"/missing", RebuildOptions{})
	require.ErrorIs(t, err, ErrNotFound, "missing data rebuild: %v", err)
}

// ---- schema helpers ----

func TestColumnTypeAndNameRoundTrip(t *testing.T) {
	types := []codec.Type{
		codec.TypeBool, codec.TypeInt8, codec.TypeInt16, codec.TypeInt32, codec.TypeInt64,
		codec.TypeUint8, codec.TypeUint16, codec.TypeUint32, codec.TypeUint64,
		codec.TypeFloat32, codec.TypeFloat64, codec.TypeString, codec.TypeBytes,
		codec.TypeDate, codec.TypeTime, codec.TypeDateTime, codec.TypeDecimal,
	}
	for _, ct := range types {
		name := typeName(ct)
		got, err := columnType(name)
		require.NoError(t, err, "roundtrip %d -> %q -> %d, %v", ct, name, got, err)
		require.Equal(t, ct, got, "roundtrip %d -> %q -> %d, %v", ct, name, got, err)
	}
	// Unknown type strings are not guessed: the record is treated as plain
	// stored data and its table is skipped in the schema index.
	_, err := columnType("bigint unsigned")
	require.ErrorIs(t, err, errUnknownColumnType, "unknown type: %v", err)
	require.Equal(t, "unknown", typeName(codec.Type(200)), "typeName(unknown) = %q", typeName(codec.Type(200)))
}

func TestNullString(t *testing.T) {
	require.Equal(t, "YES", nullString(true))
	require.Equal(t, "NO", nullString(false))
}

// ---- helpers ----

func newEmptyStoreAt(t *testing.T, base string) *Store {
	t.Helper()
	db, err := Create(base, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Size()
}
