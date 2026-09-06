package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// TypedTuple v1 row encoding:
//
//	u32 ColumnCount
//	u32 NullBitmapBytes   // ceil(ColumnCount/8)
//	bytes NullBitmap      // bit i = 1 => column i is NULL, LSB first
//	Value × non-null columns
//
// Unused high bits of the last bitmap byte must be zero; decoding must leave
// no trailing bytes. Value encodings are fixed by the v1 spec.

// ErrSchemaMismatch is returned when row and schema disagree.
var ErrSchemaMismatch = errors.New("rowpack: schema mismatch")

// Encode serializes row according to schema. It validates column count, value
// types, NULL vs nullable, limits, string UTF-8 and time/decimal ranges. The
// returned buffer is freshly allocated.
func Encode(schema *Schema, row []Value, limits Limits) ([]byte, error) {
	return EncodeInto(schema, row, limits, nil)
}

// EncodeInto is Encode with a caller-provided scratch buffer that is reused
// across calls (the returned slice may alias reuse). The caller must not hold
// the returned slice across the next EncodeInto call unless it copies it.
func EncodeInto(schema *Schema, row []Value, limits Limits, reuse []byte) ([]byte, error) {
	if schema == nil {
		return nil, errors.New("rowpack: nil schema")
	}
	if len(row) != len(schema.Columns) {
		return nil, fmt.Errorf("%w: row has %d values, schema has %d columns", ErrSchemaMismatch, len(row), len(schema.Columns))
	}
	if err := schema.Validate(limits); err != nil {
		return nil, err
	}

	bitmapBytes := (len(schema.Columns) + 7) / 8
	// Estimate: header + bitmap + worst-case 9 bytes per value (u64).
	need := 8 + bitmapBytes + 9*len(schema.Columns)
	var buf []byte
	if cap(reuse) >= need {
		buf = reuse[:0]
	} else {
		buf = make([]byte, 0, need)
	}
	buf = appendU32(buf, uint32(len(schema.Columns)))
	buf = appendU32(buf, uint32(bitmapBytes))
	bitmapOff := len(buf)
	buf = append(buf, make([]byte, bitmapBytes)...)

	for i, col := range schema.Columns {
		v := row[i]
		if v.IsNull() {
			if !col.Nullable {
				return nil, fmt.Errorf("%w: column %q is not nullable but row %d is NULL", ErrSchemaMismatch, col.Name, i)
			}
			buf[bitmapOff+i/8] |= 1 << uint(i%8)
			continue
		}
		if v.Type() != col.Type {
			return nil, fmt.Errorf("%w: column %q wants type %d but value is %d", ErrSchemaMismatch, col.Name, col.Type, v.Type())
		}
		if col.Type == TypeDecimal {
			if v.d.Scale != col.Scale {
				return nil, fmt.Errorf("%w: column %q has scale %d but value scale %d", ErrSchemaMismatch, col.Name, col.Scale, v.d.Scale)
			}
		}
		var err error
		buf, err = appendValue(buf, v, limits)
		if err != nil {
			return nil, fmt.Errorf("rowpack: encode column %d (%q): %w", i, col.Name, err)
		}
		if uint32(len(buf)) > limits.MaxRowBytes {
			return nil, fmt.Errorf("rowpack: encoded row of %d bytes exceeds limit %d", len(buf), limits.MaxRowBytes)
		}
	}
	return buf, nil
}

// Decode parses a TypedTuple payload against schema. It validates every
// length before allocation, requires exactly the expected bitmap size, rejects
// trailing bytes and unused bitmap bits, and enforces limits.
func Decode(data []byte, schema *Schema, limits Limits) ([]Value, error) {
	if schema == nil {
		return nil, errors.New("rowpack: nil schema")
	}
	if err := schema.Validate(limits); err != nil {
		return nil, err
	}
	pos := 0
	readU32 := func() (uint32, bool) {
		if len(data)-pos < 4 {
			return 0, false
		}
		v := binary.LittleEndian.Uint32(data[pos:])
		pos += 4
		return v, true
	}
	colCount, ok := readU32()
	if !ok {
		return nil, fmt.Errorf("rowpack: truncated tuple header")
	}
	if int(colCount) != len(schema.Columns) {
		return nil, fmt.Errorf("%w: tuple has %d columns, schema has %d", ErrSchemaMismatch, colCount, len(schema.Columns))
	}
	expectBitmap := (len(schema.Columns) + 7) / 8
	bitmapBytes, ok := readU32()
	if !ok || int(bitmapBytes) != expectBitmap {
		return nil, fmt.Errorf("rowpack: bitmap bytes = %d, want %d", bitmapBytes, expectBitmap)
	}
	if len(data)-pos < int(bitmapBytes) {
		return nil, fmt.Errorf("rowpack: truncated null bitmap")
	}
	bitmap := data[pos : pos+int(bitmapBytes)]
	pos += int(bitmapBytes)
	// Unused high bits of the last byte must be zero.
	if bits := len(schema.Columns) % 8; bits != 0 {
		if last := bitmap[len(bitmap)-1]; last>>uint(bits) != 0 {
			return nil, fmt.Errorf("rowpack: non-zero unused null bitmap bits")
		}
	}

	row := make([]Value, len(schema.Columns))
	for i, col := range schema.Columns {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			row[i] = Null()
			continue
		}
		v, n, err := readValue(data[pos:], col, limits)
		if err != nil {
			return nil, fmt.Errorf("rowpack: decode column %d (%q): %w", i, col.Name, err)
		}
		pos += n
		row[i] = v
	}
	if pos != len(data) {
		return nil, fmt.Errorf("rowpack: %d trailing bytes after tuple", len(data)-pos)
	}
	return row, nil
}

