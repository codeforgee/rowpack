package rowpack

// Probe tests for code paths that the coverage report showed as uncovered.
// Each test exercises a specific gap; a failure here means the gap was hiding
// a real bug.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// ---- PeekHeader (header.go — every branch was uncovered) ----

func TestPeekHeaderPlainStore(t *testing.T) {
	base := filepath.Join(tmpdb(t), "plain")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h, err := PeekHeader(base)
	require.NoError(t, err)
	require.False(t, h.Encrypted)
	require.Equal(t, "", h.KeyID)
	require.False(t, h.CreatedAt.IsZero())
	// Cross-check UUID/CreatedAt against the raw on-disk header.
	raw, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	var dh format.DataFileHeader
	require.NoError(t, dh.Unmarshal(raw))
	require.Equal(t, [16]byte(h.UUID), dh.StoreUUID)
	require.Equal(t, h.CreatedAt, time.Unix(0, dh.CreatedUnixNano).UTC())
}

func TestPeekHeaderEncryptedStore(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc")
	db, err := Create(base, encOptions("peek-key"))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h, err := PeekHeader(base)
	require.NoError(t, err)
	require.True(t, h.Encrypted)
	require.Equal(t, "peek-key", h.KeyID)
}

func TestPeekHeaderMissingStore(t *testing.T) {
	base := filepath.Join(tmpdb(t), "absent")
	_, err := PeekHeader(base)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPeekHeaderGarbage(t *testing.T) {
	dir := tmpdb(t)
	p := filepath.Join(dir, "garbage.rpk")
	require.NoError(t, os.WriteFile(p, make([]byte, format.DataFileHeaderSize), 0o644))

	_, err := PeekHeader(filepath.Join(dir, "garbage"))
	require.Error(t, err) // bad magic; not a version error
	require.False(t, format.IsVersionError(err))
}

func TestPeekHeaderTruncated(t *testing.T) {
	dir := tmpdb(t)
	p := filepath.Join(dir, "short.rpk")
	require.NoError(t, os.WriteFile(p, []byte("RPK"), 0o644))

	_, err := PeekHeader(filepath.Join(dir, "short"))
	require.Error(t, err)
}

func TestPeekHeaderFutureMajorVersion(t *testing.T) {
	dir := tmpdb(t)
	// Build a valid plain store, then bump the major version bytes in place.
	base := filepath.Join(dir, "future")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	p := base + ".rpk"
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	raw[8] = 0x09 // major
	raw[9] = 0x00
	require.NoError(t, os.WriteFile(p, raw, 0o644))

	_, err = PeekHeader(base)
	require.ErrorIs(t, err, ErrVersionUnsupported)
}

// ---- datetime_tz through the Store (schema.go columnType/typeName,
// decodeFixedInto TZ fast path, values.go DateTimeTZ — all uncovered) ----

func TestDateTimeTZStoreRoundTrip(t *testing.T) {
	db := testDB(t, Options{BlockSize: 1024})
	ctx := context.Background()

	orig := time.Date(2026, 8, 6, 15, 4, 5, 123456789, time.FixedZone("+08", 8*3600))
	origUTC := time.Date(2026, 8, 6, 15, 4, 5, 987654321, time.UTC)

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	// Non-nullable column forces the fixed-width fast decode path.
	require.NoError(t, w.DefineTable("tz", []Column{
		{Name: "a", Type: TypeDateTimeTZ},
		{Name: "b", Type: TypeDateTimeTZ, Nullable: true},
	}))
	require.NoError(t, w.Insert(ctx, "tz", 1, Row{DateTimeTZ(orig), DateTimeTZ(origUTC)}))
	require.NoError(t, w.Insert(ctx, "tz", 2, Row{DateTimeTZ(orig), Null()}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	// Get path.
	row, err := db.Get(ctx, snap, "tz", 1, nil)
	require.NoError(t, err)
	got, ok := row[0].DateTimeTZValue()
	require.True(t, ok)
	require.True(t, got.Equal(orig), "tz instant changed: %v -> %v", orig, got)
	_, off := got.Zone()
	require.Equal(t, 8*3600, off)
	require.Equal(t, 123456789, got.Nanosecond())

	gotB, ok := row[1].DateTimeTZValue()
	require.True(t, ok)
	require.True(t, gotB.Equal(origUTC))

	// NULL round trip.
	row, err = db.Get(ctx, snap, "tz", 2, nil)
	require.NoError(t, err)
	_, ok = row[1].DateTimeTZValue()
	require.False(t, ok, "NULL must not read as a time")

	// Batch path (DecodeBatchInto -> decodeFixedInto / fastDecode).
	rows, err := db.ReadBatch(ctx, snap, "tz", []RowID{1, 2})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	got, ok = rows[0][0].DateTimeTZValue()
	require.True(t, ok)
	require.True(t, got.Equal(orig))

	// Schema-level type mapping (typeName -> metadata -> columnType).
	sc, err := db.Schema(ctx, snap, "tz", 0)
	require.NoError(t, err)
	require.Equal(t, TypeDateTimeTZ, sc.Columns[0].Type)
}

// ---- Store.Schema version 0 resolves to the latest version ----

func TestSchemaVersionZeroResolvesLatest(t *testing.T) {
	db := testDB(t, Options{BlockSize: 1024})
	ctx := context.Background()

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1)}))
	snap1, err := w.Commit(ctx)
	require.NoError(t, err)

	// A DELTA that inherits the schema without redefining it.
	w, err = db.Begin(ctx, snap1)
	require.NoError(t, err)
	require.NoError(t, w.Insert(ctx, "t", 2, Row{Int64(2)}))
	snap2, err := w.Commit(ctx)
	require.NoError(t, err)

	sc1, err := db.Schema(ctx, snap1, "t", 0)
	require.NoError(t, err)
	require.Len(t, sc1.Columns, 1)

	sc2, err := db.Schema(ctx, snap2, "t", 0)
	require.NoError(t, err)
	require.Len(t, sc2.Columns, 1, "version 0 must resolve to the table's latest schema")

	// Explicit version 1 matches the resolved one.
	scExplicit, err := db.Schema(ctx, snap2, "t", 1)
	require.NoError(t, err)
	require.Len(t, scExplicit.Columns, 1)
}

// ---- codec Value Or-accessors (values.go defaults — uncovered) ----

func TestValueOrAccessors(t *testing.T) {
	require.Equal(t, true, Null().BoolOr(true))
	require.Equal(t, false, Bool(false).BoolOr(true))
	require.Equal(t, int64(7), Null().Int64Or(7))
	require.Equal(t, int64(7), Int64(7).Int64Or(1))
	require.Equal(t, uint64(7), Null().Uint64Or(7))
	require.Equal(t, float32(1.5), Null().Float32Or(1.5))
	require.Equal(t, 2.5, Null().Float64Or(2.5))
	require.Equal(t, "d", Null().StringOr("d"))
	require.Equal(t, "s", String("s").StringOr("d"))
	require.Equal(t, []byte("d"), Null().BytesOr([]byte("d")))
	fallback := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	require.True(t, Null().DateTimeOr(fallback).Equal(fallback))
	require.True(t, Null().DateTimeTZOr(fallback).Equal(fallback))
	// Or-forms also default on type mismatch, not just NULL.
	require.Equal(t, int64(9), String("x").Int64Or(9))
}
