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
	"math/big"
	"time"

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
// getters return copies, so a Value never aliases caller-owned memory. NULL
// carries no inherent type; the column's Schema type decides how it is written.
type Value struct {
	typ  Type
	null bool

	b   bool
	i   int64  // Int*, Date, Time, DateTime
	u   uint64 // Uint*
	f64 float64
	f32 float32
	s   string
	by  []byte // immutable copy
	d   Decimal
}

// ErrTypeMismatch is returned when a row value's type does not match its
// column's Schema type.
var ErrTypeMismatch = errors.New("rowpack: value type mismatch with schema column")

// ---- constructors ----

// Null returns a NULL value.
func Null() Value { return Value{null: true} }

// Bool returns a boolean value.
func Bool(v bool) Value { return Value{typ: TypeBool, b: v} }

// Int8 returns an int8 value.
func Int8(v int8) Value { return Value{typ: TypeInt8, i: int64(v)} }

// Int16 returns an int16 value.
func Int16(v int16) Value { return Value{typ: TypeInt16, i: int64(v)} }

// Int32 returns an int32 value.
func Int32(v int32) Value { return Value{typ: TypeInt32, i: int64(v)} }

// Int64 returns an int64 value.
func Int64(v int64) Value { return Value{typ: TypeInt64, i: v} }

// Uint8 returns a uint8 value.
func Uint8(v uint8) Value { return Value{typ: TypeUint8, u: uint64(v)} }

// Uint16 returns a uint16 value.
func Uint16(v uint16) Value { return Value{typ: TypeUint16, u: uint64(v)} }

// Uint32 returns a uint32 value.
func Uint32(v uint32) Value { return Value{typ: TypeUint32, u: uint64(v)} }

// Uint64 returns a uint64 value.
func Uint64(v uint64) Value { return Value{typ: TypeUint64, u: v} }

// Float32 returns a float32 value preserving its exact bit pattern.
func Float32(v float32) Value { return Value{typ: TypeFloat32, f32: v} }

// Float64 returns a float64 value.
func Float64(v float64) Value { return Value{typ: TypeFloat64, f64: v} }

// String returns a string value.
func String(v string) Value { return Value{typ: TypeString, s: v} }

// Bytes returns a bytes value; v is copied.
func Bytes(v []byte) Value {
	cp := make([]byte, len(v))
	copy(cp, v)
	return Value{typ: TypeBytes, by: cp}
}

// DateValue returns a date value.
func DateValue(v Date) Value { return Value{typ: TypeDate, i: int64(v)} }

// TimeValue returns a time-of-day value.
func TimeValue(v TimeOfDay) Value { return Value{typ: TypeTime, i: int64(v)} }

// DateTime converts t to UTC, strips the monotonic clock and location, and
// returns a DateTime value preserving nanosecond precision.
func DateTime(t time.Time) Value {
	t = t.UTC().Round(0)
	return Value{typ: TypeDateTime, i: t.UnixNano()}
}

// DecimalValue returns a decimal value; v.Unscaled is copied.
func DecimalValue(v Decimal) Value {
	u := big.NewInt(0)
	if v.Unscaled != nil {
		u.Set(v.Unscaled)
	}
	return Value{typ: TypeDecimal, d: Decimal{Unscaled: u, Scale: v.Scale}}
}

// ---- accessors ----

// Type returns the value's type; it is 0 for NULL.
func (v Value) Type() Type { return v.typ }

// IsNull reports whether the value is NULL.
func (v Value) IsNull() bool { return v.null }

// Bool returns the boolean value and whether it is a non-NULL Bool.
func (v Value) Bool() (bool, bool) { return v.b, !v.null && v.typ == TypeBool }

// Int8 returns the int8 value and whether the accessor is valid.
func (v Value) Int8() (int8, bool) {
	if v.null || v.typ != TypeInt8 {
		return 0, false
	}
	return int8(v.i), true
}

// Int16 returns the int16 value and whether the accessor is valid.
func (v Value) Int16() (int16, bool) {
	if v.null || v.typ != TypeInt16 {
		return 0, false
	}
	return int16(v.i), true
}

// Int32 returns the int32 value and whether the accessor is valid.
func (v Value) Int32() (int32, bool) {
	if v.null || v.typ != TypeInt32 {
		return 0, false
	}
	return int32(v.i), true
}

// Int64 returns the int64 value and whether the accessor is valid.
func (v Value) Int64() (int64, bool) {
	if v.null || v.typ != TypeInt64 {
		return 0, false
	}
	return v.i, true
}

// Uint8 returns the uint8 value and whether the accessor is valid.
func (v Value) Uint8() (uint8, bool) {
	if v.null || v.typ != TypeUint8 {
		return 0, false
	}
	return uint8(v.u), true
}

// Uint16 returns the uint16 value and whether the accessor is valid.
func (v Value) Uint16() (uint16, bool) {
	if v.null || v.typ != TypeUint16 {
		return 0, false
	}
	return uint16(v.u), true
}

// Uint32 returns the uint32 value and whether the accessor is valid.
func (v Value) Uint32() (uint32, bool) {
	if v.null || v.typ != TypeUint32 {
		return 0, false
	}
	return uint32(v.u), true
}

// Uint64 returns the uint64 value and whether the accessor is valid.
func (v Value) Uint64() (uint64, bool) {
	if v.null || v.typ != TypeUint64 {
		return 0, false
	}
	return v.u, true
}

// Float32 returns the float32 value and whether the accessor is valid.
func (v Value) Float32() (float32, bool) {
	if v.null || v.typ != TypeFloat32 {
		return 0, false
	}
	return v.f32, true
}

// Float64 returns the float64 value and whether the accessor is valid.
func (v Value) Float64() (float64, bool) {
	if v.null || v.typ != TypeFloat64 {
		return 0, false
	}
	return v.f64, true
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
	cp := make([]byte, len(v.by))
	copy(cp, v.by)
	return cp, true
}

// Date returns the date value and whether the accessor is valid.
func (v Value) Date() (Date, bool) {
	if v.null || v.typ != TypeDate {
		return 0, false
	}
	return Date(v.i), true
}

// Time returns the time-of-day value and whether the accessor is valid.
func (v Value) Time() (TimeOfDay, bool) {
	if v.null || v.typ != TypeTime {
		return 0, false
	}
	return TimeOfDay(v.i), true
}

// DateTimeValue returns the UTC time and whether the accessor is valid.
func (v Value) DateTimeValue() (time.Time, bool) {
	if v.null || v.typ != TypeDateTime {
		return time.Time{}, false
	}
	return time.Unix(0, v.i).UTC(), true
}

// Decimal returns the decimal value (unscaled copied) and whether the
// accessor is valid.
func (v Value) Decimal() (Decimal, bool) {
	if v.null || v.typ != TypeDecimal {
		return Decimal{}, false
	}
	u := big.NewInt(0)
	if v.d.Unscaled != nil {
		u.Set(v.d.Unscaled)
	}
	return Decimal{Unscaled: u, Scale: v.d.Scale}, true
}
