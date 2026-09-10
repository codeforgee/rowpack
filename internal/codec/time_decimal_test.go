package codec

import (
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewDate(t *testing.T) {
	t.Run("basic date", func(t *testing.T) {
		loc, _ := time.LoadLocation("UTC")
		dt := time.Date(2024, 1, 15, 12, 30, 45, 0, loc)
		d := NewDate(dt)
		y, m, day := civilFromDays(int(d))
		require.Equal(t, 2024, y)
		require.Equal(t, 1, m)
		require.Equal(t, 15, day)
	})

	t.Run("ignores time of day", func(t *testing.T) {
		loc, _ := time.LoadLocation("UTC")
		dt1 := time.Date(2024, 1, 15, 0, 0, 0, 0, loc)
		dt2 := time.Date(2024, 1, 15, 23, 59, 59, 999999999, loc)
		d1 := NewDate(dt1)
		d2 := NewDate(dt2)
		require.Equal(t, d1, d2)
	})

	t.Run("epoch date", func(t *testing.T) {
		loc, _ := time.LoadLocation("UTC")
		dt := time.Date(1970, 1, 1, 0, 0, 0, 0, loc)
		d := NewDate(dt)
		require.Equal(t, Date(0), d)
	})

	t.Run("pre-epoch date", func(t *testing.T) {
		loc, _ := time.LoadLocation("UTC")
		dt := time.Date(1969, 12, 31, 0, 0, 0, 0, loc)
		d := NewDate(dt)
		require.Equal(t, Date(-1), d)
	})
}

func TestDateMethods(t *testing.T) {
	// Use a date that's timezone-independent for testing
	// 19737 days since epoch = 2024-01-15
	d := Date(19737)

	t.Run("Time conversion", func(t *testing.T) {
		tm := d.Time(time.UTC)
		require.Equal(t, 2024, tm.Year())
		require.Equal(t, time.January, tm.Month())
		require.Equal(t, 15, tm.Day())
		require.Equal(t, time.UTC, tm.Location())
	})

	t.Run("Unix timestamp", func(t *testing.T) {
		unix := d.Unix()
		// 19737 days since epoch = 2024-01-15
		require.Equal(t, int64(1705276800), unix)
	})

	t.Run("civilFromDays roundtrip", func(t *testing.T) {
		y, m, day := civilFromDays(int(d))
		require.Equal(t, 2024, y)
		require.Equal(t, 1, m)
		require.Equal(t, 15, day)
	})
}

func TestDaysFromCivil(t *testing.T) {
	testCases := []struct {
		name    string
		y, m, d int
		want    int
	}{
		{"epoch", 1970, 1, 1, 0},
		{"leap year", 2000, 1, 1, 10957},
		{"pre-epoch", 1969, 12, 31, -1},
		{"regular", 2024, 1, 15, 19737},
		{"end of year", 2024, 12, 31, 20088},
		{"far future", 2100, 1, 1, 47482},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := daysFromCivil(tc.y, tc.m, tc.d)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestCivilFromDays(t *testing.T) {
	testCases := []struct {
		name                string
		z                   int
		wantY, wantM, wantD int
	}{
		{"epoch", 0, 1970, 1, 1},
		{"leap year", 10957, 2000, 1, 1},
		{"pre-epoch", -1, 1969, 12, 31},
		{"regular", 19737, 2024, 1, 15},
		{"end of year", 20088, 2024, 12, 31},
		{"far future", 47482, 2100, 1, 1},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			y, m, d := civilFromDays(tc.z)
			require.Equal(t, tc.wantY, y)
			require.Equal(t, tc.wantM, m)
			require.Equal(t, tc.wantD, d)
		})
	}
}

func TestNewTimeOfDay(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		tod, err := NewTimeOfDay(12, 30, 45, 123456789)
		require.NoError(t, err)
		require.Equal(t, TimeOfDay(int64(12)*3600e9+int64(30)*60e9+int64(45)*1e9+123456789), tod)
	})

	t.Run("midnight", func(t *testing.T) {
		tod, err := NewTimeOfDay(0, 0, 0, 0)
		require.NoError(t, err)
		require.Equal(t, TimeOfDay(0), tod)
	})

	t.Run("end of day", func(t *testing.T) {
		tod, err := NewTimeOfDay(23, 59, 59, 999999999)
		require.NoError(t, err)
		require.Equal(t, TimeOfDay(86399999999999), tod)
	})

	t.Run("invalid hour", func(t *testing.T) {
		_, err := NewTimeOfDay(24, 0, 0, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "hour")

		_, err = NewTimeOfDay(-1, 0, 0, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "hour")
	})

	t.Run("invalid minute", func(t *testing.T) {
		_, err := NewTimeOfDay(12, 60, 0, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "minute")

		_, err = NewTimeOfDay(12, -1, 0, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "minute")
	})

	t.Run("invalid second", func(t *testing.T) {
		_, err := NewTimeOfDay(12, 30, 60, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "second")

		_, err = NewTimeOfDay(12, 30, -1, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "second")
	})

	t.Run("invalid nanosecond", func(t *testing.T) {
		_, err := NewTimeOfDay(12, 30, 45, 1000000000)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nanosecond")

		_, err = NewTimeOfDay(12, 30, 45, -1)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nanosecond")
	})
}

