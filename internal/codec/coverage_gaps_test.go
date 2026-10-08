package codec

import (
	"math/big"
	"strings"
	"testing"

	"time"

	"github.com/stretchr/testify/require"
)

// TestCivilDayRoundtripExtremeDates drives daysFromCivil/civilFromDays
// across era boundaries, including negative years (BC dates) where both the
// era-- correction branches fire. The pair must be exact inverses.
func TestCivilDayRoundtripExtremeDates(t *testing.T) {
	cases := []struct {
		y, m, d int
	}{
		{-4800, 1, 1}, {-4800, 12, 31},
		{-401, 3, 1}, {-401, 2, 28},
		{-400, 1, 1}, {-400, 3, 1},
		{-101, 6, 15}, {-5, 2, 28}, {-1, 12, 31},
		{0, 1, 1}, {0, 3, 1}, {0, 12, 31},
		{1, 1, 1}, {4, 2, 29}, {100, 3, 1}, {400, 2, 29},
		{1969, 12, 31}, {1970, 1, 1}, {1970, 2, 28}, {1970, 3, 1},
		{2000, 2, 29}, {2400, 3, 1}, {9999, 12, 31},
	}
	for _, c := range cases {
		days := daysFromCivil(c.y, c.m, c.d)
		gotY, gotM, gotD := civilFromDays(days)
		if gotY != c.y || gotM != c.m || gotD != c.d {
			t.Fatalf("civil roundtrip %04d-%02d-%02d: got %04d-%02d-%02d (days=%d)",
				c.y, c.m, c.d, gotY, gotM, gotD, days)
		}
	}
	// Negative years through the public Date API too: NewDate must honor the
	// location offset and Date.Time must invert it.
	loc := time.FixedZone("E2", 2*3600)
	in := time.Date(-5, time.March, 1, 23, 30, 0, 0, loc) // already 03-02 01:30 local+off
	d := NewDate(in)
	y, m, day := civilFromDays(int(d))
	if y != -5 || m != 3 || day != 2 {
		t.Fatalf("NewDate(-5-03-01 23:30 +02:00) = %04d-%02d-%02d, want -5-03-02", y, m, day)
	}
	back := d.Time(time.UTC)
	if back.Year() != -5 || back.Month() != time.March || back.Day() != 2 {
		t.Fatalf("Date.Time roundtrip = %v", back)
	}
}

// TestBitmapHelperBranches pins the two bitmap helpers' edge outputs.
func TestBitmapHelperBranches(t *testing.T) {
	if !bitmapIsZero(nil) || !bitmapIsZero([]byte{0, 0}) {
		t.Fatal("zero bitmaps must report zero")
	}
	if bitmapIsZero([]byte{0, 1}) {
		t.Fatal("non-zero bitmap must report false")
	}
	require.Equal(t, uint8(0xFE), tailBitmapMask(1), "tailBitmapMask(1) = %#x, want 0xFE", tailBitmapMask(1))
	require.Equal(t, uint8(0xF0), tailBitmapMask(4), "tailBitmapMask(4) = %#x, want 0xF0", tailBitmapMask(4))
	require.Equal(t, uint8(0), tailBitmapMask(8), "tailBitmapMask(8) = %#x, want 0", tailBitmapMask(8))
}

// TestAppendValueInvalidValues drives appendValue's validation arms directly:
// per-type limits, decimal checks and the unsupported-type default.
func TestAppendValueInvalidValues(t *testing.T) {
	defaults := DefaultLimits()
	cases := []struct {
		name   string
		v      Value
		limits Limits
		want   string
	}{
		{"bytes over MaxValueBytes", Bytes(make([]byte, 9)), Limits{MaxValueBytes: 8, MaxRowBytes: defaults.MaxRowBytes},
			"bytes value of 9 bytes exceeds limit 8"},
		{"time negative", Value{typ: TypeTime, i: -1}, defaults, "time of day -1 out of range"},
		{"time at MaxTimeOfDay", Value{typ: TypeTime, i: MaxTimeOfDay}, defaults, "out of range"},
		{"decimal negative scale", DecimalValue(Decimal{Unscaled: big.NewInt(1), Scale: -1}), defaults,
			"decimal scale -1 is negative"},
		// DecimalValue() substitutes a zero big.Int, so build the raw value
		// directly to exercise the nil-unscaled guard.
		{"decimal nil unscaled", Value{typ: TypeDecimal, d: Decimal{Scale: 3}}, defaults, "decimal unscaled is nil"},
		{"decimal int64 over MaxValueBytes", DecimalValue(Decimal{Unscaled: big.NewInt(100000), Scale: 0}),
			Limits{MaxValueBytes: 2, MaxRowBytes: defaults.MaxRowBytes}, "exceeds limit 2"},
		{"decimal over MaxRowBytes", DecimalValue(Decimal{Unscaled: big.NewInt(100000), Scale: 0}),
			Limits{MaxValueBytes: defaults.MaxValueBytes, MaxRowBytes: 4}, "exceeds limit 4"},
		{"unsupported type", Value{typ: Type(99)}, defaults, "unsupported type 99"},
	}
	for _, c := range cases {
		buf, err := appendValue(nil, c.v, c.limits)
		require.NotNil(t, err, "%s: accepted", c.name)
		require.Nil(t, buf, "%s: returned non-nil buffer on error", c.name)
		require.Contains(t, err.Error(), c.want, "%s: error %q, want substring %q", c.name, err, c.want)
	}
}

