package codec

import (
	"errors"
	"fmt"
	"math/big"
	"time"
)

// NewDate converts t to a calendar date (days since the Unix epoch) in the
// proleptic Gregorian calendar. The time-of-day part is ignored.
func NewDate(t time.Time) Date {
	_, off := t.Zone()
	local := t.Add(time.Duration(off) * time.Second)
	y, m, d := local.Date()
	return Date(daysFromCivil(y, int(m), d))
}

// Time converts d to a time.Time at 00:00:00 UTC in the given location.
func (d Date) Time(loc *time.Location) time.Time {
	y, m, day := civilFromDays(int(d))
	return time.Date(y, time.Month(m), day, 0, 0, 0, 0, loc)
}

// Unix returns the Unix seconds of the date at 00:00:00 UTC.
func (d Date) Unix() int64 {
	y, m, day := civilFromDays(int(d))
	return time.Date(y, time.Month(m), day, 0, 0, 0, 0, time.UTC).Unix()
}

// daysFromCivil returns the number of days since 1970-01-01 for a civil
// (proleptic Gregorian) date. This is Howard Hinnant's days_from_civil.
func daysFromCivil(y, m, d int) int {
	if m <= 2 {
		y--
	}
	era := (y - (y % 400)) / 400
	if y < 0 && y%400 != 0 {
		era--
	}
	yoe := y - era*400
	mp := m + 9
	if m > 2 {
		mp = m - 3
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// civilFromDays inverts daysFromCivil (Hinnant's civil_from_days).
func civilFromDays(z int) (y, m, d int) {
	z += 719468
	era := z / 146097
	if z < 0 && z%146097 != 0 {
		era--
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if m > 12 {
		m -= 12
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

// MaxTimeOfDay is the exclusive upper bound of a TimeOfDay in nanoseconds.
const MaxTimeOfDay = int64(24 * 60 * 60 * 1_000_000_000)

// NewTimeOfDay builds a TimeOfDay from hour/minute/second/nanosecond,
// validating each field's range.
func NewTimeOfDay(hour, min, sec, nsec int) (TimeOfDay, error) {
	if hour < 0 || hour > 23 {
		return 0, fmt.Errorf("rowpack: hour %d out of range [0,24)", hour)
	}
	if min < 0 || min > 59 {
		return 0, fmt.Errorf("rowpack: minute %d out of range", min)
	}
	if sec < 0 || sec > 59 {
		return 0, fmt.Errorf("rowpack: second %d out of range", sec)
	}
	if nsec < 0 || nsec >= 1_000_000_000 {
		return 0, fmt.Errorf("rowpack: nanosecond %d out of range", nsec)
	}
	ns := int64(hour)*3600e9 + int64(min)*60e9 + int64(sec)*1e9 + int64(nsec)
	return TimeOfDay(ns), nil
}

// Hour returns the hour component.
func (t TimeOfDay) Hour() int { return int(int64(t) / 3600e9) }

// Minute returns the minute component.
func (t TimeOfDay) Minute() int { return int(int64(t) % 3600e9 / 60e9) }

// Second returns the second component.
func (t TimeOfDay) Second() int { return int(int64(t) % 60e9 / 1e9) }

// Nanosecond returns the nanosecond component.
func (t TimeOfDay) Nanosecond() int { return int(int64(t) % 1e9) }

// Time converts t to a time.Time on 1970-01-01 in UTC.
func (t TimeOfDay) Time() time.Time {
	return time.Unix(0, int64(t)).UTC()
}

var _ = errors.New

// ---- Decimal canonical big-endian two's-complement encoding ----

// decimalMaxBytes caps the serialized unscaled integer length.
const decimalMaxBytes = 1 << 20 // 1 MiB, well above practical decimals

// encodeDecimalBytes returns the canonical minimal big-endian two's-complement
// bytes of u: no redundant 0x00 (positive) or 0xFF (negative) sign-extension
// bytes, and zero encoded as a single 0x00 byte.
func encodeDecimalBytes(u *big.Int) ([]byte, error) {
	if u == nil {
		return nil, errors.New("rowpack: decimal unscaled is nil")
	}
	if u.Sign() == 0 {
		return []byte{0x00}, nil
	}
	raw := u.Bytes() // magnitude, big-endian
	if u.Sign() > 0 {
		// Positive: ensure the top byte's high bit is clear, else prepend 0x00.
		if raw[0]&0x80 != 0 {
			return append([]byte{0x00}, raw...), nil
		}
		return raw, nil
	}
	// Negative: find the minimal two's-complement width w in bytes. A negative
	// value with magnitude m fits in w bytes iff m <= 2^(8w-1).
	m := new(big.Int).Neg(u)
	b1 := new(big.Int).Sub(m, big.NewInt(1))
	w := (b1.BitLen() + 8) / 8
	mod := new(big.Int).Lsh(big.NewInt(1), uint(8*w))
	v := new(big.Int).Add(u, mod)
	tc := v.Bytes()
	if len(tc) < w {
		pad := make([]byte, w-len(tc))
		tc = append(pad, tc...)
	}
	// Strip redundant leading 0xFF bytes while the next byte's high bit is set.
	for len(tc) > 1 && tc[0] == 0xFF && tc[1]&0x80 != 0 {
		tc = tc[1:]
	}
	return tc, nil
}

// decodeDecimalBytes interprets canonical two's-complement bytes.
func decodeDecimalBytes(b []byte) (*big.Int, error) {
	if len(b) == 0 {
		return nil, errors.New("rowpack: empty decimal unscaled")
	}
	x := new(big.Int).SetBytes(b)
	if b[0]&0x80 != 0 {
		// Sign-extend: subtract 2^(8*len).
		mod := new(big.Int).Lsh(big.NewInt(1), uint(8*len(b)))
		x.Sub(x, mod)
	}
	// Reject non-canonical encodings (redundant sign-extension bytes).
	back, err := encodeDecimalBytes(x)
	if err != nil {
		return nil, err
	}
	if len(back) != len(b) || !bytesEqual(back, b) {
		return nil, errors.New("rowpack: non-canonical decimal encoding")
	}
	return x, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
