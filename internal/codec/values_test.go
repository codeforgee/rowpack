package codec

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValueConstructors(t *testing.T) {
	v := Null()
	require.True(t, v.IsNull())
	require.Equal(t, Type(0), v.Type())

	v = Bool(true)
	require.False(t, v.IsNull())
	require.Equal(t, TypeBool, v.Type())
	b, ok := v.Bool()
	require.True(t, ok)
	require.True(t, b)

	v = Int8(42)
	require.Equal(t, TypeInt8, v.Type())
	i8, ok := v.Int8()
	require.True(t, ok)
	require.Equal(t, int8(42), i8)

	v = Int16(1000)
	i16, _ := v.Int16()
	require.Equal(t, int16(1000), i16)

	v = Int32(100000)
	i32, _ := v.Int32()
	require.Equal(t, int32(100000), i32)

	v = Int64(10000000000)
	i64, _ := v.Int64()
	require.Equal(t, int64(10000000000), i64)

	v = Uint8(200)
	u8, _ := v.Uint8()
	require.Equal(t, uint8(200), u8)

	v = Uint16(50000)
	u16, _ := v.Uint16()
	require.Equal(t, uint16(50000), u16)

	v = Uint32(3000000000)
	u32, _ := v.Uint32()
	require.Equal(t, uint32(3000000000), u32)

	v = Uint64(18446744073709551615)
	u64, _ := v.Uint64()
	require.Equal(t, uint64(18446744073709551615), u64)

	v = Float32(3.14)
	f32, _ := v.Float32()
	require.InDelta(t, 3.14, f32, 0.001)

	v = Float64(3.141592653589793)
	f64, _ := v.Float64()
	require.InDelta(t, 3.141592653589793, f64, 0.000000000000001)

	v = String("hello")
	s, _ := v.String()
	require.Equal(t, "hello", s)

	v = Bytes([]byte{1, 2, 3})
	b2, _ := v.Bytes()
	require.Equal(t, []byte{1, 2, 3}, b2)

	v = DateValue(Date(19500))
	d, _ := v.Date()
	require.Equal(t, Date(19500), d)

	v = TimeValue(TimeOfDay(3600 * 1e9))
	tod, _ := v.Time()
	require.Equal(t, TimeOfDay(3600*1e9), tod)

	dt := time.Date(2024, 1, 15, 12, 30, 45, 123456789, time.UTC)
	v = DateTime(dt)
	dt2, _ := v.DateTimeValue()
	require.Equal(t, dt.UnixNano(), dt2.UnixNano())

	dec := DecimalValue(Decimal{Unscaled: big.NewInt(12345), Scale: 2})
	dval, _ := dec.Decimal()
	require.Equal(t, int64(12345), dval.Unscaled.Int64())
	require.Equal(t, int32(2), dval.Scale)
}

func TestValueAccessorsInvalidType(t *testing.T) {
	v := Int64(42)

	_, ok := v.Bool()
	require.False(t, ok)

	_, ok = v.Int8()
	require.False(t, ok)

	_, ok = v.Int16()
	require.False(t, ok)

	_, ok = v.Int32()
	require.False(t, ok)

	_, ok = v.Uint8()
	require.False(t, ok)

	_, ok = v.Uint16()
	require.False(t, ok)

	_, ok = v.Uint32()
	require.False(t, ok)

	_, ok = v.Float32()
	require.False(t, ok)

	_, ok = v.Float64()
	require.False(t, ok)

	_, ok = v.String()
	require.False(t, ok)

	_, ok = v.Bytes()
	require.False(t, ok)

	_, ok = v.Date()
	require.False(t, ok)

	_, ok = v.Time()
	require.False(t, ok)

	_, ok = v.DateTimeValue()
	require.False(t, ok)

	_, ok = v.Decimal()
	require.False(t, ok)
}

func TestValueAccessorsNull(t *testing.T) {
	v := Null()

	_, ok := v.Bool()
	require.False(t, ok)

	_, ok = v.Int64()
	require.False(t, ok)

	_, ok = v.String()
	require.False(t, ok)

	_, ok = v.Bytes()
	require.False(t, ok)

	_, ok = v.Decimal()
	require.False(t, ok)
}

func TestBytesCopy(t *testing.T) {
	src := []byte{1, 2, 3}
	v2 := Bytes(src)
	src[0] = 99
	b, _ := v2.Bytes()
	require.Equal(t, byte(1), b[0])
}

func TestDecimalValueCopy(t *testing.T) {
	unscaled := big.NewInt(12345)
	v := DecimalValue(Decimal{Unscaled: unscaled, Scale: 3})
	unscaled.SetInt64(99999)
	dval, _ := v.Decimal()
	require.Equal(t, int64(12345), dval.Unscaled.Int64())
}

func TestDateTimeRoundTrip(t *testing.T) {
	now := time.Now().UTC().Round(0)
	v := DateTime(now)
	dt, ok := v.DateTimeValue()
	require.True(t, ok)
	require.Equal(t, now.UnixNano(), dt.UnixNano())
	require.Equal(t, time.UTC, dt.Location())
}