// TestEncodeIntoWideSchemaBitmapFallback forces the make()-based null-bitmap
// fill: more than 16384 columns means bitmapBytes > len(bitmapScratch).
func TestEncodeIntoWideSchemaBitmapFallback(t *testing.T) {
	const ncols = 16400 // bitmapBytes = 2050 > 2048
	limits := DefaultLimits()
	limits.MaxColumns = 20000
	limits.MaxRowBytes = 1 << 20
	cols := make([]Column, ncols)
	for i := range cols {
		cols[i] = Column{Name: "c", Type: TypeUint8}
	}
	schema := &Schema{Name: "wide", Columns: cols}
	c := Codec{Limits: limits}

	row := make([]Value, ncols)
	for i := range row {
		row[i] = Uint8(uint8(i))
	}
	body, err := c.EncodeInto(schema, row, nil)
	require.NoError(t, err, "encode wide row")
	dec, err := c.CompileDecoder(schema)
	require.NoError(t, err, "compile")
	got, err := dec.DecodeInto(nil, body, nil)
	require.NoError(t, err, "decode wide row")
	for i := range ncols {
		require.Equal(t, uint64(uint8(i)), got[i].u, "col %d = %d", i, got[i].u)
	}
}

// TestEncodeIntoRowBytesLimit drives the running-size guard of
// encodeBodyInto: the cumulative buffer length is checked after every column.
func TestEncodeIntoRowBytesLimit(t *testing.T) {
	c := Codec{Limits: Limits{MaxColumns: 100, MaxValueBytes: 1 << 20, MaxRowBytes: 16}}
	schema := &Schema{Name: "s", Columns: []Column{
		{Name: "a", Type: TypeString},
		{Name: "b", Type: TypeString},
	}}
	row := []Value{String(strings.Repeat("x", 12)), String("y")}
	if _, err := c.EncodeInto(schema, row, nil); err == nil || !strings.Contains(err.Error(), "exceeds limit 16") {
		t.Fatalf("want row-bytes error, got %v", err)
	}
}

// TestDecodeSinkMaterialization pins the Sink contract on both decode paths:
// the plan-driven fast path on valid bodies, and the validating generic path
// (reached when fastDecode declines on trailing bytes) which still
// materializes earlier values through the sink before reporting the error.
func TestDecodeSinkMaterialization(t *testing.T) {
	schema := &Schema{Name: "s", Columns: []Column{
		{Name: "a", Type: TypeString},
		{Name: "b", Type: TypeBytes},
	}}
	c := Codec{Limits: DefaultLimits()}
	dec, err := c.CompileDecoder(schema)
	require.NoError(t, err, "compile")
	body, err := c.EncodeInto(schema, []Value{String("hello"), Bytes([]byte{1, 2, 3})}, nil)
	require.NoError(t, err, "encode")

	var gotStrings, gotBytes []string
	sink := &Sink{
		String: func(p []byte) string { s := string(p); gotStrings = append(gotStrings, s); return s },
		Bytes: func(p []byte) []byte {
			cp := append([]byte(nil), p...)
			gotBytes = append(gotBytes, string(cp))
			return cp
		},
	}

	// Fast path: both sink funcs materialize the payloads.
	row, err := dec.DecodeInto(nil, body, sink)
	require.NoError(t, err, "fast decode")
	if row[0].s != "hello" || string(row[1].by) != "\x01\x02\x03" {
		t.Fatalf("fast decode values: %q %q", row[0].s, row[1].by)
	}
	if len(gotStrings) != 1 || gotStrings[0] != "hello" || len(gotBytes) != 1 {
		t.Fatalf("fast sink calls: %q %q", gotStrings, gotBytes)
	}

	// Generic path: trailing byte makes fastDecode decline; the validating
	// decoder must still materialize both values through the sink, then fail
	// on the trailing byte.
	tailed := append(append([]byte(nil), body...), 0xFF)
	if _, err := dec.DecodeInto(nil, tailed, sink); err == nil || !strings.Contains(err.Error(), "trailing bytes") {
		t.Fatalf("want trailing-bytes error, got %v", err)
	}
	// The generic decoder reads every value before checking trailing bytes,
	// so both columns are materialized a second time.
	if len(gotStrings) != 3 || gotStrings[1] != "hello" || gotStrings[2] != "hello" ||
		len(gotBytes) != 3 || gotBytes[1] != "\x01\x02\x03" || gotBytes[2] != "\x01\x02\x03" {
		t.Fatalf("generic sink calls: %q %q", gotStrings, gotBytes)
	}
}

