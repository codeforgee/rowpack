package codec

import (
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testCodec is the row codec shared by the tests (all encode/decode cases here
// run under the default limits).
var testCodec = Codec{Limits: DefaultLimits()}

func decodeTestBody(t testing.TB, c Codec, schema *Schema, body []byte) ([]Value, error) {
	t.Helper()
	decoder, err := c.CompileDecoder(schema)
	if err != nil {
		return nil, err
	}
	return decoder.DecodeInto(nil, body, nil)
}

func decodeTestBodyInto(t testing.TB, c Codec, dst []Value, schema *Schema, body []byte) ([]Value, error) {
	t.Helper()
	decoder, err := c.CompileDecoder(schema)
	if err != nil {
		return nil, err
	}
	return decoder.DecodeInto(dst, body, nil)
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "name", Type: TypeString, Nullable: true},
			{Name: "active", Type: TypeBool},
			{Name: "balance", Type: TypeDecimal, Scale: 2},
			{Name: "data", Type: TypeBytes, Nullable: true},
			{Name: "score", Type: TypeFloat64, Nullable: true},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	t.Run("all non-null", func(t *testing.T) {
		row := []Value{
			Uint64(42),
			String("hello"),
			Bool(true),
			DecimalValue(Decimal{Unscaled: big.NewInt(12345), Scale: 2}),
			Bytes([]byte{1, 2, 3}),
			Float64(3.14),
		}
		encoded, err := testCodec.EncodeInto(schema, row, nil)
		require.NoError(t, err)

		decoded, err := decodeTestBody(t, testCodec, schema, encoded)
		require.NoError(t, err)
		require.Equal(t, len(row), len(decoded))
		for i := range row {
			require.Equal(t, row[i].Type(), decoded[i].Type())
			switch row[i].Type() {
			case TypeUint64:
				require.Equal(t, row[i].u, decoded[i].u)
			case TypeString:
				require.Equal(t, row[i].s, decoded[i].s)
			case TypeBool:
				require.Equal(t, row[i].b, decoded[i].b)
			case TypeDecimal:
				require.Equal(t, row[i].d.Unscaled.Int64(), decoded[i].d.Unscaled.Int64())
				require.Equal(t, row[i].d.Scale, decoded[i].d.Scale)
			case TypeBytes:
				require.Equal(t, row[i].by, decoded[i].by)
			case TypeFloat64:
				require.Equal(t, row[i].f64, decoded[i].f64)
			}
		}
	})

	t.Run("with nulls", func(t *testing.T) {
		row := []Value{
			Uint64(42),
			Null(),
			Bool(false),
			DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 2}),
			Null(),
			Null(),
		}
		encoded, err := testCodec.EncodeInto(schema, row, nil)
		require.NoError(t, err)

		decoded, err := decodeTestBody(t, testCodec, schema, encoded)
		require.NoError(t, err)
		require.Equal(t, len(row), len(decoded))
		require.True(t, decoded[1].IsNull())
		require.True(t, decoded[4].IsNull())
		require.True(t, decoded[5].IsNull())
	})
}

func TestEncodeIntoReuse(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "val", Type: TypeInt64},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	var buf []byte
	for i := range 10 {
		row := []Value{Uint64(uint64(i)), Int64(int64(i * 10))}
		var err error
		buf, err = testCodec.EncodeInto(schema, row, buf)
		require.NoError(t, err)
		require.Greater(t, len(buf), 0)

		decoded, err := decodeTestBody(t, testCodec, schema, buf)
		require.NoError(t, err)
		require.Equal(t, uint64(i), decoded[0].u)
		require.Equal(t, int64(i*10), decoded[1].i)
	}
}

func TestEncodeBodyInto(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "val", Type: TypeString},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	row := []Value{Uint64(100), String("test-value")}
	body, err := testCodec.EncodeInto(schema, row, nil)
	require.NoError(t, err)

	decoded, err := decodeTestBody(t, testCodec, schema, body)
	require.NoError(t, err)
	require.Equal(t, uint64(100), decoded[0].u)
	require.Equal(t, "test-value", decoded[1].s)
}

func TestDecodeIntoReuse(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "val", Type: TypeInt64},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	row := []Value{Uint64(42), Int64(100)}
	encoded, err := testCodec.EncodeInto(schema, row, nil)
	require.NoError(t, err)

	var dst []Value
	for range 5 {
		decoded, err := decodeTestBodyInto(t, testCodec, dst, schema, encoded)
		require.NoError(t, err)
		require.Equal(t, uint64(42), decoded[0].u)
		require.Equal(t, int64(100), decoded[1].i)
		dst = decoded
	}
}