// readValue decodes one value of column type col from b, returning the value
// and the number of bytes consumed.
func readValue(b []byte, col Column, limits Limits) (Value, int, error) {
	need := func(n int) ([]byte, bool) {
		if len(b) < n {
			return nil, false
		}
		return b[:n], true
	}
	switch col.Type {
	case TypeBool:
		raw, ok := need(1)
		if !ok {
			return Value{}, 0, errors.New("truncated bool")
		}
		switch raw[0] {
		case 0:
			return Bool(false), 1, nil
		case 1:
			return Bool(true), 1, nil
		}
		return Value{}, 0, fmt.Errorf("invalid bool byte 0x%02x", raw[0])
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64:
		n := fixedWidth(col.Type)
		raw, ok := need(n)
		if !ok {
			return Value{}, 0, fmt.Errorf("truncated %d", col.Type)
		}
		switch col.Type {
		case TypeInt8:
			return Int8(int8(raw[0])), n, nil
		case TypeInt16:
			return Int16(int16(binary.LittleEndian.Uint16(raw))), n, nil
		case TypeInt32:
			return Int32(int32(binary.LittleEndian.Uint32(raw))), n, nil
		}
		return Int64(int64(binary.LittleEndian.Uint64(raw))), n, nil
	case TypeUint8, TypeUint16, TypeUint32, TypeUint64:
		n := fixedWidth(col.Type)
		raw, ok := need(n)
		if !ok {
			return Value{}, 0, fmt.Errorf("truncated %d", col.Type)
		}
		switch col.Type {
		case TypeUint8:
			return Uint8(raw[0]), n, nil
		case TypeUint16:
			return Uint16(binary.LittleEndian.Uint16(raw)), n, nil
		case TypeUint32:
			return Uint32(binary.LittleEndian.Uint32(raw)), n, nil
		}
		return Uint64(binary.LittleEndian.Uint64(raw)), n, nil
	case TypeFloat32:
		raw, ok := need(4)
		if !ok {
			return Value{}, 0, errors.New("truncated float32")
		}
		return Float32(math.Float32frombits(binary.LittleEndian.Uint32(raw))), 4, nil
	case TypeFloat64:
		raw, ok := need(8)
		if !ok {
			return Value{}, 0, errors.New("truncated float64")
		}
		return Float64(math.Float64frombits(binary.LittleEndian.Uint64(raw))), 8, nil
	case TypeString, TypeBytes:
		ln, ok := getVarLen(b)
		if !ok {
			return Value{}, 0, fmt.Errorf("truncated type %d length", col.Type)
		}
		if uint32(ln) > limits.MaxValueBytes {
			return Value{}, 0, fmt.Errorf("type %d value of %d bytes exceeds limit %d", col.Type, ln, limits.MaxValueBytes)
		}
		raw, ok := need(4 + ln)
		if !ok {
			return Value{}, 0, fmt.Errorf("truncated type %d payload", col.Type)
		}
		payload := raw[4 : 4+ln]
		if col.Type == TypeString && !utf8.Valid(payload) {
			return Value{}, 0, errors.New("string is not valid UTF-8")
		}
		if col.Type == TypeString {
			return String(string(payload)), 4 + ln, nil
		}
		return Bytes(payload), 4 + ln, nil
	case TypeDate:
		raw, ok := need(4)
		if !ok {
			return Value{}, 0, errors.New("truncated date")
		}
		return DateValue(Date(int32(binary.LittleEndian.Uint32(raw)))), 4, nil
	case TypeTime:
		raw, ok := need(8)
		if !ok {
			return Value{}, 0, errors.New("truncated time")
		}
		ns := int64(binary.LittleEndian.Uint64(raw))
		if ns < 0 || ns >= MaxTimeOfDay {
			return Value{}, 0, fmt.Errorf("time of day %d out of range", ns)
		}
		return TimeValue(TimeOfDay(ns)), 8, nil
	case TypeDateTime:
		raw, ok := need(8)
		if !ok {
			return Value{}, 0, errors.New("truncated datetime")
		}
		return Value{typ: TypeDateTime, i: int64(binary.LittleEndian.Uint64(raw))}, 8, nil
	case TypeDecimal:
		ln, ok := getVarLen(b)
		if !ok {
			return Value{}, 0, errors.New("truncated decimal length")
		}
		if uint32(ln) > limits.MaxValueBytes {
			return Value{}, 0, fmt.Errorf("decimal unscaled of %d bytes exceeds limit %d", ln, limits.MaxValueBytes)
		}
		raw, ok := need(4 + ln)
		if !ok {
			return Value{}, 0, errors.New("truncated decimal payload")
		}
		u, err := decodeDecimalBytes(raw[4 : 4+ln])
		if err != nil {
			return Value{}, 0, err
		}
		return DecimalValue(Decimal{Unscaled: u, Scale: col.Scale}), 4 + ln, nil
	}
	return Value{}, 0, fmt.Errorf("unsupported type %d", col.Type)
}

