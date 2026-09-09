package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
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

// bitmapScratch is a zeroed stack buffer used to zero-fill the null bitmap in
// EncodeInto without a per-row allocation. 2 KiB covers the 16384-column
// default limit; larger schemas fall back to make().
var bitmapScratch [2048]byte

// Encode serializes row against schema into a freshly allocated slice.
// The schema must already be validated (see Schema.Validate) before it is
// first used; Encode itself only enforces per-value and per-row limits.
func Encode(schema *Schema, row []Value, limits Limits) ([]byte, error) {
	return EncodeInto(schema, row, limits, nil)
}

// EncodeInto is Encode with a caller-provided scratch buffer that is reused
// across calls (the returned slice may alias reuse). The caller must not hold
// the returned slice across the next EncodeInto call unless it copies it.
// Like Encode it requires an already-validated schema; per-value
// type/null/scale/UTF-8/time/limit checks are still performed on every call.
func EncodeInto(schema *Schema, row []Value, limits Limits, reuse []byte) ([]byte, error) {
	if schema == nil {
		return nil, errors.New("rowpack: nil schema")
	}
	if len(schema.Columns) > int(limits.MaxColumns) {
		return nil, fmt.Errorf("rowpack: schema %q has %d columns, limit %d", schema.Name, len(schema.Columns), limits.MaxColumns)
	}
	if len(row) != len(schema.Columns) {
		return nil, fmt.Errorf("%w: row has %d values, schema has %d columns", ErrSchemaMismatch, len(row), len(schema.Columns))
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
	// Zero-fill the bitmap region from a stack array (bitmapBytes <= 2 KiB
	// even at the 16384-column limit); appending via a make-free slice avoids
	// a per-row allocation on the hot write path.
	if bitmapBytes <= len(bitmapScratch) {
		buf = append(buf, bitmapScratch[:bitmapBytes]...)
	} else {
		buf = append(buf, make([]byte, bitmapBytes)...)
	}

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

// Decode parses a TypedTuple payload against schema into a freshly allocated
// Row. It validates every length before allocation, requires exactly the
// expected bitmap size, rejects trailing bytes and unused bitmap bits, and
// enforces limits, so malformed payloads can never panic. The schema must
// already be validated (see Schema.Validate).
func Decode(data []byte, schema *Schema, limits Limits) ([]Value, error) {
	return DecodeInto(nil, data, schema, limits, nil)
}

// Sink lets decode paths that control payload-buffer lifetime (iterators)
// materialize String/Bytes payloads as zero-copy views instead of fresh
// copies. A nil *Sink, or a nil func field, keeps the copy semantics of
// Decode: the payload is copied into the decoded Value. Each func is called
// at most once per decoded value; returned views must stay valid for as long
// as the decoded Row is documented to live (see the iterator ownership
// contract).
type Sink struct {
	// String materializes a decoded String payload.
	String func(payload []byte) string
	// Bytes materializes a decoded Bytes payload.
	Bytes func(payload []byte) []byte
}

// DecodeInto is the single decode entry: it writes the decoded values into
// dst (growing it when the schema has more columns than dst can hold) and the
// returned Row aliases dst; the caller owns it and must not retain it across
// the next reuses of dst. Column values are copied with the same ownership
// semantics as Decode (String/Bytes payloads are copied or materialized
// through sink, Decimal reuses dst's existing *big.Int when the corresponding
// column already holds one). Like Decode it requires an already-validated
// schema and enforces all length/bounds/limit checks, so malformed payloads
// never panic.
func DecodeInto(dst []Value, data []byte, schema *Schema, limits Limits, sink *Sink) ([]Value, error) {
	if schema == nil {
		return nil, errors.New("rowpack: nil schema")
	}
	if len(schema.Columns) > int(limits.MaxColumns) {
		return nil, fmt.Errorf("rowpack: schema %q has %d columns, limit %d", schema.Name, len(schema.Columns), limits.MaxColumns)
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

	// Reuse the caller's slice capacity when it suffices; growth (if needed)
	// is a fresh allocation, matching Decode.
	row := dst
	if cap(row) < len(schema.Columns) {
		row = make([]Value, len(schema.Columns))
	}
	row = row[:len(schema.Columns)]
	for i, col := range schema.Columns {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			row[i] = Null()
			continue
		}
		v, n, err := readValueInto(row[i], data[pos:], col, limits, sink)
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

// readValueInto decodes one value of column type col from b into the reuse
// slot: for Decimal columns the
// existing *big.Int is kept and reused (colored by the caller decoding into
// the same row slice across rows), avoiding a big.Int allocation per row. For
// all other types the value is freshly built. When sink is non-nil, String
// payloads are materialized through it instead of being copied.
func readValueInto(reuse Value, b []byte, col Column, limits Limits, sink *Sink) (Value, int, error) {
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
			if sink != nil && sink.String != nil {
				return Value{typ: TypeString, s: sink.String(payload)}, 4 + ln, nil
			}
			return String(string(payload)), 4 + ln, nil
		}
		if sink != nil && sink.Bytes != nil {
			return Value{typ: TypeBytes, by: sink.Bytes(payload)}, 4 + ln, nil
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
		u := reuse.d.Unscaled
		if u == nil {
			u = new(big.Int)
		}
		if err := decodeDecimalBytesInto(u, raw[4:4+ln]); err != nil {
			return Value{}, 0, err
		}
		return Value{typ: TypeDecimal, d: Decimal{Unscaled: u, Scale: col.Scale}}, 4 + ln, nil
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
		buf, err := appendDecimalBytes(buf, v.d.Unscaled, limits.MaxValueBytes)
		if err != nil {
			return nil, err
		}
		if uint32(len(buf)) > limits.MaxRowBytes {
			return nil, fmt.Errorf("encoded row of %d bytes exceeds limit %d", len(buf), limits.MaxRowBytes)
		}
		return buf, nil
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
