package rowpack

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestValueConstructorGetters(t *testing.T) {
	cases := []struct {
		name string
		v    Value
	}{
		{"null", Null()},
		{"bool", Bool(true)},
		{"int8", Int8(-128)},
		{"int16", Int16(-32768)},
		{"int32", Int32(math.MinInt32)},
		{"int64", Int64(math.MinInt64)},
		{"uint8", Uint8(255)},
		{"uint16", Uint16(65535)},
		{"uint32", Uint32(math.MaxUint32)},
		{"uint64", Uint64(math.MaxUint64)},
		{"float32", Float32(float32(math.Pi))},
		{"float64", Float64(math.Pi)},
		{"string", String("hello")},
		{"bytes", Bytes([]byte{1, 2, 3})},
		{"date", DateValue(19000)},
		{"time", TimeValue(86399999999999)},
		{"datetime", DateTime(time.Unix(1700000000, 123456789).UTC())},
		{"decimal", DecimalValue(Decimal{Unscaled: big.NewInt(-12345), Scale: 3})},
	}
	for _, tc := range cases {
		require.Equal(t, tc.name == "null", tc.v.IsNull(), "%s: IsNull = %v", tc.name, tc.v.IsNull())
		if tc.name != "null" && tc.name != "bool" {
			require.NotZero(t, tc.v.Type(), "%s: unexpected zero type", tc.name)
		}
	}
}

func TestValueGettersRoundTrip(t *testing.T) {
	v, ok := Bool(true).Bool()
	require.True(t, v && ok, "Bool roundtrip")
	v2, ok := Int8(-128).Int8()
	require.Equal(t, int8(-128), v2)
	require.True(t, ok, "Int8 roundtrip")
	v3, ok := Int16(-32768).Int16()
	require.Equal(t, int16(-32768), v3)
	require.True(t, ok, "Int16 roundtrip")
	v4, ok := Int32(math.MinInt32).Int32()
	require.Equal(t, int32(math.MinInt32), v4)
	require.True(t, ok, "Int32 roundtrip")
	v5, ok := Int64(math.MinInt64).Int64()
	require.Equal(t, int64(math.MinInt64), v5)
	require.True(t, ok, "Int64 roundtrip")
	v6, ok := Uint8(255).Uint8()
	require.Equal(t, uint8(255), v6)
	require.True(t, ok, "Uint8 roundtrip")
	v7, ok := Uint16(65535).Uint16()
	require.Equal(t, uint16(65535), v7)
	require.True(t, ok, "Uint16 roundtrip")
	v8, ok := Uint32(math.MaxUint32).Uint32()
	require.Equal(t, uint32(math.MaxUint32), v8)
	require.True(t, ok, "Uint32 roundtrip")
	v9, ok := Uint64(math.MaxUint64).Uint64()
	require.Equal(t, uint64(math.MaxUint64), v9)
	require.True(t, ok, "Uint64 roundtrip")
	v10, ok := Float32(float32(math.Pi)).Float32()
	require.Equal(t, float32(math.Pi), v10)
	require.True(t, ok, "Float32 roundtrip")
	v11, ok := Float64(math.Pi).Float64()
	require.Equal(t, math.Pi, v11)
	require.True(t, ok, "Float64 roundtrip")
	v12, ok := String("hello").String()
	require.Equal(t, "hello", v12)
	require.True(t, ok, "String roundtrip")
	v13, ok := DateValue(19000).Date()
	require.Equal(t, Date(19000), v13)
	require.True(t, ok, "Date roundtrip")
	v14, ok := TimeValue(86399999999999).Time()
	require.Equal(t, TimeOfDay(86399999999999), v14)
	require.True(t, ok, "Time roundtrip")
	d, ok := DecimalValue(Decimal{Unscaled: big.NewInt(-12345), Scale: 3}).Decimal()
	require.True(t, ok, "Decimal roundtrip")
	require.Equal(t, int64(-12345), d.Unscaled.Int64())
	require.Equal(t, int32(3), d.Scale)
}

func TestValueBytesCopySemantics(t *testing.T) {
	src := []byte{1, 2, 3}
	v := Bytes(src)
	src[0] = 0xFF // constructor must have copied
	got, ok := v.Bytes()
	require.True(t, ok)
	require.Equal(t, byte(1), got[0], "Bytes not copied at construction: %v", got)
	got[0] = 0x77 // getter must return a copy too
	got2, _ := v.Bytes()
	require.Equal(t, byte(1), got2[0], "Bytes getter aliases internal state")
}

func TestValueDateTimePrecision(t *testing.T) {
	tm := time.Date(2024, 3, 15, 10, 30, 15, 123456789, time.UTC)
	v := DateTime(tm)
	got, ok := v.DateTimeValue()
	require.True(t, ok)
	require.Equal(t, 123456789, got.Nanosecond(), "DateTime precision lost: %v", got)
	// Non-UTC input is converted to UTC.
	loc := time.FixedZone("x", 3600)
	v2 := DateTime(time.Date(2024, 3, 15, 10, 30, 15, 0, loc))
	got2, _ := v2.DateTimeValue()
	require.Equal(t, 9, got2.UTC().Hour(), "DateTime not normalized to UTC: %v", got2)
}

func TestNewDateAndTimeOfDay(t *testing.T) {
	// 2024-03-15 UTC is 19797 days after the epoch.
	got := NewDate(time.Date(2024, 3, 15, 23, 59, 0, 0, time.UTC))
	require.Equal(t, Date(19797), got)
	tod, err := NewTimeOfDay(23, 59, 59, 999999999)
	require.NoError(t, err)
	require.Equal(t, TimeOfDay(23*3600e9+59*60e9+59e9+999999999), tod)
	for _, bad := range [][4]int{{24, 0, 0, 0}, {-1, 0, 0, 0}, {0, 60, 0, 0}, {0, 0, 60, 0}, {0, 0, 0, 1000000000}, {0, 0, 0, -1}} {
		_, err := NewTimeOfDay(bad[0], bad[1], bad[2], bad[3])
		require.Error(t, err, "NewTimeOfDay%v accepted", bad)
	}
}
