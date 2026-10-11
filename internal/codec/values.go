// Package codec implements the v1 TypedTuple row encoding and the strong-typed
// Value model that the public rowpack API re-exports.
//
// A Row is encoded strictly by its Schema: no per-value type tag is written;
// the type is implied by the column position. NULL is expressed by a bit
// bitmap. All lengths and ranges are validated before allocation so untrusted
// input can never panic.
package codec

import (
	"errors"
	"math"
	"math/big"
	"time"
	"unsafe"

	"github.com/codeforgee/rowpack/internal/format"
)

// Type is the logical value type (alias of the disk ValueType).
type Type = format.ValueType

// Value type constants, re-exported for use inside the codec package.
const (
	TypeBool     Type = format.TypeBool
	TypeInt8     Type = format.TypeInt8
	TypeInt16    Type = format.TypeInt16
	TypeInt32    Type = format.TypeInt32
	TypeInt64    Type = format.TypeInt64
	TypeUint8    Type = format.TypeUint8
	TypeUint16   Type = format.TypeUint16
	TypeUint32   Type = format.TypeUint32
	TypeUint64   Type = format.TypeUint64
	TypeFloat32  Type = format.TypeFloat32
	TypeFloat64  Type = format.TypeFloat64
	TypeString   Type = format.TypeString
	TypeBytes    Type = format.TypeBytes
	TypeDate     Type = format.TypeDate
	TypeTime     Type = format.TypeTime
	TypeDateTime Type = format.TypeDateTime
	TypeDecimal  Type = format.TypeDecimal

	TypeDateTimeTZ Type = format.TypeDateTimeTZ
)

// Date is a calendar date as days since the Unix epoch.
type Date = rowDate

// TimeOfDay is nanoseconds since midnight in [0, 86400e9).
type TimeOfDay = rowTimeOfDay

// rowDate and rowTimeOfDay are private aliases so the public names come from
// this package while the root package re-exports them.
type (
	rowDate      int32
	rowTimeOfDay int64
)

// Decimal is a decimal fixed-point value: an unscaled two's-complement
// integer plus a non-negative scale.
type Decimal struct {
	Unscaled *big.Int
	Scale    int32
}

// Value is an immutable, strong-typed cell value. Constructors copy inputs and
// getters return copies, so a Value never aliases caller-owned memory (decoded
// values may alias a caller-supplied Sink arena, per the decode contract).
// NULL carries no inherent type; the column's Schema type decides how it is
// written.
//
// Memory layout: 48 B with two pointer words (s, ref), down from the 104 B
// all-variants-inlined struct it replaces. The fields are a tagged union in
// practice — which members are meaningful is decided by typ:
//
//	num Int*/Uint* (raw bits), Date, Time, DateTime seconds, f64/f32 bit
//	    patterns, Bool (0/1)
//	aux DateTime: nanoseconds; DateTimeTZ: {tz uint32 << 32 | nanos};
//	    Decimal: scale
//	s   String; Bytes (owned copy, or a Sink-arena view on decode)
//	ref Decimal.Unscaled
type Value struct {
	typ  Type
	null bool
	num  uint64
	aux  uint64
	s    string
	ref  *big.Int
}

// packTZAux packs the DateTimeTZ aux word: zone offset in the high half,
// sub-second nanoseconds in the low half. tz is round-tripped through uint32
// so negative offsets survive the unsigned shift.
func packTZAux(tz int32, nanos uint32) uint64 {
	return uint64(uint32(tz))<<32 | uint64(nanos)
}

func tzFromAux(aux uint64) int32 { return int32(uint32(aux >> 32)) }

func nanosFromAux(aux uint64) uint32 { return uint32(aux) }

// ErrTypeMismatch is returned when a row value's type does not match its
// column's Schema type.
var ErrTypeMismatch = errors.New("rowpack: value type mismatch with schema column")

// ---- constructors ----

// Null returns a NULL value.
func Null() Value { return Value{null: true} }

// Bool returns a boolean value.
func Bool(v bool) Value {
	n := uint64(0)
	if v {
		n = 1
	}
	return Value{typ: TypeBool, num: n}
}

// Int8 returns an int8 value.
func Int8(v int8) Value { return Value{typ: TypeInt8, num: uint64(v)} }

// Int16 returns an int16 value.
func Int16(v int16) Value { return Value{typ: TypeInt16, num: uint64(v)} }

// Int32 returns an int32 value.
func Int32(v int32) Value { return Value{typ: TypeInt32, num: uint64(v)} }

// Int64 returns an int64 value.
func Int64(v int64) Value { return Value{typ: TypeInt64, num: uint64(v)} }

// Uint8 returns a uint8 value.
func Uint8(v uint8) Value { return Value{typ: TypeUint8, num: uint64(v)} }

