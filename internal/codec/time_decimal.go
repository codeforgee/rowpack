package codec

import (
	"errors"
	"fmt"
	"math/big"
	"math/bits"
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

// ---- Decimal canonical big-endian two's-complement encoding ----

// appendDecimalBytes appends the canonical big-endian two's-complement bytes
// of u to buf (with a u32 length prefix) without allocating when u fits int64.
// It is the write-path counterpart of encodeDecimalBytes used by appendValue.
// maxValue bounds the raw unscaled length; the caller still enforces the full
// row limit.
func appendDecimalBytes(buf []byte, u *big.Int, maxValue uint32) ([]byte, error) {
	if u == nil {
		return nil, errors.New("rowpack: decimal unscaled is nil")
	}
	if u.IsInt64() {
		v := u.Int64()
		n := intLen(v)
		if uint32(n) > maxValue {
			return nil, fmt.Errorf("decimal unscaled of %d bytes exceeds limit %d", n, maxValue)
		}
		buf = appendU32(buf, uint32(n))
		return appendInt64(buf, v), nil
	}
	raw, err := encodeBig(u)
	if err != nil {
		return nil, err
	}
	if uint32(len(raw)) > maxValue {
		return nil, fmt.Errorf("decimal unscaled of %d bytes exceeds limit %d", len(raw), maxValue)
	}
	buf = appendU32(buf, uint32(len(raw)))
	return append(buf, raw...), nil
}

// intLen returns the canonical encoding length of an int64 decimal
// value: 1 for zero, ceil(bitlen/8) plus a leading 0x00 when the top byte's
// high bit is set for positives, and the minimal two's-complement width for
// negatives.
func intLen(v int64) int {
	switch {
	case v == 0:
		return 1
	case v > 0:
		n := bits.Len64(uint64(v))
		w := (n + 7) / 8
		if n%8 == 0 {
			w++ // top byte's high bit is set: prepend 0x00
		}
		return w
	default:
		w := 1
		for w < 8 && v < -(int64(1)<<(8*w-1)) {
			w++
		}
		return w
	}
}

// appendInt64 appends the canonical big-endian two's-complement
// bytes of v to buf without allocating. Byte output matches
// encodeDecimalInt64 exactly.
func appendInt64(buf []byte, v int64) []byte {
	switch {
	case v == 0:
		return append(buf, 0x00)
	case v > 0:
		n := bits.Len64(uint64(v))
		w := (n + 7) / 8
		if n%8 == 0 {
			buf = append(buf, 0x00)
		}
		x := uint64(v)
		for i := w - 1; i >= 0; i-- {
			buf = append(buf, byte(x>>(8*i)))
		}
		return buf
	default:
		w := 1
		for w < 8 && v < -(int64(1)<<(8*w-1)) {
			w++
		}
		x := uint64(v)
		for i := w - 1; i >= 0; i-- {
			buf = append(buf, byte(x>>(8*i)))
		}
		return buf
	}
}

// encodeBig is the general big.Int path for decimals that do not fit
// int64, preserving the original v1 canonical encoding.
func encodeBig(u *big.Int) ([]byte, error) {
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

// canonicalBytes checks the canonical big-endian two's-complement
// form without allocating big.Ints: no redundant leading 0x00 (positive), no
// redundant leading 0xFF (negative), and zero is a single 0x00 byte.
func canonicalBytes(b []byte) bool {
	n := len(b)
	if n == 0 {
		return false
	}
	if b[0]&0x80 != 0 {
		// Negative: a redundant sign-extension 0xFF byte is only valid when
		// the next byte's high bit is clear (i.e. the 0xFF carries a bit).
		if b[0] == 0xFF && n > 1 && b[1]&0x80 != 0 {
			return false
		}
		return true
	}
	// Non-negative: a leading 0x00 is only valid when it is needed to keep
	// the number's high bit clear (value >= 2^(8*(n-1))). Zero is a single
	// 0x00 byte.
	if n == 1 {
		return true
	}
	if b[0] == 0x00 && b[1]&0x80 == 0 {
		return false
	}
	return true
}

// decodeBytesInto interprets canonical two's-complement bytes into dst
// (reused across rows when the caller decodes into the same Value), avoiding
// the temporary big.Int chain of the general path. It validates canonical form
// and returns an error for non-canonical encodings.
func decodeBytesInto(dst *big.Int, b []byte) error {
	if len(b) == 0 {
		return errors.New("rowpack: empty decimal unscaled")
	}
	if !canonicalBytes(b) {
		return errors.New("rowpack: non-canonical decimal encoding")
	}
	if len(b) <= 8 {
		// Fast path: sign-extended int64, no big.Int allocation at all.
		var u uint64
		for _, c := range b {
			u = u<<8 | uint64(c)
		}
		if b[0]&0x80 != 0 && len(b) < 8 {
			u |= ^uint64(0) << (8 * len(b))
		}
		dst.SetInt64(int64(u))
		return nil
	}
	dst.SetBytes(b)
	if b[0]&0x80 != 0 {
		// Sign-extend: subtract 2^(8*len).
		mod := new(big.Int).Lsh(big.NewInt(1), uint(8*len(b)))
		dst.Sub(dst, mod)
	}
	return nil
}