func TestPreparedDecoderMatchesDecodeInto(t *testing.T) {
	schema := &Schema{Name: "prepared", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString, Nullable: true},
		{Name: "score", Type: TypeFloat64},
	}}
	body, err := testCodec.EncodeInto(schema, []Value{Uint64(7), Null(), Float64(3.5)}, nil)
	require.NoError(t, err)
	decoder, err := testCodec.CompileDecoder(schema)
	require.NoError(t, err)
	want, err := decodeTestBody(t, testCodec, schema, body)
	require.NoError(t, err)
	got, err := decoder.DecodeInto(nil, body, nil)
	require.NoError(t, err)
	require.Equal(t, want, got)

	_, err = testCodec.CompileDecoder(nil)
	require.Error(t, err)
	limited := Codec{Limits: Limits{MaxColumns: 1}}
	_, err = limited.CompileDecoder(schema)
	require.Error(t, err)
}

func TestPreparedDecoderFixedWidthFastPath(t *testing.T) {
	schema := &Schema{Name: "fixed", Columns: []Column{
		{Name: "b", Type: TypeBool}, {Name: "i8", Type: TypeInt8},
		{Name: "i16", Type: TypeInt16}, {Name: "i32", Type: TypeInt32},
		{Name: "i64", Type: TypeInt64}, {Name: "u8", Type: TypeUint8},
		{Name: "u16", Type: TypeUint16}, {Name: "u32", Type: TypeUint32},
		{Name: "u64", Type: TypeUint64}, {Name: "f32", Type: TypeFloat32},
		{Name: "f64", Type: TypeFloat64}, {Name: "date", Type: TypeDate},
		{Name: "time", Type: TypeTime}, {Name: "dt", Type: TypeDateTime},
	}}
	input := []Value{
		Bool(true), Int8(-8), Int16(-16), Int32(-32), Int64(-64),
		Uint8(8), Uint16(16), Uint32(32), Uint64(64), Float32(1.5),
		Float64(2.5), DateValue(Date(-10)), TimeValue(TimeOfDay(123)),
		DateTime(time.Unix(0, -456).UTC()),
	}
	body, err := testCodec.EncodeInto(schema, input, nil)
	require.NoError(t, err)
	decoder, err := testCodec.CompileDecoder(schema)
	require.NoError(t, err)
	require.True(t, decoder.fixed)
	got, err := decoder.DecodeInto(nil, body, nil)
	require.NoError(t, err)
	require.Equal(t, input, got)

	// Invalid fixed-width values must fall back to the generic validator.
	badBool := append([]byte(nil), body...)
	badBool[decoder.bitmapBytes] = 2
	_, err = decoder.DecodeInto(nil, badBool, nil)
	require.ErrorContains(t, err, "invalid bool")
	badTime := append([]byte(nil), body...)
	timeOff := decoder.bitmapBytes + 1 + 1 + 2 + 4 + 8 + 1 + 2 + 4 + 8 + 4 + 8 + 4
	binary.LittleEndian.PutUint64(badTime[timeOff:], uint64(MaxTimeOfDay))
	_, err = decoder.DecodeInto(nil, badTime, nil)
	require.ErrorContains(t, err, "out of range")
}

func TestPreparedDecoderBatch(t *testing.T) {
	schema := &Schema{Name: "batch", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "value", Type: TypeInt32},
	}}
	decoder, err := testCodec.CompileDecoder(schema)
	require.NoError(t, err)
	var bodies [][]byte
	for i := range 3 {
		body, err := testCodec.EncodeInto(schema, []Value{Uint64(uint64(i + 1)), Int32(int32(i * 10))}, nil)
		require.NoError(t, err)
		bodies = append(bodies, body)
	}
	prefix := []Value{Int64(99)}
	slab := make([]Value, len(prefix), len(prefix)+len(bodies)*len(schema.Columns))
	copy(slab, prefix)
	got, err := decoder.DecodeBatchInto(slab, bodies, nil)
	require.NoError(t, err)
	require.Equal(t, prefix[0], got[0])
	require.Len(t, got, 1+3*2)
	for i := range 3 {
		id, ok := got[1+i*2].Uint64()
		require.True(t, ok)
		require.Equal(t, uint64(i+1), id)
	}

	bad := append([]byte(nil), bodies[1]...)
	bad = bad[:len(bad)-1]
	_, err = decoder.DecodeBatchInto(nil, [][]byte{bodies[0], bad}, nil)
	require.ErrorContains(t, err, "decode batch row 1")

	// A nullable schema is intentionally handled by the generic batch path.
	nullable := &Schema{Name: "nullable", Columns: []Column{{Name: "v", Type: TypeInt64, Nullable: true}}}
	nullDecoder, err := testCodec.CompileDecoder(nullable)
	require.NoError(t, err)
	nullBody, err := testCodec.EncodeInto(nullable, []Value{Null()}, nil)
	require.NoError(t, err)
	nullRows, err := nullDecoder.DecodeBatchInto(nil, [][]byte{nullBody}, nil)
	require.NoError(t, err)
	require.True(t, nullRows[0].IsNull())
}

