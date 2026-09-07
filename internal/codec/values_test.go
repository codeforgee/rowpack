package codec

import (
	"encoding/binary"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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
		require.NoError(t, err, "%d", c.val)
		require.Equal(t, c.want, toHex(b), "encode %d = %s, want %s", c.val, toHex(b), c.want)
		back, err := decodeDecimalBytes(b)
		require.NoError(t, err, "decode %s", c.want)
		require.Equal(t, c.val, back.Int64(), "decode %s = %d, want %d", c.want, back.Int64(), c.val)
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
		_, err := decodeDecimalBytes(b)
		require.Error(t, err, "accepted non-canonical decimal %x", b)
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
		require.Equal(t, c.want, int64(got), "NewDate(%v) = %d, want %d", c.t, got, c.want)
		back := got.Time(time.UTC)
		wantDay := time.Date(c.t.Year(), c.t.Month(), c.t.Day(), 0, 0, 0, 0, time.UTC)
		require.True(t, back.Equal(wantDay), "Date(%d).Time() = %v, want %v", got, back, wantDay)
	}
}

func TestTimeOfDay(t *testing.T) {
	v, err := NewTimeOfDay(23, 59, 59, 999_999_999)
	require.NoError(t, err)
	require.Equal(t, 23, v.Hour(), "components = %d:%d:%d.%d", v.Hour(), v.Minute(), v.Second(), v.Nanosecond())
	require.Equal(t, 59, v.Minute(), "components = %d:%d:%d.%d", v.Hour(), v.Minute(), v.Second(), v.Nanosecond())
	require.Equal(t, 59, v.Second(), "components = %d:%d:%d.%d", v.Hour(), v.Minute(), v.Second(), v.Nanosecond())
	require.Equal(t, 999_999_999, v.Nanosecond(), "components = %d:%d:%d.%d", v.Hour(), v.Minute(), v.Second(), v.Nanosecond())
	_, err = NewTimeOfDay(24, 0, 0, 0)
	require.Error(t, err, "accepted hour 24")
	_, err = NewTimeOfDay(0, 60, 0, 0)
	require.Error(t, err, "accepted minute 60")
	_, err = NewTimeOfDay(0, 0, -1, 0)
	require.Error(t, err, "accepted negative second")
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
	b, ok := vals[1].Bool()
	require.True(t, ok && b, "Bool getter failed")
	i, ok := vals[2].Int8()
	require.True(t, ok && i == -128, "Int8 getter failed")
	u, ok := vals[6].Uint8()
	require.True(t, ok && u == 255, "Uint8 getter failed")
	s, ok := vals[12].String()
	require.True(t, ok && s == "张三", "String getter failed")
	bb, ok := vals[13].Bytes()
	require.True(t, ok && reflect.DeepEqual(bb, []byte{1, 2, 3}), "Bytes getter failed")
	tm, ok := vals[16].DateTimeValue()
	require.True(t, ok && tm.UnixNano() == time.Date(2024, 3, 1, 1, 2, 3, 456, time.UTC).UnixNano(), "DateTime getter failed")
	d, ok := vals[17].Decimal()
	require.True(t, ok && d.Scale == 3 && d.Unscaled.Int64() == -123456, "Decimal getter failed")
}

func TestBytesImmutability(t *testing.T) {
	src := []byte{1, 2, 3}
	v := Bytes(src)
	src[0] = 99
	got, _ := v.Bytes()
	require.Equal(t, byte(1), got[0], "Bytes constructor did not copy input")
	got[0] = 42
	got2, _ := v.Bytes()
	require.Equal(t, byte(1), got2[0], "Bytes getter returned aliased buffer")
}

func TestDateTimeNormalization(t *testing.T) {
	loc := time.FixedZone("X", 8*3600)
	tm := time.Date(2024, 1, 1, 9, 30, 0, 123, loc)
	v := DateTime(tm)
	out, _ := v.DateTimeValue()
	want := time.Date(2024, 1, 1, 1, 30, 0, 123, time.UTC)
	require.True(t, out.Equal(want), "DateTime = %v, want %v", out, want)
	require.Equal(t, time.UTC, out.Location(), "DateTime not normalized to UTC: %v", out.Location())
}

func TestFloat32NaNBitPreservation(t *testing.T) {
	payload := uint32(0x7FC0_1234) // NaN with payload
	f := math.Float32frombits(payload)
	v := Float32(f)
	back, _ := v.Float32()
	require.Equal(t, payload, math.Float32bits(back), "NaN payload not preserved: %08x vs %08x", math.Float32bits(back), payload)
}

// TestAppendDecimalEquivalence asserts the zero-allocation write path
// (appendDecimalBytes) produces byte-identical output to the reference
// encoder (encodeDecimalBytes) at boundaries and across random int64 values
// (V1.1-B).
func TestAppendDecimalEquivalence(t *testing.T) {
	boundaries := []int64{
		0, 1, -1, 127, 128, 255, 256, 32767, 32768, 65535, 65536,
		-128, -129, -255, -256, -32768, -32769, -65536,
		1 << 40, -(1 << 40), math.MaxInt64, math.MinInt64, math.MinInt64 + 1,
	}
	rng := uint64(88172645463325252)
	for i := 0; i < 1000; i++ {
		rng = rng*6364136223846793005 + 1442695040888963407
		boundaries = append(boundaries, int64(rng), int64(rng>>1))
	}
	for _, v := range boundaries {
		u := big.NewInt(v)
		want, err := encodeDecimalBytes(u)
		require.NoError(t, err, "%d", v)
		got, err := appendDecimalBytes(nil, u, 1<<20)
		require.NoError(t, err, "%d", v)
		require.Equal(t, toHex(want), toHex(got[4:]), "%d: inline %s != reference %s", v, toHex(got[4:]), toHex(want))
		require.Equal(t, uint32(len(want)), binary.LittleEndian.Uint32(got[:4]), "%d: length prefix %d != %d", v, binary.LittleEndian.Uint32(got[:4]), len(want))
		// Round-trip through the decoder.
		back, err := decodeDecimalBytes(want)
		require.NoError(t, err, "%d: decode", v)
		require.Equal(t, v, back.Int64(), "%d: round trip got %d", v, back.Int64())
	}
}
