package codec

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testCodec is the row codec shared by the tests (all encode/decode cases here
// run under the default limits).
var testCodec = DefaultCodec()

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
		encoded, err := testCodec.Encode(schema, row)
		require.NoError(t, err)

		decoded, err := testCodec.Decode(encoded, schema)
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
		encoded, err := testCodec.Encode(schema, row)
		require.NoError(t, err)

		decoded, err := testCodec.Decode(encoded, schema)
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
	for i := 0; i < 10; i++ {
		row := []Value{Uint64(uint64(i)), Int64(int64(i * 10))}
		var err error
		buf, err = testCodec.EncodeTupleInto(schema, row, buf)
		require.NoError(t, err)
		require.Greater(t, len(buf), 0)

		decoded, err := testCodec.Decode(buf, schema)
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

	decoded, err := testCodec.DecodeInto(nil, body, schema, nil)
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
	encoded, err := testCodec.Encode(schema, row)
	require.NoError(t, err)

	var dst []Value
	for i := 0; i < 5; i++ {
		decoded, err := testCodec.DecodeTupleInto(dst, encoded, schema, nil)
		require.NoError(t, err)
		require.Equal(t, uint64(42), decoded[0].u)
		require.Equal(t, int64(100), decoded[1].i)
		dst = decoded
	}
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
		_, err := testCodec.Encode(nil, []Value{Uint64(1)})
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil schema")
	})

	t.Run("column count mismatch", func(t *testing.T) {
		_, err := testCodec.Encode(schema, []Value{Uint64(1)})
		require.Error(t, err)
		require.Contains(t, err.Error(), "schema mismatch")
	})

	t.Run("not nullable but null", func(t *testing.T) {
		_, err := testCodec.Encode(schema, []Value{Uint64(1), Null()})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not nullable")
	})

	t.Run("type mismatch", func(t *testing.T) {
		_, err := testCodec.Encode(schema, []Value{Uint64(1), Int64(2)})
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
		_, err := testCodec.Encode(schemaWithDecimal, []Value{Uint64(1), DecimalValue(Decimal{Unscaled: big.NewInt(100), Scale: 3})})
		require.Error(t, err)
		require.Contains(t, err.Error(), "scale")
	})
}

func TestDecodeErrors(t *testing.T) {
	schema := &Schema{
		Name: "test",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "name", Type: TypeString},
		},
	}
	require.NoError(t, schema.Validate(DefaultLimits()))

	validRow := []Value{Uint64(1), String("test")}
	encoded, _ := testCodec.Encode(schema, validRow)

	t.Run("nil schema", func(t *testing.T) {
		_, err := testCodec.Decode(encoded, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil schema")
	})

	t.Run("truncated header", func(t *testing.T) {
		_, err := testCodec.Decode([]byte{1, 2, 3}, schema)
		require.Error(t, err)
		require.Contains(t, err.Error(), "truncated tuple header")
	})

	t.Run("column count mismatch", func(t *testing.T) {
		_, err := testCodec.Decode(encoded, &Schema{Name: "t", Columns: []Column{{Name: "a", Type: TypeInt64}}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "tuple has")
	})

	t.Run("bitmap bytes mismatch", func(t *testing.T) {
		bad := make([]byte, len(encoded))
		copy(bad, encoded)
		bad[4] = 99
		_, err := testCodec.Decode(bad, schema)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bitmap bytes")
	})

	t.Run("non-zero unused bitmap bits", func(t *testing.T) {
		bad := make([]byte, len(encoded))
		copy(bad, encoded)
		// bitmap starts at offset 8, first bitmap byte is at index 8
		bad[8] |= 0x80
		_, err := testCodec.Decode(bad, schema)
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-zero unused null bitmap bits")
	})

	t.Run("truncated null bitmap", func(t *testing.T) {
		// Body-only decode with only 1 byte (need at least 1 byte for bitmap)
		_, err := testCodec.DecodeInto(nil, []byte{0}, schema, nil)
		require.Error(t, err)
		require.Contains(t, err.Error(), "truncated")
	})

	t.Run("trailing bytes", func(t *testing.T) {
		body, _ := testCodec.EncodeInto(schema, validRow, nil)
		body = append(body, 0xFF)
		_, err := testCodec.DecodeInto(nil, body, schema, nil)
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

	decoded, err := testCodec.DecodeInto(nil, body, schema, nil)
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
		_, err := c.Encode(schema, row)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})

	t.Run("row exceeds limit", func(t *testing.T) {
		row := []Value{Uint64(1), String(string(make([]byte, 3000)))}
		_, err := c.Encode(schema, row)
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

	encoded, err := testCodec.Encode(schema, row)
	require.NoError(t, err)

	decoded, err := testCodec.Decode(encoded, schema)
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
