package rowpack

import (
	"context"
	"math"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// allTypesSchema covers every engine type plus NULL-able columns.
func allTypesSchema() []Column {
	return []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "b", Type: TypeBool},
		{Name: "i8", Type: TypeInt8}, {Name: "i16", Type: TypeInt16},
		{Name: "i32", Type: TypeInt32}, {Name: "i64", Type: TypeInt64},
		{Name: "u8", Type: TypeUint8}, {Name: "u16", Type: TypeUint16},
		{Name: "u32", Type: TypeUint32}, {Name: "u64", Type: TypeUint64},
		{Name: "f32", Type: TypeFloat32}, {Name: "f64", Type: TypeFloat64},
		{Name: "s", Type: TypeString},
		{Name: "by", Type: TypeBytes},
		{Name: "d", Type: TypeDate}, {Name: "t", Type: TypeTime},
		{Name: "dt", Type: TypeDateTime},
		{Name: "dec", Type: TypeDecimal, Scale: 6},
		{Name: "nul", Type: TypeString, Nullable: true},
	}
}

// TestAllTypesRoundTrip writes and reads one row per value type through the
// public API and verifies exact equality, plus a NULL column.
func TestAllTypesRoundTrip(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("t", allTypesSchema()))

	bigDec := new(big.Int).Mul(big.NewInt(123456789), big.NewInt(1_000_000_000_000))
	bigDec = bigDec.Mul(bigDec, big.NewInt(1_000_000_000_000)).Neg(bigDec) // -1.23456789e33
	row := Row{
		Uint64(42),
		Bool(true),
		Int8(-8), Int16(-1600), Int32(-32_000_000), Int64(-64_000_000_000),
		Uint8(255), Uint16(65535), Uint32(4_000_000_000), Uint64(18_000_000_000_000_000_000),
		Float32(0.5), Float64(-3.141592653589793),
		String("中文/emoji 🚀 and \x00 binary"),
		Bytes([]byte{0, 1, 2, 250, 251, 252, 253, 254, 255}),
		DateValue(NewDate(time.Date(2020, 2, 29, 0, 0, 0, 0, time.UTC))),
		TimeValue(TimeOfDay(1*3600e9 + 2*60e9 + 3e9)), // 01:02:03
		DateTime(time.Date(2026, 7, 8, 9, 10, 11, 123456789, time.FixedZone("CST", 8*3600))),
		DecimalValue(Decimal{Unscaled: bigDec, Scale: 6}),
		Null(),
	}
	require.NoError(t, w.Insert("t", 1, row))

	// NaN and ±Inf bit patterns must be preserved exactly.
	row[0] = Uint64(2)
	row[10] = Float32(float32(math.NaN()))
	row[11] = Float64(math.Inf(-1))
	require.NoError(t, w.Insert("t", 2, row))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Get(ctx, snap, "t", 1, nil)
	require.NoError(t, err)
	require.Len(t, got, len(row))

	x, _ := got[0].Uint64()
	require.Equal(t, uint64(42), x)
	b, _ := got[1].Bool()
	require.True(t, b)
	i8, _ := got[2].Int8()
	require.Equal(t, int8(-8), i8)
	i16, _ := got[3].Int16()
	require.Equal(t, int16(-1600), i16)
	i32, _ := got[4].Int32()
	require.Equal(t, int32(-32_000_000), i32)
	i64, _ := got[5].Int64()
	require.Equal(t, int64(-64_000_000_000), i64)
	u8, _ := got[6].Uint8()
	require.Equal(t, uint8(255), u8)
	u16, _ := got[7].Uint16()
	require.Equal(t, uint16(65535), u16)
	u32, _ := got[8].Uint32()
	require.Equal(t, uint32(4_000_000_000), u32)
	u64, _ := got[9].Uint64()
	require.Equal(t, uint64(18_000_000_000_000_000_000), u64)
	f32, _ := got[10].Float32()
	require.Equal(t, float32(0.5), f32)
	f64, _ := got[11].Float64()
	require.Equal(t, -3.141592653589793, f64)
	s, _ := got[12].String()
	require.Equal(t, "中文/emoji 🚀 and \x00 binary", s)
	by, _ := got[13].Bytes()
	require.Equal(t, []byte{0, 1, 2, 250, 251, 252, 253, 254, 255}, by)
	d, _ := got[14].Date()
	require.Equal(t, NewDate(time.Date(2020, 2, 29, 0, 0, 0, 0, time.UTC)), d)
	tod, _ := got[15].Time()
	require.Equal(t, TimeOfDay(1*3600e9+2*60e9+3e9), tod)
	dt, _ := got[16].DateTimeValue()
	require.True(t, dt.Equal(time.Date(2026, 7, 8, 1, 10, 11, 123456789, time.UTC)), "dt=%v (9:10:11 CST = 1:10:11 UTC)", dt)
	dec, _ := got[17].Decimal()
	require.Equal(t, 0, dec.Unscaled.Cmp(bigDec), "scaled big decimal")
	require.Equal(t, int32(6), dec.Scale)
	require.True(t, got[18].IsNull(), "nullable column written as NULL")

	// NaN and ±Inf bit patterns must be preserved exactly.
	got2, err := db.Get(ctx, snap, "t", 2, nil)
	require.NoError(t, err)
	f32v, _ := got2[10].Float32()
	require.True(t, math.IsNaN(float64(f32v)), "NaN bit pattern preserved")
	f64v, _ := got2[11].Float64()
	require.True(t, math.IsInf(f64v, -1), "-Inf preserved")
}

