package rowpack

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// ---- Create / Open error paths ----

func TestCreateOpenPathErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(dir+"/x.rpk", Options{}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Create with .rpk suffix: %v", err)
	}
	if _, err := Create(dir+"/x.rpi", Options{}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Create with .rpi suffix: %v", err)
	}
	if _, err := Open(dir+"/missing", Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open missing: %v", err)
	}
	// Create never overwrites.
	db, err := Create(dir+"/dup", Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Create(dir+"/dup", Options{}); err == nil {
		t.Fatal("Create over existing files succeeded")
	}
	// Reopening a healthy store exposes its identity.
	db2, err := Open(dir+"/dup", Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var nonzero bool
	for _, b := range db2.UUID() {
		if b != 0 {
			nonzero = true
		}
	}
	if !nonzero {
		t.Fatal("UUID should be non-zero after open")
	}
}

func TestOpenStoreMismatch(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(dir+"/a", Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(dir+"/b", Options{})
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close()
	// Pair a's data with b's index: UUIDs no longer match.
	data, err := os.ReadFile(dir + "/a.rpi")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/b.rpi", data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir+"/b", Options{}); !errors.Is(err, ErrStoreMismatch) {
		t.Fatalf("mismatched pair: %v", err)
	}
}

func TestOpenCorruptHeader(t *testing.T) {
	base := t.TempDir() + "/c"
	db, err := Create(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Corrupt the data header magic.
	raw, err := os.ReadFile(base + ".rpk")
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'X'
	if err := os.WriteFile(base+".rpk", raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(base, Options{}); err == nil {
		t.Fatal("Open accepted a corrupt data header")
	}
}

// ---- Read API error paths ----

func TestReadAPINotFoundErrors(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 3)

	// Unknown snapshot.
	if _, err := db.Get(context.Background(), 42, 1, 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown snapshot: %v", err)
	}
	if _, err := db.Snapshot(context.Background(), 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Snapshot unknown: %v", err)
	}
	if _, err := db.Scan(context.Background(), 42, 1, ScanOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Scan unknown snapshot: %v", err)
	}
	if _, err := db.Schema(context.Background(), 42, 1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Schema unknown snapshot: %v", err)
	}
	if _, err := db.LatestSchema(context.Background(), 42, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestSchema unknown snapshot: %v", err)
	}
	// Unknown row / table / schema version.
	if _, err := db.Get(context.Background(), full, 1, 99, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown row: %v", err)
	}
	if _, err := db.Get(context.Background(), full, 9, 1, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get unknown table: %v", err)
	}
	if _, err := db.Schema(context.Background(), full, 1, 7); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Schema unknown version: %v", err)
	}
	if _, err := db.LatestSchema(context.Background(), full, 9); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestSchema unknown table: %v", err)
	}
	// Invalid scan range.
	if _, err := db.Scan(context.Background(), full, 1, ScanOptions{StartRowID: 5, EndRowID: 5}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Scan empty range: %v", err)
	}
}

