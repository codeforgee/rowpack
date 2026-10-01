package rowpack

import (
	"time"

	"github.com/codeforgee/rowpack/internal/codec"
)

// Value is an immutable, strong-typed cell value. Constructors copy inputs and
// getters return copies, so a Value never aliases caller-owned memory. NULL is
// expressed by Null() and carries no inherent type; the column's Schema type
// decides how it is written.
type Value = codec.Value

// Row is an ordered set of column values.
type Row []Value

// Null returns a NULL value.
func Null() Value { return codec.Null() }

// Bool returns a boolean value.
func Bool(v bool) Value { return codec.Bool(v) }

// Int8 returns an int8 value.
func Int8(v int8) Value { return codec.Int8(v) }

// Int16 returns an int16 value.
func Int16(v int16) Value { return codec.Int16(v) }

// Int32 returns an int32 value.
func Int32(v int32) Value { return codec.Int32(v) }

// Int64 returns an int64 value.
func Int64(v int64) Value { return codec.Int64(v) }

// Uint8 returns a uint8 value.
func Uint8(v uint8) Value { return codec.Uint8(v) }

// Uint16 returns a uint16 value.
func Uint16(v uint16) Value { return codec.Uint16(v) }

// Uint32 returns a uint32 value.
func Uint32(v uint32) Value { return codec.Uint32(v) }

// Uint64 returns a uint64 value.
func Uint64(v uint64) Value { return codec.Uint64(v) }

// Float32 returns a float32 value preserving its exact bit pattern.
func Float32(v float32) Value { return codec.Float32(v) }

// Float64 returns a float64 value.
func Float64(v float64) Value { return codec.Float64(v) }

// String returns a string value.
func String(v string) Value { return codec.String(v) }

// Bytes returns a bytes value; v is copied.
func Bytes(v []byte) Value { return codec.Bytes(v) }

// DateValue returns a date value.
func DateValue(v Date) Value { return codec.DateValue(v) }

// TimeValue returns a time-of-day value.
func TimeValue(v TimeOfDay) Value { return codec.TimeValue(v) }

// DateTime converts t to UTC, strips monotonic clock and location, and returns
// a DateTime value preserving nanosecond precision.
func DateTime(t time.Time) Value { return codec.DateTime(t) }

// DateTimeTZ returns a timezone-aware DateTime value preserving the instant
// and the original zone offset of t.
func DateTimeTZ(t time.Time) Value { return codec.DateTimeTZ(t) }

// DecimalValue returns a decimal value; v.Unscaled is copied.
func DecimalValue(v Decimal) Value { return codec.DecimalValue(v) }

// NewDate converts t to a calendar date (days since the Unix epoch) in the
// proleptic Gregorian calendar.
func NewDate(t time.Time) Date { return codec.NewDate(t) }

// NewTimeOfDay builds a TimeOfDay from hour/minute/second/nanosecond,
// validating each field's range.
func NewTimeOfDay(hour, min, sec, nsec int) (TimeOfDay, error) {
	return codec.NewTimeOfDay(hour, min, sec, nsec)
}