func TestEncodeErrors(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "name", Type: TypeString},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	t.Run("nil schema", func(t *testing.T) {
		_, err := testCodec.EncodeInto(nil, []Value{Uint64(1)}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil schema")
	})

	t.Run("column count mismatch", func(t *testing.T) {
		_, err := testCodec.EncodeInto(schema, []Value{Uint64(1)}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "schema mismatch")
	})

	t.Run("not nullable but null", func(t *testing.T) {
		_, err := testCodec.EncodeInto(schema, []Value{Uint64(1), Null()}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not nullable")
	})

	t.Run("type mismatch", func(t *testing.T) {
		_, err := testCodec.EncodeInto(schema, []Value{Uint64(1), Int64(2)}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "wants type")
	})

	t.Run("decimal scale mismatch", func(t *testing.T) {
		schemaWithDecimal := &Schema{
			Name: "test",
			Columns: []Column{
				{Name: "id", Type: TypeUint64},
				{Name: "balance", Type: TypeDecimal, Scale: 2},
			},
		}
		require.NoError(t, schemaWithDecimal.Validate(DefaultLimits()))
		_, err := testCodec.EncodeInto(schemaWithDecimal, []Value{Uint64(1), DecimalValue(Decimal{Unscaled: big.NewInt(100), Scale: 3})}, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "scale")
	})
}

// TestNullBitmapOmittedWhenNoColumnIsNullable pins the space optimisation: a
// schema with no nullable column writes no NULL bitmap at all, because no value
// can be NULL and the ceil(ncols/8) bytes per row would be pure overhead. The
// encoder and the decoder must agree on the width, so both are exercised.
func TestNullBitmapOmittedWhenNoColumnIsNullable(t *testing.T) {
	tight := &Schema{Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}}
	require.NoError(t, tight.Validate(DefaultLimits()))
	require.Zero(t, tight.nullBitmapBytes(), "no nullable column => no bitmap")

	loose := tight.Clone()
	loose.Columns[1].Nullable = true
	require.Equal(t, 1, loose.nullBitmapBytes(), "one nullable column keeps the bitmap")

	row := []Value{Uint64(7), String("hi")}
	tightBody, err := testCodec.EncodeInto(tight, row, nil)
	require.NoError(t, err, "encode without bitmap")
	looseBody, err := testCodec.EncodeInto(loose, row, nil)
	require.NoError(t, err, "encode with bitmap")
	require.Len(t, tightBody, len(looseBody)-1, "the bitmap byte must be gone")

	got, err := decodeTestBody(t, testCodec, tight, tightBody)
	require.NoError(t, err, "decode without a bitmap")
	require.Equal(t, uint64(7), got[0].Uint64Or(0), "id")
	require.Equal(t, "hi", got[1].StringOr(""), "name")

	// A wide schema would spend the same single byte, and still spends none.
	wide := &Schema{Name: "w", Columns: []Column{
		{Name: "a", Type: TypeUint64},
		{Name: "b", Type: TypeUint64},
		{Name: "c", Type: TypeUint64},
	}}
	require.NoError(t, wide.Validate(DefaultLimits()))
	require.Zero(t, wide.nullBitmapBytes(), "wide non-nullable schema => no bitmap")
}

func TestDecodeErrors(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			// Nullable so the row carries a NULL bitmap: the unused-high-bit
			// check below only exists when there is a bitmap to check.
			{Name: "name", Type: TypeString, Nullable: true},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	validRow := []Value{Uint64(1), String("test")}
	encoded, _ := testCodec.EncodeInto(schema, validRow, nil)

	t.Run("non-zero unused bitmap bits", func(t *testing.T) {
		bad := make([]byte, len(encoded))
		copy(bad, encoded)
		bad[0] |= 0x80
		_, err := decodeTestBody(t, testCodec, schema, bad)
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-zero unused null bitmap bits")
	})

	t.Run("truncated null bitmap", func(t *testing.T) {
		// Body-only decode with only 1 byte (need at least 1 byte for bitmap)
		_, err := decodeTestBody(t, testCodec, schema, []byte{0})
		require.Error(t, err)
		require.Contains(t, err.Error(), "truncated")
	})

	t.Run("trailing bytes", func(t *testing.T) {
		body, _ := testCodec.EncodeInto(schema, validRow, nil)
		body = append(body, 0xFF)
		_, err := decodeTestBody(t, testCodec, schema, body)
		require.Error(t, err)
		require.Contains(t, err.Error(), "trailing bytes")
	})
}