// Uint16 returns a uint16 value.
func Uint16(v uint16) Value { return Value{typ: TypeUint16, num: uint64(v)} }

// Uint32 returns a uint32 value.
func Uint32(v uint32) Value { return Value{typ: TypeUint32, num: uint64(v)} }

// Uint64 returns a uint64 value.
func Uint64(v uint64) Value { return Value{typ: TypeUint64, num: v} }

// Float32 returns a float32 value preserving its exact bit pattern.
func Float32(v float32) Value { return Value{typ: TypeFloat32, num: uint64(math.Float32bits(v))} }

// Float64 returns a float64 value.
func Float64(v float64) Value { return Value{typ: TypeFloat64, num: math.Float64bits(v)} }

// String returns a string value.
func String(v string) Value { return Value{typ: TypeString, s: v} }

// Bytes returns a bytes value; v is copied. The copy is stored as an
// immutable string: same ownership and aliasing semantics, one pointer word
// less, and the accessor below still hands back a mutable copy.
func Bytes(v []byte) Value {
	if len(v) == 0 {
		return Value{typ: TypeBytes}
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return Value{typ: TypeBytes, s: unsafe.String(&cp[0], len(cp))}
}

// DateValue returns a date value.
func DateValue(v Date) Value { return Value{typ: TypeDate, num: uint64(v)} }

// TimeValue returns a time-of-day value.
func TimeValue(v TimeOfDay) Value { return Value{typ: TypeTime, num: uint64(v)} }

// DateTime converts t to UTC, strips the monotonic clock and location, and
// returns a DateTime value preserving nanosecond precision. Unlike UnixNano,
// the seconds/nanoseconds split keeps the full MySQL DATETIME range
// (year 1000..9999) and far beyond, with no int64-nanosecond overflow.
func DateTime(t time.Time) Value {
	t = t.UTC().Round(0)
	return Value{typ: TypeDateTime, num: uint64(t.Unix()), aux: uint64(t.Nanosecond())}
}

// DateTimeTZ returns a timezone-aware DateTime value preserving the instant
// and the original zone offset of t (seconds/nanoseconds split, no overflow).
func DateTimeTZ(t time.Time) Value {
	t = t.Round(0)
	_, off := t.Zone()
	return Value{typ: TypeDateTimeTZ, num: uint64(t.Unix()), aux: packTZAux(int32(off), uint32(t.Nanosecond()))}
}

// DateTimeTZValue returns the time value with its original zone offset and
// whether the accessor is valid.
func (v Value) DateTimeTZValue() (time.Time, bool) {
	if v.null || v.typ != TypeDateTimeTZ {
		return time.Time{}, false
	}
	return time.Unix(int64(v.num), int64(nanosFromAux(v.aux))).In(time.FixedZone("", int(tzFromAux(v.aux)))), true
}

// DecimalValue returns a decimal value; v.Unscaled is copied.
func DecimalValue(v Decimal) Value {
	u := big.NewInt(0)
	if v.Unscaled != nil {
		u.Set(v.Unscaled)
	}
	return Value{typ: TypeDecimal, aux: uint64(uint32(v.Scale)), ref: u}
}

// ---- accessors ----

// Type returns the value's type; it is 0 for NULL.
func (v Value) Type() Type { return v.typ }

// IsNull reports whether the value is NULL.
func (v Value) IsNull() bool { return v.null }

// Bool returns the boolean value and whether it is a non-NULL Bool.
func (v Value) Bool() (bool, bool) { return v.num != 0, !v.null && v.typ == TypeBool }

// Int8 returns the int8 value and whether the accessor is valid.
func (v Value) Int8() (int8, bool) {
	if v.null || v.typ != TypeInt8 {
		return 0, false
	}
	return int8(int64(v.num)), true
}

// Int16 returns the int16 value and whether the accessor is valid.
func (v Value) Int16() (int16, bool) {
	if v.null || v.typ != TypeInt16 {
		return 0, false
	}
	return int16(int64(v.num)), true
}

// Int32 returns the int32 value and whether the accessor is valid.
func (v Value) Int32() (int32, bool) {
	if v.null || v.typ != TypeInt32 {
		return 0, false
	}
	return int32(int64(v.num)), true
}

// Int64 returns the int64 value and whether the accessor is valid.
func (v Value) Int64() (int64, bool) {
	if v.null || v.typ != TypeInt64 {
		return 0, false
	}
	return int64(v.num), true
}

// Uint8 returns the uint8 value and whether the accessor is valid.
func (v Value) Uint8() (uint8, bool) {
	if v.null || v.typ != TypeUint8 {
		return 0, false
	}
	return uint8(v.num), true
}

// Uint16 returns the uint16 value and whether the accessor is valid.
func (v Value) Uint16() (uint16, bool) {
	if v.null || v.typ != TypeUint16 {
		return 0, false
	}
	return uint16(v.num), true
}

// Uint32 returns the uint32 value and whether the accessor is valid.
func (v Value) Uint32() (uint32, bool) {
	if v.null || v.typ != TypeUint32 {
		return 0, false
	}
	return uint32(v.num), true
}

// Uint64 returns the uint64 value and whether the accessor is valid.
func (v Value) Uint64() (uint64, bool) {
	if v.null || v.typ != TypeUint64 {
		return 0, false
	}
	return v.num, true
}

// Float32 returns the float32 value and whether the accessor is valid.
func (v Value) Float32() (float32, bool) {
	if v.null || v.typ != TypeFloat32 {
		return 0, false
	}
	return math.Float32frombits(uint32(v.num)), true
}

// Float64 returns the float64 value and whether the accessor is valid.
func (v Value) Float64() (float64, bool) {
	if v.null || v.typ != TypeFloat64 {
		return 0, false
	}
	return math.Float64frombits(v.num), true
}

// String returns the string value and whether the accessor is valid.
func (v Value) String() (string, bool) {
	if v.null || v.typ != TypeString {
		return "", false
	}
	return v.s, true
}

// Bytes returns a copy of the bytes value and whether the accessor is valid.
func (v Value) Bytes() ([]byte, bool) {
	if v.null || v.typ != TypeBytes {
		return nil, false
	}
	cp := make([]byte, len(v.s))
	copy(cp, v.s)
	return cp, true
}

// ---- fallback accessors ----
//
// The Or forms pair an accessor with a default, collapsing the two-value
// decode idiom into one call for wide-table decoding. NULL and type mismatch
// both yield def.

// BoolOr returns the boolean value, or def for NULL or a type mismatch.
func (v Value) BoolOr(def bool) bool {
	if b, ok := v.Bool(); ok {
		return b
	}
	return def
}

// Int64Or returns the int64 value, or def for NULL or a type mismatch.
func (v Value) Int64Or(def int64) int64 {
	if n, ok := v.Int64(); ok {
		return n
	}
	return def
}

// Uint64Or returns the uint64 value, or def for NULL or a type mismatch.
func (v Value) Uint64Or(def uint64) uint64 {
	if n, ok := v.Uint64(); ok {
		return n
	}
	return def
}

// Float32Or returns the float32 value, or def for NULL or a type mismatch.
func (v Value) Float32Or(def float32) float32 {
	if f, ok := v.Float32(); ok {
		return f
	}
	return def
}

// Float64Or returns the float64 value, or def for NULL or a type mismatch.
func (v Value) Float64Or(def float64) float64 {
	if f, ok := v.Float64(); ok {
		return f
	}
	return def
}

// StringOr returns the string value, or def for NULL or a type mismatch.
func (v Value) StringOr(def string) string {
	if s, ok := v.String(); ok {
		return s
	}
	return def
}

// BytesOr returns a copy of the bytes value, or def for NULL or a type
// mismatch.
func (v Value) BytesOr(def []byte) []byte {
	if b, ok := v.Bytes(); ok {
		return b
	}
	return def
}

// DateTimeOr returns the time value, or def for NULL or a type mismatch.
func (v Value) DateTimeOr(def time.Time) time.Time {
	if t, ok := v.DateTimeValue(); ok {
		return t
	}
	return def
}

// DateTimeTZOr returns the timezone-aware time value, or def for NULL or a
// type mismatch.
func (v Value) DateTimeTZOr(def time.Time) time.Time {
	if t, ok := v.DateTimeTZValue(); ok {
		return t
	}
	return def
}

// Date returns the date value and whether the accessor is valid.
func (v Value) Date() (Date, bool) {
	if v.null || v.typ != TypeDate {
		return 0, false
	}
	return Date(int64(v.num)), true
}

// Time returns the time-of-day value and whether the accessor is valid.
func (v Value) Time() (TimeOfDay, bool) {
	if v.null || v.typ != TypeTime {
		return 0, false
	}
	return TimeOfDay(int64(v.num)), true
}

// DateTimeValue returns the UTC time and whether the accessor is valid.
func (v Value) DateTimeValue() (time.Time, bool) {
	if v.null || v.typ != TypeDateTime {
		return time.Time{}, false
	}
	return time.Unix(int64(v.num), int64(nanosFromAux(v.aux))).UTC(), true
}

// Decimal returns the decimal value (unscaled copied) and whether the
// accessor is valid.
func (v Value) Decimal() (Decimal, bool) {
	if v.null || v.typ != TypeDecimal {
		return Decimal{}, false
	}
	u := big.NewInt(0)
	if v.ref != nil {
		u.Set(v.ref)
	}
	return Decimal{Unscaled: u, Scale: int32(v.aux)}, true
}
