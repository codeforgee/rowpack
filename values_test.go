package rowpack

import (
	"math"
	"math/big"
	"testing"
	"time"
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
		if tc.v.IsNull() != (tc.name == "null") {
			t.Fatalf("%s: IsNull = %v", tc.name, tc.v.IsNull())
		}
		if tc.name != "null" && tc.v.Type() == 0 && tc.name != "bool" {
			// Type 0 is TypeBool; only the bool value may carry it.
			if tc.name != "bool" {
				t.Fatalf("%s: unexpected zero type", tc.name)
			}
		}
	}
}

func TestValueGettersRoundTrip(t *testing.T) {
	if v, ok := Bool(true).Bool(); !v || !ok {
		t.Fatal("Bool roundtrip")
	}
	if v, ok := Int8(-128).Int8(); v != -128 || !ok {
		t.Fatal("Int8 roundtrip")
	}
	if v, ok := Int16(-32768).Int16(); v != -32768 || !ok {
		t.Fatal("Int16 roundtrip")
	}
	if v, ok := Int32(math.MinInt32).Int32(); v != math.MinInt32 || !ok {
		t.Fatal("Int32 roundtrip")
	}
	if v, ok := Int64(math.MinInt64).Int64(); v != math.MinInt64 || !ok {
		t.Fatal("Int64 roundtrip")
	}
	if v, ok := Uint8(255).Uint8(); v != 255 || !ok {
		t.Fatal("Uint8 roundtrip")
	}
	if v, ok := Uint16(65535).Uint16(); v != 65535 || !ok {
		t.Fatal("Uint16 roundtrip")
	}
	if v, ok := Uint32(math.MaxUint32).Uint32(); v != math.MaxUint32 || !ok {
		t.Fatal("Uint32 roundtrip")
	}
	if v, ok := Uint64(math.MaxUint64).Uint64(); v != math.MaxUint64 || !ok {
		t.Fatal("Uint64 roundtrip")
	}
	if v, ok := Float32(float32(math.Pi)).Float32(); v != float32(math.Pi) || !ok {
		t.Fatal("Float32 roundtrip")
	}
	if v, ok := Float64(math.Pi).Float64(); v != math.Pi || !ok {
		t.Fatal("Float64 roundtrip")
	}
	if v, ok := String("hello").String(); v != "hello" || !ok {
		t.Fatal("String roundtrip")
	}
	if v, ok := DateValue(19000).Date(); v != 19000 || !ok {
		t.Fatal("Date roundtrip")
	}
	if v, ok := TimeValue(86399999999999).Time(); v != 86399999999999 || !ok {
		t.Fatal("Time roundtrip")
	}
	if v, ok := DecimalValue(Decimal{Unscaled: big.NewInt(-12345), Scale: 3}).Decimal(); !ok || v.Unscaled.Int64() != -12345 || v.Scale != 3 {
		t.Fatal("Decimal roundtrip")
	}
}

func TestValueBytesCopySemantics(t *testing.T) {
	src := []byte{1, 2, 3}
	v := Bytes(src)
	src[0] = 0xFF // constructor must have copied
	got, ok := v.Bytes()
	if !ok || got[0] != 1 {
		t.Fatalf("Bytes not copied at construction: %v", got)
	}
	got[0] = 0x77 // getter must return a copy too
	got2, _ := v.Bytes()
	if got2[0] != 1 {
		t.Fatal("Bytes getter aliases internal state")
	}
}

func TestValueDateTimePrecision(t *testing.T) {
	tm := time.Date(2024, 3, 15, 10, 30, 15, 123456789, time.UTC)
	v := DateTime(tm)
	got, ok := v.DateTimeValue()
	if !ok || got.Nanosecond() != 123456789 {
		t.Fatalf("DateTime precision lost: %v", got)
	}
	// Non-UTC input is converted to UTC.
	loc := time.FixedZone("x", 3600)
	v2 := DateTime(time.Date(2024, 3, 15, 10, 30, 15, 0, loc))
	got2, _ := v2.DateTimeValue()
	if got2.UTC().Hour() != 9 {
		t.Fatalf("DateTime not normalized to UTC: %v", got2)
	}
}

func TestNewDateAndTimeOfDay(t *testing.T) {
	// 2024-03-15 UTC is 19797 days after the epoch.
	got := NewDate(time.Date(2024, 3, 15, 23, 59, 0, 0, time.UTC))
	if got != 19797 {
		t.Fatalf("NewDate = %d, want 19797", got)
	}
	tod, err := NewTimeOfDay(23, 59, 59, 999999999)
	if err != nil || tod != 23*3600e9+59*60e9+59e9+999999999 {
		t.Fatalf("NewTimeOfDay = %d, %v", tod, err)
	}
	for _, bad := range [][4]int{{24, 0, 0, 0}, {-1, 0, 0, 0}, {0, 60, 0, 0}, {0, 0, 60, 0}, {0, 0, 0, 1000000000}, {0, 0, 0, -1}} {
		if _, err := NewTimeOfDay(bad[0], bad[1], bad[2], bad[3]); err == nil {
			t.Fatalf("NewTimeOfDay%v accepted", bad)
		}
	}
}