func TestTimeOfDayMethods(t *testing.T) {
	tod := TimeOfDay(int64(12)*3600e9 + int64(30)*60e9 + int64(45)*1e9 + 123456789)

	require.Equal(t, 12, tod.Hour())
	require.Equal(t, 30, tod.Minute())
	require.Equal(t, 45, tod.Second())
	require.Equal(t, 123456789, tod.Nanosecond())

	tm := tod.Time()
	require.Equal(t, int64(12)*3600e9+int64(30)*60e9+int64(45)*1e9+123456789, tm.UnixNano())
	require.Equal(t, time.UTC, tm.Location())
}

func TestMaxTimeOfDay(t *testing.T) {
	require.Equal(t, int64(24*60*60*1_000_000_000), int64(MaxTimeOfDay))
}

func TestDecimalEncoding(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		u := big.NewInt(0)
		buf, err := appendDecimalBytes(nil, u, 100)
		require.NoError(t, err)
		// u32 length (1) + 1 byte data (0x00)
		require.Equal(t, []byte{1, 0, 0, 0, 0x00}, buf)
	})

	t.Run("positive fits int64", func(t *testing.T) {
		u := big.NewInt(12345)
		buf, err := appendDecimalBytes(nil, u, 100)
		require.NoError(t, err)
		// u32 length (2) + 2 bytes data (0x30, 0x39)
		require.Equal(t, []byte{2, 0, 0, 0, 0x30, 0x39}, buf)
	})

	t.Run("negative fits int64", func(t *testing.T) {
		u := big.NewInt(-12345)
		_, err := appendDecimalBytes(nil, u, 100)
		require.NoError(t, err)
	})

	t.Run("large positive", func(t *testing.T) {
		u := new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)
		_, err := appendDecimalBytes(nil, u, 100)
		require.NoError(t, err)
	})

	t.Run("large negative", func(t *testing.T) {
		u := new(big.Int).Neg(new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
		_, err := appendDecimalBytes(nil, u, 100)
		require.NoError(t, err)
	})

	t.Run("exceeds max value", func(t *testing.T) {
		u := new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)
		_, err := appendDecimalBytes(nil, u, 5)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})

	t.Run("nil unscaled", func(t *testing.T) {
		_, err := appendDecimalBytes(nil, nil, 100)
		require.Error(t, err)
		require.Contains(t, err.Error(), "nil")
	})
}

func TestDecimalInt64Len(t *testing.T) {
	testCases := []struct {
		v    int64
		want int
	}{
		{0, 1},
		{1, 1},
		{127, 1},
		{128, 2},
		{255, 2},
		{256, 2},
		{65535, 3},
		{65536, 3},
		{-1, 1},
		{-128, 1},
		{-129, 2},
		{-256, 2},
		{-32768, 2},
		{-32769, 3},
		{-2147483648, 4},
		{9223372036854775807, 8},
		{-9223372036854775808, 8},
	}

	for _, tc := range testCases {
		t.Run("", func(t *testing.T) {
			got := decimalInt64Len(tc.v)
			require.Equal(t, tc.want, got, "value=%d", tc.v)
		})
	}
}