// appendValue appends the encoding of v, returning the extended buffer.
func appendValue(buf []byte, v Value, limits Limits) ([]byte, error) {
	switch v.Type() {
	case TypeBool:
		if v.b {
			return append(buf, 1), nil
		}
		return append(buf, 0), nil
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64:
		return appendFixed(buf, uint64(v.i), fixedWidth(v.Type())), nil
	case TypeUint8, TypeUint16, TypeUint32, TypeUint64:
		return appendFixed(buf, v.u, fixedWidth(v.Type())), nil
	case TypeFloat32:
		return appendU32(buf, math.Float32bits(v.f32)), nil
	case TypeFloat64:
		return appendU64(buf, math.Float64bits(v.f64)), nil
	case TypeString:
		if err := CheckString(v.s, limits); err != nil {
			return nil, err
		}
		buf = appendU32(buf, uint32(len(v.s)))
		return append(buf, v.s...), nil
	case TypeBytes:
		if uint32(len(v.by)) > limits.MaxValueBytes {
			return nil, fmt.Errorf("bytes value of %d bytes exceeds limit %d", len(v.by), limits.MaxValueBytes)
		}
		buf = appendU32(buf, uint32(len(v.by)))
		return append(buf, v.by...), nil
	case TypeDate:
		return appendU32(buf, uint32(int32(v.i))), nil
	case TypeTime:
		if v.i < 0 || v.i >= MaxTimeOfDay {
			return nil, fmt.Errorf("time of day %d out of range", v.i)
		}
		return appendU64(buf, uint64(v.i)), nil
	case TypeDateTime:
		return appendU64(buf, uint64(v.i)), nil
	case TypeDecimal:
		if v.d.Scale < 0 {
			return nil, fmt.Errorf("decimal scale %d is negative", v.d.Scale)
		}
		raw, err := encodeDecimalBytes(v.d.Unscaled)
		if err != nil {
			return nil, err
		}
		if uint32(len(raw)) > limits.MaxValueBytes {
			return nil, fmt.Errorf("decimal unscaled of %d bytes exceeds limit %d", len(raw), limits.MaxValueBytes)
		}
		buf = appendU32(buf, uint32(len(raw)))
		return append(buf, raw...), nil
	}
	return nil, fmt.Errorf("unsupported type %d", v.Type())
}

func fixedWidth(t Type) int {
	switch t {
	case TypeInt8, TypeUint8:
		return 1
	case TypeInt16, TypeUint16:
		return 2
	case TypeInt32, TypeUint32, TypeFloat32, TypeDate:
		return 4
	case TypeInt64, TypeUint64, TypeFloat64, TypeTime, TypeDateTime:
		return 8
	}
	return 0
}

// getVarLen reads a u32 length prefix, enforcing the value byte limit.
func getVarLen(b []byte) (int, bool) {
	if len(b) < 4 {
		return 0, false
	}
	return int(binary.LittleEndian.Uint32(b)), true
}

func appendFixed(buf []byte, v uint64, n int) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(buf, tmp[:n]...)
}

func appendU32(buf []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(buf, tmp[:]...)
}

func appendU64(buf []byte, v uint64) []byte {
	var tmp [8]byte
	binary.LittleEndian.PutUint64(tmp[:], v)
	return append(buf, tmp[:]...)
}