func TestDecodeBodyInto(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "val", Type: TypeString},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	row := []Value{Uint64(100), String("test-value")}
	body, err := testCodec.EncodeInto(schema, row, nil)
	require.NoError(t, err)

	decoded, err := decodeTestBody(t, testCodec, schema, body)
	require.NoError(t, err)
	require.Equal(t, uint64(100), decoded[0].u)
	require.Equal(t, "test-value", decoded[1].s)
}

func TestMaxLimits(t *testing.T) {
	lim := Limits{
		MaxColumns:    100,
		MaxValueBytes: 1024,
		MaxRowBytes:   4096,
	}
	c := Codec{Limits: lim}

	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "data", Type: TypeString},
		},
	}
	require.NoError(t, schema.Validate(lim))

	t.Run("value exceeds limit", func(t *testing.T) {
		row := []Value{Uint64(1), String(string(make([]byte, 2048)))}
		_, err := c.EncodeInto(schema, row, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})

	t.Run("row exceeds limit", func(t *testing.T) {
		row := []Value{Uint64(1), String(string(make([]byte, 3000)))}
		_, err := c.EncodeInto(schema, row, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})
}

func TestAllTypes(t *testing.T) {
	schema := &Schema{
		Name: "all_types",
		Columns: []Column{
			{Name: "b", Type: TypeBool},
			{Name: "i8", Type: TypeInt8},
			{Name: "i16", Type: TypeInt16},
			{Name: "i32", Type: TypeInt32},
			{Name: "i64", Type: TypeInt64},
			{Name: "u8", Type: TypeUint8},
			{Name: "u16", Type: TypeUint16},
			{Name: "u32", Type: TypeUint32},
			{Name: "u64", Type: TypeUint64},
			{Name: "f32", Type: TypeFloat32},
			{Name: "f64", Type: TypeFloat64},
			{Name: "s", Type: TypeString},
			{Name: "by", Type: TypeBytes, Nullable: true},
			{Name: "dt", Type: TypeDate},
			{Name: "tm", Type: TypeTime},
			{Name: "dttm", Type: TypeDateTime},
			{Name: "dec", Type: TypeDecimal, Scale: 3},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	row := []Value{
		Bool(true),
		Int8(-12),
		Int16(-1234),
		Int32(-123456),
		Int64(-123456789012),
		Uint8(200),
		Uint16(50000),
		Uint32(3000000000),
		Uint64(18446744073709551615),
		Float32(3.14),
		Float64(3.141592653589793),
		String("hello world"),
		Bytes([]byte{1, 2, 3, 4, 5}),
		DateValue(Date(19735)),
		TimeValue(TimeOfDay(12*3600e9 + 30*60e9 + 45*1e9 + 123456789)),
		DateTime(time.Date(2024, 1, 15, 12, 30, 45, 123456789, time.UTC)),
		DecimalValue(Decimal{Unscaled: big.NewInt(123456), Scale: 3}),
	}

	encoded, err := testCodec.EncodeInto(schema, row, nil)
	require.NoError(t, err)

	decoded, err := decodeTestBody(t, testCodec, schema, encoded)
	require.NoError(t, err)

	for i := range row {
		require.Equal(t, row[i].Type(), decoded[i].Type())
		switch row[i].Type() {
		case TypeBool:
			require.Equal(t, row[i].b, decoded[i].b)
		case TypeInt8, TypeInt16, TypeInt32, TypeInt64, TypeDate, TypeTime, TypeDateTime:
			require.Equal(t, row[i].i, decoded[i].i)
		case TypeUint8, TypeUint16, TypeUint32, TypeUint64:
			require.Equal(t, row[i].u, decoded[i].u)
		case TypeFloat32:
			require.InDelta(t, row[i].f32, decoded[i].f32, 0.001)
		case TypeFloat64:
			require.InDelta(t, row[i].f64, decoded[i].f64, 0.000000000000001)
		case TypeString:
			require.Equal(t, row[i].s, decoded[i].s)
		case TypeBytes:
			require.Equal(t, row[i].by, decoded[i].by)
		case TypeDecimal:
			require.Equal(t, row[i].d.Unscaled.Int64(), decoded[i].d.Unscaled.Int64())
			require.Equal(t, row[i].d.Scale, decoded[i].d.Scale)
		}
	}
}
