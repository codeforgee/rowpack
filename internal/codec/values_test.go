package codec

import (
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"
)

func schemaOf(cols ...Column) *Schema {
	return &Schema{TableID: 1, Version: 1, Name: "t", Columns: cols}
}

// TestDecimalCanonicalEncoding exercises the minimal big-endian two's
// complement encoding at its boundaries.
func TestDecimalCanonicalEncoding(t *testing.T) {
	cases := []struct {
		val  int64
		want string // hex bytes
	}{
		{0, "00"},
		{1, "01"},
		{127, "7f"},
		{128, "0080"},
		{255, "00ff"},
		{256, "0100"},
		{32767, "7fff"},
		{32768, "008000"},
		{-1, "ff"},
		{-128, "80"},
		{-129, "ff7f"},
		{-255, "ff01"},
		{-256, "ff00"},
		{-32768, "8000"},
		{-32769, "ff7fff"},
		{65535, "00ffff"},
		{-65536, "ff0000"},
	}
	for _, c := range cases {
		u := big.NewInt(c.val)
		b, err := encodeDecimalBytes(u)
		if err != nil {
			t.Fatalf("%d: %v", c.val, err)
		}
		if got := toHex(b); got != c.want {
			t.Errorf("encode %d = %s, want %s", c.val, got, c.want)
		}
		back, err := decodeDecimalBytes(b)
		if err != nil {
			t.Fatalf("decode %s: %v", c.want, err)
		}
		if back.Int64() != c.val {
			t.Errorf("decode %s = %d, want %d", c.want, back.Int64(), c.val)
		}
	}
}

func toHex(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, hexd[x>>4], hexd[x&0xF])
	}
	return string(out)
}

func TestDecimalRejectsNonCanonical(t *testing.T) {
	bad := [][]byte{
		{0x00, 0x7f}, // redundant leading 00
		{0xff, 0xff}, // redundant leading ff
		{0xff, 0x80}, // redundant leading ff
		{0x00, 0x00}, // redundant leading 00 (zero must be single 00)
		{0x00, 0x01}, // redundant leading 00
	}
	for _, b := range bad {
		if _, err := decodeDecimalBytes(b); err == nil {
			t.Errorf("accepted non-canonical decimal %x", b)
		}
	}
}

func TestDateConversions(t *testing.T) {
	cases := []struct {
		t    time.Time
		want int64 // days
	}{
		{time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), 0},
		{time.Date(1970, 1, 2, 23, 59, 59, 0, time.UTC), 1},
		{time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC), -1},
		{time.Date(2000, 2, 29, 12, 0, 0, 0, time.UTC), 11016},
		{time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), 19723},
		{time.Date(2038, 1, 19, 3, 14, 7, 0, time.UTC), 24855},
	}
	for _, c := range cases {
		got := NewDate(c.t)
		if int64(got) != c.want {
			t.Errorf("NewDate(%v) = %d, want %d", c.t, got, c.want)
		}
		back := got.Time(time.UTC)
		wantDay := time.Date(c.t.Year(), c.t.Month(), c.t.Day(), 0, 0, 0, 0, time.UTC)
		if !back.Equal(wantDay) {
			t.Errorf("Date(%d).Time() = %v, want %v", got, back, wantDay)
		}
	}
}

func TestTimeOfDay(t *testing.T) {
	v, err := NewTimeOfDay(23, 59, 59, 999_999_999)
	if err != nil {
		t.Fatal(err)
	}
	if v.Hour() != 23 || v.Minute() != 59 || v.Second() != 59 || v.Nanosecond() != 999_999_999 {
		t.Fatalf("components = %d:%d:%d.%d", v.Hour(), v.Minute(), v.Second(), v.Nanosecond())
	}
	if _, err := NewTimeOfDay(24, 0, 0, 0); err == nil {
		t.Fatal("accepted hour 24")
	}
	if _, err := NewTimeOfDay(0, 60, 0, 0); err == nil {
		t.Fatal("accepted minute 60")
	}
	if _, err := NewTimeOfDay(0, 0, -1, 0); err == nil {
		t.Fatal("accepted negative second")
	}
}

func TestValueConstructors(t *testing.T) {
	vals := []Value{
		Null(), Bool(true), Int8(-128), Int16(-32768), Int32(math.MinInt32), Int64(math.MinInt64),
		Uint8(255), Uint16(65535), Uint32(math.MaxUint32), Uint64(math.MaxUint64),
		Float32(float32(math.Pi)), Float64(math.E), String("张三"), Bytes([]byte{1, 2, 3}),
		DateValue(19723), TimeValue(TimeOfDay(86400e9 - 1)), DateTime(time.Date(2024, 3, 1, 1, 2, 3, 456, time.UTC)),
		DecimalValue(Decimal{Unscaled: big.NewInt(-123456), Scale: 3}),
	}
	for _, v := range vals {
		if v.IsNull() && v.Type() != 0 && v.Type() != TypeDecimal {
			// Null carries no type; skip.
		}
	}
	if b, ok := vals[1].Bool(); !ok || !b {
		t.Fatal("Bool getter failed")
	}
	if i, ok := vals[2].Int8(); !ok || i != -128 {
		t.Fatal("Int8 getter failed")
	}
	if u, ok := vals[6].Uint8(); !ok || u != 255 {
		t.Fatal("Uint8 getter failed")
	}
	if s, ok := vals[12].String(); !ok || s != "张三" {
		t.Fatal("String getter failed")
	}
	if b, ok := vals[13].Bytes(); !ok || !reflect.DeepEqual(b, []byte{1, 2, 3}) {
		t.Fatal("Bytes getter failed")
	}
	if tm, ok := vals[16].DateTimeValue(); !ok || tm.UnixNano() != time.Date(2024, 3, 1, 1, 2, 3, 456, time.UTC).UnixNano() {
		t.Fatal("DateTime getter failed")
	}
	if d, ok := vals[17].Decimal(); !ok || d.Scale != 3 || d.Unscaled.Int64() != -123456 {
		t.Fatal("Decimal getter failed")
	}
}

func TestBytesImmutability(t *testing.T) {
	src := []byte{1, 2, 3}
	v := Bytes(src)
	src[0] = 99
	got, _ := v.Bytes()
	if got[0] != 1 {
		t.Fatal("Bytes constructor did not copy input")
	}
	got[0] = 42
	got2, _ := v.Bytes()
	if got2[0] != 1 {
		t.Fatal("Bytes getter returned aliased buffer")
	}
}

func TestDateTimeNormalization(t *testing.T) {
	loc := time.FixedZone("X", 8*3600)
	tm := time.Date(2024, 1, 1, 9, 30, 0, 123, loc)
	v := DateTime(tm)
	out, _ := v.DateTimeValue()
	want := time.Date(2024, 1, 1, 1, 30, 0, 123, time.UTC)
	if !out.Equal(want) {
		t.Fatalf("DateTime = %v, want %v", out, want)
	}
	if out.Location() != time.UTC {
		t.Fatalf("DateTime not normalized to UTC: %v", out.Location())
	}
}

func TestFloat32NaNBitPreservation(t *testing.T) {
	payload := uint32(0x7FC0_1234) // NaN with payload
	f := math.Float32frombits(payload)
	v := Float32(f)
	back, _ := v.Float32()
	if math.Float32bits(back) != payload {
		t.Fatalf("NaN payload not preserved: %08x vs %08x", math.Float32bits(back), payload)
	}
}