// TestBatchDecodeFallsBackToValidator covers DecodeBatchInto's fallback arm:
// a body whose unused null-bitmap bits are set declines the fast path, the
// validating decoder rejects it, and the error names the offending row.
func TestBatchDecodeFallsBackToValidator(t *testing.T) {
	schema := &Schema{Name: "s", Columns: []Column{{Name: "a", Type: TypeString}}}
	c := Codec{Limits: DefaultLimits()}
	dec, err := c.CompileDecoder(schema)
	require.NoError(t, err, "compile")
	good, err := c.EncodeInto(schema, []Value{String("ok")}, nil)
	require.NoError(t, err, "encode")
	bad := append([]byte(nil), good...)
	bad[0] |= 0x02 // single-column schema: bit 1 is an unused high bit

	if _, err := dec.DecodeBatchInto(nil, [][]byte{good, bad}, nil); err == nil {
		t.Fatal("batch with unused bitmap bits accepted")
	} else {
		require.Contains(t, err.Error(), "row 1", "error should name row 1: %v", err)
	}
}

// TestBatchDecodeFixedFallback covers the batch kernel's other fallback arm:
// a fully-fixed schema takes decodeFixedInto, and an invalid value (bool = 2)
// sends the row to the validating decoder for the canonical error.
func TestBatchDecodeFixedFallback(t *testing.T) {
	schema := &Schema{Name: "s", Columns: []Column{
		{Name: "a", Type: TypeBool},
		{Name: "b", Type: TypeUint8},
	}}
	c := Codec{Limits: DefaultLimits()}
	dec, err := c.CompileDecoder(schema)
	require.NoError(t, err, "compile")
	// Zero bitmap, then payload: bool = 2 (invalid), uint8 = 7.
	body := []byte{0x00, 0x02, 0x07}
	if _, err := dec.DecodeBatchInto(nil, [][]byte{body}, nil); err == nil {
		t.Fatal("invalid bool accepted")
	}
	// The same body through DecodeInto takes the identical fallback.
	if _, err := dec.DecodeInto(nil, body, nil); err == nil {
		t.Fatal("invalid bool accepted (single)")
	}
}

// TestUnsupportedTypeChain walks an unvalidatable (hand-built) schema with an
// unknown column type through compile, fast decode, generic decode and
// encode: every layer must refuse it without panicking.
func TestUnsupportedTypeChain(t *testing.T) {
	c := Codec{Limits: DefaultLimits()}
	schema := &Schema{Name: "bogus", Columns: []Column{{Name: "a", Type: Type(99)}}}
	dec, err := c.CompileDecoder(schema)
	require.NoError(t, err, "CompileDecoder must not validate types")
	body := []byte{0x00, 0x01, 0x02, 0x03} // null bitmap byte + arbitrary payload
	if _, err := dec.DecodeInto(nil, body, nil); err == nil || !strings.Contains(err.Error(), "unsupported type 99") {
		t.Fatalf("decode: want unsupported-type error, got %v", err)
	}
	if _, err := c.EncodeInto(schema, []Value{{typ: Type(99)}}, nil); err == nil || !strings.Contains(err.Error(), "unsupported type 99") {
		t.Fatalf("encode: want unsupported-type error, got %v", err)
	}
}
