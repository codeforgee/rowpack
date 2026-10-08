package codec

import (
	"fmt"
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDecodePlanMatchesGeneric differentially tests the compiled decode plan
// against the generic validating decoder, over every column type and mixed
// nullability, on pristine and randomly corrupted bodies.
//
// The contract: whenever the plan accepts a body, the generic decoder must
// accept it too and produce an identical row; when the plan declines, the
// decoder must return exactly what the generic decoder returns (an error, or a
// row for the rare shapes only it accepts).
func TestDecodePlanMatchesGeneric(t *testing.T) {
	schema := &Schema{Name: "diff", Columns: []Column{
		{Name: "b", Type: TypeBool},
		{Name: "i8", Type: TypeInt8},
		{Name: "i16", Type: TypeInt16},
		{Name: "i32", Type: TypeInt32},
		{Name: "i64", Type: TypeInt64},
		{Name: "u8", Type: TypeUint8},
		{Name: "u16", Type: TypeUint16},
		{Name: "u32", Type: TypeUint32},
		{Name: "u64", Type: TypeUint64},
		{Name: "f32", Type: TypeFloat32, Nullable: true},
		{Name: "f64", Type: TypeFloat64},
		{Name: "date", Type: TypeDate, Nullable: true},
		{Name: "time", Type: TypeTime},
		{Name: "dt", Type: TypeDateTime},
		{Name: "s", Type: TypeString, Nullable: true},
		{Name: "by", Type: TypeBytes},
		{Name: "dec", Type: TypeDecimal, Scale: 2, Nullable: true},
	}}
	decoder, err := testCodec.CompileDecoder(schema)
	require.NoError(t, err)
	bitmapBytes := (len(schema.Columns) + 7) / 8
	rng := rand.New(rand.NewSource(1))
	row := make([]Value, len(schema.Columns))

	for range 2000 {
		for i := range schema.Columns {
			col := &schema.Columns[i]
			if col.Nullable && rng.Intn(3) == 0 {
				row[i] = Null()
				continue
			}
			row[i] = randomColumnValue(rng, col)
		}
		body, err := testCodec.EncodeInto(schema, row, nil)
		require.NoError(t, err)

		cases := [][]byte{body}
		for range 3 {
			mut := append([]byte(nil), body...)
			if len(mut) > 0 && rng.Intn(2) == 0 {
				mut = mut[:rng.Intn(len(mut))]
			}
			if len(mut) > 0 {
				mut[rng.Intn(len(mut))] ^= 1 << uint(rng.Intn(8))
			}
			cases = append(cases, mut)
		}

		for _, in := range cases {
			want, wantErr := testCodec.decodeBodyIntoPrepared(nil, in, schema, bitmapBytes, nil)
			fastRow, fastOK := decoder.fastDecode(nil, in, nil)
			if fastOK {
				require.NoError(t, wantErr, "plan accepted a body the validator rejects")
				require.Equal(t, want, fastRow, "plan disagrees with the validator")
				continue
			}
			got, gotErr := decoder.DecodeInto(nil, in, nil)
			if wantErr != nil {
				require.Error(t, gotErr)
				continue
			}
			require.NoError(t, gotErr)
			require.Equal(t, want, got)
		}
	}
}

func randomColumnValue(rng *rand.Rand, col *Column) Value {
	switch col.Type {
	case TypeBool:
		return Bool(rng.Intn(2) == 0)
	case TypeInt8:
		return Int8(int8(rng.Intn(256)))
	case TypeInt16:
		return Int16(int16(rng.Intn(65536)))
	case TypeInt32:
		return Int32(int32(rng.Uint32()))
	case TypeInt64:
		return Int64(int64(rng.Uint64()))
	case TypeUint8:
		return Uint8(uint8(rng.Intn(256)))
	case TypeUint16:
		return Uint16(uint16(rng.Intn(65536)))
	case TypeUint32:
		return Uint32(rng.Uint32())
	case TypeUint64:
		return Uint64(rng.Uint64())
	case TypeFloat32:
		return Float32(rng.Float32())
	case TypeFloat64:
		return Float64(rng.Float64())
	case TypeDate:
		return DateValue(Date(rng.Intn(20000) - 10000))
	case TypeTime:
		return TimeValue(TimeOfDay(rng.Int63n(int64(MaxTimeOfDay))))
	case TypeDateTime:
		return DateTime(time.Unix(0, rng.Int63()).UTC())
	case TypeString:
		return String(fmt.Sprintf("value-%d-π", rng.Intn(1000)))
	case TypeBytes:
		b := make([]byte, rng.Intn(6))
		rng.Read(b)
		return Bytes(b)
	case TypeDecimal:
		return DecimalValue(Decimal{Unscaled: big.NewInt(rng.Int63n(1 << 40)), Scale: col.Scale})
	}
	panic("unhandled type")
}