func TestAppendDecimalInt64Into(t *testing.T) {
	testCases := []struct {
		name string
		v    int64
		want []byte
	}{
		{"zero", 0, []byte{0x00}},
		{"positive small", 1, []byte{0x01}},
		{"positive 127", 127, []byte{0x7f}},
		{"positive 128", 128, []byte{0x00, 0x80}},
		{"positive 255", 255, []byte{0x00, 0xff}},
		{"positive 256", 256, []byte{0x01, 0x00}},
		{"negative -1", -1, []byte{0xff}},
		{"negative -128", -128, []byte{0x80}},
		{"negative -129", -129, []byte{0xff, 0x7f}},
		{"negative -256", -256, []byte{0xff, 0x00}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			buf := appendDecimalInt64Into(nil, tc.v)
			require.Equal(t, tc.want, buf)
		})
	}
}

func TestEncodeDecimalBig(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		u := big.NewInt(0)
		raw, err := encodeDecimalBig(u)
		require.NoError(t, err)
		require.Equal(t, []byte{0x00}, raw)
	})

	t.Run("positive no leading 0x80", func(t *testing.T) {
		u := big.NewInt(12345)
		raw, err := encodeDecimalBig(u)
		require.NoError(t, err)
		require.Equal(t, []byte{0x30, 0x39}, raw)
	})

	t.Run("positive with leading 0x80", func(t *testing.T) {
		u := big.NewInt(128)
		raw, err := encodeDecimalBig(u)
		require.NoError(t, err)
		require.Equal(t, []byte{0x00, 0x80}, raw)
	})

	t.Run("negative", func(t *testing.T) {
		u := big.NewInt(-1)
		raw, err := encodeDecimalBig(u)
		require.NoError(t, err)
		require.Equal(t, []byte{0xff}, raw)
	})

	t.Run("negative multi-byte", func(t *testing.T) {
		u := big.NewInt(-129)
		raw, err := encodeDecimalBig(u)
		require.NoError(t, err)
		require.Equal(t, []byte{0xff, 0x7f}, raw)
	})
}

func TestCanonicalDecimalBytes(t *testing.T) {
	testCases := []struct {
		name  string
		b     []byte
		valid bool
	}{
		{"zero", []byte{0x00}, true},
		{"positive single", []byte{0x01}, true},
		{"positive multi", []byte{0x12, 0x34}, true},
		{"positive with leading zero invalid", []byte{0x00, 0x12}, false},
		{"positive with leading zero valid", []byte{0x00, 0x80}, true},
		{"negative single", []byte{0xff}, true},
		{"negative multi", []byte{0xff, 0x7f}, true},
		{"negative redundant 0xff", []byte{0xff, 0xff, 0x7f}, false},
		{"empty", []byte{}, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := canonicalDecimalBytes(tc.b)
			require.Equal(t, tc.valid, got)
		})
	}
}

func TestDecodeDecimalBytesInto(t *testing.T) {
	t.Run("zero", func(t *testing.T) {
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, []byte{0x00})
		require.NoError(t, err)
		require.Equal(t, int64(0), dst.Int64())
	})

	t.Run("positive", func(t *testing.T) {
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, []byte{0x30, 0x39})
		require.NoError(t, err)
		require.Equal(t, int64(12345), dst.Int64())
	})

	t.Run("negative", func(t *testing.T) {
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, []byte{0xff})
		require.NoError(t, err)
		require.Equal(t, int64(-1), dst.Int64())
	})

	t.Run("negative multi-byte", func(t *testing.T) {
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, []byte{0xff, 0x7f})
		require.NoError(t, err)
		require.Equal(t, int64(-129), dst.Int64())
	})

	t.Run("large positive", func(t *testing.T) {
		u := new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)
		raw, _ := encodeDecimalBig(u)
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, raw)
		require.NoError(t, err)
		require.Equal(t, u, dst)
	})

	t.Run("large negative", func(t *testing.T) {
		u := new(big.Int).Neg(new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil))
		raw, _ := encodeDecimalBig(u)
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, raw)
		require.NoError(t, err)
		require.Equal(t, u, dst)
	})

	t.Run("empty", func(t *testing.T) {
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, []byte{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty")
	})

	t.Run("non-canonical", func(t *testing.T) {
		dst := new(big.Int)
		err := decodeDecimalBytesInto(dst, []byte{0x00, 0x12})
		require.Error(t, err)
		require.Contains(t, err.Error(), "non-canonical")
	})
}