func TestReadAPITombstonesAndTables(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 3)

	w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(testSchema()); err != nil {
		t.Fatal(err)
	}
	if err := w.Delete(context.Background(), 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(context.Background(), 1, 10, 1, Row{Uint64(10), String("new")}); err != nil {
		t.Fatal(err)
	}
	delta, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// Tombstoned row: Get fails, Exists is false, and the parent still sees it.
	if _, err := db.Get(context.Background(), delta.ID, 1, 2, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get deleted row: %v", err)
	}
	if ok, err := db.Exists(context.Background(), delta.ID, 1, 2); ok || err != nil {
		t.Fatalf("Exists deleted row = %v, %v", ok, err)
	}
	if ok, err := db.Exists(context.Background(), full, 1, 2); !ok || err != nil {
		t.Fatalf("Exists at parent = %v, %v", ok, err)
	}
	if ok, err := db.Exists(context.Background(), delta.ID, 1, 99); ok || err != nil {
		t.Fatalf("Exists absent row = %v, %v", ok, err)
	}
	// Tables resolve along the parent chain.
	tables, err := db.Tables(context.Background(), delta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 1 || tables[0].ID != 1 || tables[0].Name != "t" || tables[0].LatestVersion != 1 {
		t.Fatalf("Tables at delta = %+v", tables)
	}
}

func TestClosedStoreErrors(t *testing.T) {
	db := newEmptyStore(t)
	commitOneFull(t, db, 1)
	db.Close()

	if _, err := db.Get(context.Background(), 1, 1, 1, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get closed: %v", err)
	}
	if _, err := db.Snapshot(context.Background(), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot closed: %v", err)
	}
	if _, err := db.Scan(context.Background(), 1, 1, ScanOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Scan closed: %v", err)
	}
	if _, err := db.Verify(context.Background(), VerifyQuick); !errors.Is(err, ErrClosed) {
		t.Fatalf("Verify closed: %v", err)
	}
	if st := db.Stats(); st.Snapshots != 0 || st.Blocks != 0 {
		t.Fatalf("Stats on closed store = %+v", st)
	}
	// Close is idempotent.
	if err := db.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
}

func TestIteratorLifecycle(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 10)

	it, err := db.Scan(context.Background(), full, 1, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		if id, _ := row[0].Uint64(); id != uint64(count+1) {
			t.Fatalf("row %d = %d", count, id)
		}
		if it.RowID() != uint64(count+1) {
			t.Fatalf("RowID = %d", it.RowID())
		}
		count++
	}
	if count != 10 || it.Err() != nil {
		t.Fatalf("scan count = %d err = %v", count, it.Err())
	}
	// Next after exhaustion and after Close stays false.
	if _, ok := it.Next(); ok {
		t.Fatal("Next after end returned a row")
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	if err := it.Close(); err != nil {
		t.Fatalf("double close: %v", err)
	}
	if _, ok := it.Next(); ok {
		t.Fatal("Next after Close returned a row")
	}

	// Range-bounded scan: EndRowID is exclusive.
	it2, err := db.Scan(context.Background(), full, 1, ScanOptions{StartRowID: 3, EndRowID: 6})
	if err != nil {
		t.Fatal(err)
	}
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
	if len(ids) != 3 || ids[0] != 3 || ids[2] != 5 {
		t.Fatalf("ranged scan = %v", ids)
	}

	// A cancelled context surfaces through Err().
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	it3, err := db.Scan(ctx, full, 1, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer it3.Close()
	if _, ok := it3.Next(); ok {
		t.Fatal("cancelled scan returned a row")
	}
	if !errors.Is(it3.Err(), context.Canceled) {
		t.Fatalf("cancelled scan Err = %v", it3.Err())
	}
}

// ---- Recovery internals ----

// appendFile appends raw bytes to path (store must be closed).
func appendFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		t.Fatal(err)
	}
}

func craftedSnapshotPair(t *testing.T, id uint64) []byte {
	t.Helper()
	var hdr, ftr [fileformat.SnapshotHeaderSize]byte
	sh := fileformat.SnapshotHeader{SnapshotType: fileformat.SnapshotFull, SnapshotID: id}
	if err := sh.MarshalTo(hdr[:]); err != nil {
		t.Fatal(err)
	}
	sf := fileformat.SnapshotFooter{SnapshotType: fileformat.SnapshotFull, SnapshotID: id}
	if err := sf.MarshalTo(ftr[:]); err != nil {
		t.Fatal(err)
	}
	return append(hdr[:], ftr[:]...)
}

func TestRecoveryGarbageTail(t *testing.T) {
	base := t.TempDir() + "/g"
	db := newEmptyStoreAt(t, base)
	commitOneFull(t, db, 5)
	db.Close()

	sizeBefore := fileSize(t, base+".rpk")
	appendFile(t, base+".rpk", bytes.Repeat([]byte{0xA5}, 512))

	db2, err := Open(base, Options{})
	if err != nil {
		t.Fatalf("reopen with garbage tail: %v", err)
	}
	defer db2.Close()
	st := db2.Stats()
	if !st.Recovery.Performed || st.Recovery.DataTailIgnored != 512 {
		t.Fatalf("recovery stats = %+v", st.Recovery)
	}
	if sz := fileSize(t, base+".rpk"); sz != sizeBefore {
		t.Fatalf("tail not truncated: %d -> %d", sizeBefore, sz)
	}
	// Data is intact.
	if _, err := db2.Get(context.Background(), 1, 1, 5, nil); err != nil {
		t.Fatalf("row 5 after recovery: %v", err)
	}
}