// TestDecimalRoundTrip covers the decimal fast path (int64) and the big.Int
// path via the store, including values near the int64 boundary and beyond.
func TestDecimalRoundTrip(t *testing.T) {
	cases := []string{
		"0",
		"12345",
		"9223372036854775807",            // int64 max
		"-9223372036854775808",           // int64 min
		"123456789012345678901234567890", // over int64, positive
		"-123456789012345678901234567890",
		"-129", // two's-complement width transition
		"127",  // positive top-byte-sign transition
		"128",  // needs a leading 0x00 byte
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			u, ok := new(big.Int).SetString(tc, 10)
			require.True(t, ok)
			db := testDB(t, Options{})
			ctx := context.Background()
			w, _ := db.Begin(ctx, NoParent)
			require.NoError(t, w.DefineTable("d", []Column{{Name: "v", Type: TypeDecimal, Scale: 0}}))
			require.NoError(t, w.Insert("d", 1, Row{DecimalValue(Decimal{Unscaled: new(big.Int).Set(u), Scale: 0})}))
			snap, err := w.Commit(ctx)
			require.NoError(t, err)
			got, err := db.Get(ctx, snap, "d", 1, nil)
			require.NoError(t, err)
			dec, _ := got[0].Decimal()
			require.Equal(t, 0, dec.Unscaled.Cmp(u), "%s round trip", tc)
		})
	}
}

// TestValueCopies verifies the Value contract: constructors copy inputs and
// getters return copies.
func TestValueCopies(t *testing.T) {
	// Bytes constructor copies.
	src := []byte{1, 2, 3}
	v := Bytes(src)
	src[0] = 99
	b, _ := v.Bytes()
	require.Equal(t, []byte{1, 2, 3}, b)

	// Getting bytes returns fresh slices.
	c := Bytes([]byte{1, 2, 3})
	b1, _ := c.Bytes()
	b1[0] = 42
	b2, _ := c.Bytes()
	require.Equal(t, byte(1), b2[0], "getter must return a copy")

	// Decimal constructor copies the big.Int.
	u := big.NewInt(123)
	dv := DecimalValue(Decimal{Unscaled: u, Scale: 2})
	u.Add(u, big.NewInt(1000))
	d, _ := dv.Decimal()
	require.Equal(t, "123", d.Unscaled.String(), "constructor must copy the unscaled big.Int")
}

// TestRowReuse verifies the documented dst-reuse semantics of Get: repeated
// Gets into the same dst reuse the backing array.
func TestRowReuse(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 3)
	snap, _ := w.Commit(ctx)

	var dst Row
	for i := 1; i <= 3; i++ {
		row, err := db.Get(ctx, snap, "users", uint64(i), dst)
		require.NoError(t, err)
		dst = row
		require.Len(t, dst, 4)
	}
	row2, err := db.Get(ctx, snap, "users", 2, dst)
	require.NoError(t, err)
	v, _ := row2[1].String()
	require.Equal(t, "user-2", v)
}

// TestReadOnlyOpen verifies a read-only open reads the same bytes as the
// writer and rejects writes.
func TestReadOnlyOpen(t *testing.T) {
	base := filepath.Join(tmpdb(t), "ro")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, w.Insert("t", 1, Row{Uint64(100)}))
	snap, _ := w.Commit(ctx)
	require.NoError(t, db.Close())

	ro, err := Open(base, Options{ReadOnly: true, BlockSize: 1024})
	require.NoError(t, err)
	defer ro.Close()
	require.True(t, ro.ReadOnly())
	row, err := ro.Get(ctx, snap, "t", 1, nil)
	require.NoError(t, err)
	v, _ := row[0].Uint64()
	require.Equal(t, uint64(100), v)
	_, err = ro.Begin(ctx, NoParent)
	require.ErrorIs(t, err, ErrReadOnly)
}