func TestRecoveryRebuildGhostSnapshot(t *testing.T) {
	base := t.TempDir() + "/ghost"
	db := newEmptyStoreAt(t, base)
	commitOneFull(t, db, 5)
	db.Close()

	// A data file with a committed snapshot the index never saw (e.g. the
	// index append was lost): header+footer pair with no blocks.
	appendFile(t, base+".rpk", craftedSnapshotPair(t, 99))

	db2, err := Open(base, Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	if got := db2.Stats().Recovery.SnapshotsRebuilt; got != 1 {
		t.Fatalf("SnapshotsRebuilt = %d, want 1", got)
	}
	snaps, err := db2.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[0].ID != 1 || snaps[1].ID != 99 {
		t.Fatalf("snapshots after rebuild = %+v", snaps)
	}
	if _, err := db2.Get(context.Background(), 1, 1, 5, nil); err != nil {
		t.Fatalf("row 5: %v", err)
	}
	// The rebuilt txn is persisted: a second reopen is stable and idempotent.
	db2.Close()
	db3, err := Open(base, Options{})
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer db3.Close()
	if got := db3.Stats().Recovery.SnapshotsRebuilt; got != 0 {
		t.Fatalf("second rebuild = %d, want 0", got)
	}
}

func TestRecoveryMidFileCorruption(t *testing.T) {
	base := t.TempDir() + "/mid"
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
	if err == nil || !strings.Contains(err.Error(), "mid-file corruption") {
		t.Fatalf("Open error = %v, want mid-file corruption", err)
	}
}

// ---- Verify ----

func TestVerifyModes(t *testing.T) {
	db := newEmptyStore(t)
	full := commitOneFull(t, db, 50)

	for _, mode := range []VerifyMode{VerifyQuick, VerifyFull} {
		rep, err := db.Verify(context.Background(), mode)
		if err != nil {
			t.Fatalf("Verify(%v): %v", mode, err)
		}
		if rep.SnapshotsChecked != 1 {
			t.Fatalf("SnapshotsChecked = %d", rep.SnapshotsChecked)
		}
		if rep.BlocksChecked == 0 {
			t.Fatalf("BlocksChecked = %d", rep.BlocksChecked)
		}
		if mode == VerifyFull && rep.RowsChecked != 50 {
			t.Fatalf("RowsChecked = %d, want 50", rep.RowsChecked)
		}
	}
	_ = full
}

// ---- RebuildIndex error paths ----

func TestRebuildIndexErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RebuildIndex(ctx, t.TempDir()+"/x", RebuildOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rebuild: %v", err)
	}
	if err := RebuildIndex(context.Background(), t.TempDir()+"/x.rpk", RebuildOptions{}); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("bad path rebuild: %v", err)
	}
	if err := RebuildIndex(context.Background(), t.TempDir()+"/missing", RebuildOptions{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing data rebuild: %v", err)
	}
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
		if err != nil || got != ct {
			t.Fatalf("roundtrip %d -> %q -> %d, %v", ct, name, got, err)
		}
	}
	// Unknown type strings are not guessed: the record is treated as plain
	// stored data and its table is skipped in the schema index.
	if _, err := columnType("bigint unsigned"); !errors.Is(err, errUnknownColumnType) {
		t.Fatalf("unknown type: %v", err)
	}
	if got := typeName(codec.Type(200)); got != "unknown" {
		t.Fatalf("typeName(unknown) = %q", got)
	}
}

func TestNullString(t *testing.T) {
	if nullString(true) != "YES" || nullString(false) != "NO" {
		t.Fatal("nullString wrong")
	}
}

// ---- helpers ----

func newEmptyStoreAt(t *testing.T, base string) *Store {
	t.Helper()
	db, err := Create(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}
