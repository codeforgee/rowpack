package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/big"
	"unicode/utf8"

	"github.com/codeforgee/rowpack/internal/format"
)

// TypedTuple v1 row encoding:
//
//	bytes NullBitmap   // ceil(ColumnCount/8), bit i = 1 => column i is NULL, LSB first
//	Value × non-null columns
//
// The column count and the bitmap width are implied by the Schema, not written.
// Unused high bits of the last bitmap byte must be zero; decoding must leave
// no trailing bytes.
//
// Fixed-width values (integers, floats, Date, Time, DateTime, DateTimeTZ) are
// little-endian with no padding. Variable-width values (String, Bytes, Decimal)
// are written as [uvarint length][bytes]: the prefix width tracks the value, so
// a short string costs 1 prefix byte instead of 4.

// ErrSchemaMismatch is returned when row and schema disagree.
var ErrSchemaMismatch = errors.New("rowpack: schema mismatch")

// bitmapScratch is a zeroed stack buffer used to zero-fill the null bitmap in
// EncodeInto without a per-row allocation. 2 KiB covers the 16384-column
// default limit; larger schemas fall back to make().
var bitmapScratch [2048]byte

// Codec is the row codec of one store: the encode/decode policy (Limits)
// exposed as methods. Putting Limits on the codec keeps it off every encode
// and decode call site and gives the write path (Store.rowCodec) and the read
// paths the same policy object.
//
// Sink stays a per-call decode argument: it shapes how String/Bytes payloads
// are materialized (zero-copy arena view vs fresh copy) and differs per call
// site; a nil *Sink keeps copy semantics.
type Codec struct {
	Limits Limits
}

// Decoder is a schema-bound body decoder. It hoists schema validation and
// bitmap geometry out of repeated DecodeInto calls, which is useful for page
// scans and batches where many adjacent records share one schema version.
// Decoder is immutable and safe for concurrent use; dst and Sink retain the
// same ownership rules as Codec.DecodeInto.
type Decoder struct {
	codec          Codec
	schema         *Schema
	bitmapBytes    int
	steps          []decodeStep
	tailBitmapMask byte
	fixed          bool
	fixedBytes     int
}

// Columns returns the compiled row width.
func (d Decoder) Columns() int { return len(d.schema.Columns) }

// CompileDecoder binds a validated schema to the codec. The returned decoder
// avoids repeating invariant checks for every row in a homogeneous batch.
func (c Codec) CompileDecoder(schema *Schema) (Decoder, error) {
	if schema == nil {
		return Decoder{}, errors.New("rowpack: nil schema")
	}
	if len(schema.Columns) > int(c.Limits.MaxColumns) {
		return Decoder{}, fmt.Errorf("rowpack: schema %q has %d columns, limit %d", schema.Name, len(schema.Columns), c.Limits.MaxColumns)
	}
	steps, fixed := compileDecodeSteps(schema)
	// A schema with no nullable column writes no bitmap, so there is no last
	// byte whose unused high bits need the corruption check either.
	nb := schema.nullBitmapBytes()
	var mask byte
	if nb > 0 {
		mask = tailBitmapMask(len(schema.Columns))
	}
	d := Decoder{
		codec:          c,
		schema:         schema,
		bitmapBytes:    nb,
		steps:          steps,
		tailBitmapMask: mask,
		fixed:          fixed,
	}
	for _, st := range steps {
		d.fixedBytes += st.width
	}
	return d, nil
}

// DecodeInto decodes one body using the schema invariants bound by
// CompileDecoder.
func (d Decoder) DecodeInto(dst []Value, body []byte, sink *Sink) ([]Value, error) {
	// Fully-fixed schemas are common enough to keep the specialised kernel:
	// with no NULLs and no length prefixes it needs neither a bitmap test nor
	// a position that can move dynamically.
	if d.fixed && len(body) == d.bitmapBytes+d.fixedBytes {
		if bitmapIsZero(body[:d.bitmapBytes]) {
			if row, ok := d.decodeFixedInto(dst, body[d.bitmapBytes:]); ok {
				return row, nil
			}
		}
	} else if row, ok := d.fastDecode(dst, body, sink); ok {
		return row, nil
	}
	return d.codec.decodeBodyIntoPrepared(dst, body, d.schema, d.bitmapBytes, sink)
}

// DecodeBatchInto decodes homogeneous body-only tuples into one contiguous
// Value slab. The returned slice contains len(bodies)*Columns values in row
// order; row i is result[i*ncols:(i+1)*ncols]. dst is appended to, allowing
// callers to create row views without one allocation per row.
func (d Decoder) DecodeBatchInto(dst []Value, bodies [][]byte, sink *Sink) ([]Value, error) {
	ncols := len(d.schema.Columns)
	maxInt := int(^uint(0) >> 1)
	if ncols != 0 && len(bodies) > (maxInt-len(dst))/ncols {
		return nil, fmt.Errorf("rowpack: decode batch size overflows int")
	}
	need := len(bodies) * ncols
	if need > cap(dst)-len(dst) {
		grown := make([]Value, len(dst), len(dst)+need)
		copy(grown, dst)
		dst = grown
	}
	for batch, body := range bodies {
		start := len(dst)
		dst = dst[:start+ncols]
		var (
			row []Value
			err error
		)
		if d.fixed && len(body) == d.bitmapBytes+d.fixedBytes && bitmapIsZero(body[:d.bitmapBytes]) {
			var ok bool
			row, ok = d.decodeFixedInto(dst[start:start+ncols], body[d.bitmapBytes:])
			if !ok {
				row, err = d.codec.decodeBodyIntoPrepared(dst[start:start+ncols], body, d.schema, d.bitmapBytes, sink)
			}
		} else if r, ok := d.fastDecode(dst[start:start+ncols], body, sink); ok {
			row = r
		} else {
			row, err = d.codec.decodeBodyIntoPrepared(dst[start:start+ncols], body, d.schema, d.bitmapBytes, sink)
		}
		if err != nil {
			return nil, fmt.Errorf("rowpack: decode batch row %d: %w", batch, err)
		}
		if len(row) != ncols {
			return nil, fmt.Errorf("rowpack: decode batch row %d produced %d columns, want %d", batch, len(row), ncols)
		}
	}
	return dst, nil
}

// bitmapIsZero reports whether every bit of bitmap is clear (no NULL).
func bitmapIsZero(bitmap []byte) bool {
	for _, b := range bitmap {
		if b != 0 {
			return false
		}
	}
	return true
}

// tailBitmapMask returns the mask of the unused high bits of the last null
// bitmap byte (0 when every bit is used). Rejecting those bits matches the
// generic decoder, which treats them as corruption.
func tailBitmapMask(ncols int) byte {
	if bits := ncols % 8; bits != 0 {
		return byte(0xFF << uint(bits))
	}
	return 0
}

// decodeFixedInto is the valid-data fast path for non-nullable, fixed-width
// schemas. Geometry has already been checked by DecodeInto, so each column
// needs no individual bounds check. ok=false requests the fully validating
// generic path for invalid bool/time values, preserving its error messages.
func (d Decoder) decodeFixedInto(dst []Value, payload []byte) ([]Value, bool) {
	row := dst
	if cap(row) < len(d.schema.Columns) {
		row = make([]Value, len(d.schema.Columns))
	}
	row = row[:len(d.schema.Columns)]
	pos := 0
	for i, col := range d.schema.Columns {
		switch col.Type {
		case TypeBool:
			x := payload[pos]
			if x > 1 {
				return nil, false
			}
			row[i] = Value{typ: TypeBool, b: x == 1}
			pos++
		case TypeInt8:
			row[i] = Value{typ: TypeInt8, i: int64(int8(payload[pos]))}
			pos++
		case TypeInt16:
			row[i] = Value{typ: TypeInt16, i: int64(int16(binary.LittleEndian.Uint16(payload[pos:])))}
			pos += 2
		case TypeInt32:
			row[i] = Value{typ: TypeInt32, i: int64(int32(binary.LittleEndian.Uint32(payload[pos:])))}
			pos += 4
		case TypeInt64:
			row[i] = Value{typ: TypeInt64, i: int64(binary.LittleEndian.Uint64(payload[pos:]))}
			pos += 8
		case TypeUint8:
			row[i] = Value{typ: TypeUint8, u: uint64(payload[pos])}
			pos++
		case TypeUint16:
			row[i] = Value{typ: TypeUint16, u: uint64(binary.LittleEndian.Uint16(payload[pos:]))}
			pos += 2
		case TypeUint32:
			row[i] = Value{typ: TypeUint32, u: uint64(binary.LittleEndian.Uint32(payload[pos:]))}
			pos += 4
		case TypeUint64:
			row[i] = Value{typ: TypeUint64, u: binary.LittleEndian.Uint64(payload[pos:])}
			pos += 8
		case TypeFloat32:
			row[i] = Value{typ: TypeFloat32, f32: math.Float32frombits(binary.LittleEndian.Uint32(payload[pos:]))}
			pos += 4
		case TypeFloat64:
			row[i] = Value{typ: TypeFloat64, f64: math.Float64frombits(binary.LittleEndian.Uint64(payload[pos:]))}
			pos += 8
		case TypeDate:
			row[i] = Value{typ: TypeDate, i: int64(int32(binary.LittleEndian.Uint32(payload[pos:])))}
			pos += 4
		case TypeTime:
			x := int64(binary.LittleEndian.Uint64(payload[pos:]))
			if x < 0 || x >= MaxTimeOfDay {
				return nil, false
			}
			row[i] = Value{typ: TypeTime, i: x}
			pos += 8
		case TypeDateTime:
			row[i] = Value{
				typ: TypeDateTime,
				i:   int64(binary.LittleEndian.Uint64(payload[pos:])),
				u:   uint64(binary.LittleEndian.Uint32(payload[pos+8:])),
			}
			pos += 12
		case TypeDateTimeTZ:
			row[i] = Value{
				typ: TypeDateTimeTZ,
				i:   int64(binary.LittleEndian.Uint64(payload[pos:])),
				u:   uint64(binary.LittleEndian.Uint32(payload[pos+8:])),
				tz:  int32(binary.LittleEndian.Uint32(payload[pos+12:])),
			}
			pos += 16
		}
	}
	return row, true
}

// EncodeInto encodes the body of a TypedTuple — null bitmap + values,
// without the 8-byte ColumnCount/NullBitmapBytes header — for page layouts
// that carry the schema out of band (Rows Page). The returned slice may
// alias reuse. Type, nullability, size and decimal-scale constraints are
// checked for every value.
func (c Codec) EncodeInto(schema *Schema, row []Value, reuse []byte) ([]byte, error) {
	if schema == nil {
		return nil, errors.New("rowpack: nil schema")
	}
	if len(schema.Columns) > int(c.Limits.MaxColumns) {
		return nil, fmt.Errorf("rowpack: schema %q has %d columns, limit %d", schema.Name, len(schema.Columns), c.Limits.MaxColumns)
	}
	if len(row) != len(schema.Columns) {
		return nil, fmt.Errorf("%w: row has %d values, schema has %d columns", ErrSchemaMismatch, len(row), len(schema.Columns))
	}
	bitmapBytes := schema.nullBitmapBytes()
	need := bitmapBytes + 9*len(schema.Columns)
	var buf []byte
	if cap(reuse) >= need {
		buf = reuse[:0]
	} else {
		buf = make([]byte, 0, need)
	}
	return c.encodeBodyInto(buf, schema, row)
}

// encodeBodyInto appends the null bitmap and the per-column values to buf.
// The bitmap region is zero-filled from a stack array (bitmapBytes <= 2 KiB
// even at the 16384-column limit) so the hot write path stays allocation-free.
func (c Codec) encodeBodyInto(buf []byte, schema *Schema, row []Value) ([]byte, error) {
	bitmapBytes := schema.nullBitmapBytes()
	if bitmapBytes > 0 {
		if bitmapBytes <= len(bitmapScratch) {
			buf = append(buf, bitmapScratch[:bitmapBytes]...)
		} else {
			buf = append(buf, make([]byte, bitmapBytes)...)
		}
	}
	bitmapOff := len(buf) - bitmapBytes

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
		buf, err = appendValue(buf, v, c.Limits)
		if err != nil {
			return nil, fmt.Errorf("rowpack: encode column %d (%q): %w", i, col.Name, err)
		}
		if uint32(len(buf)) > c.Limits.MaxRowBytes {
			return nil, fmt.Errorf("rowpack: encoded row of %d bytes exceeds limit %d", len(buf), c.Limits.MaxRowBytes)
		}
	}
	return buf, nil
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

// PageRecord is one decoded Rows Page record (page layout): identity and
// metadata from the page streams plus a view of the body-only TypedTuple.
// Body aliases the page buffer and is empty for deletes; callers decode it
// against the record's schema version via Decoder.DecodeInto and must not
// retain it beyond the page's lifetime.
type PageRecord struct {
	RowID         uint64
	SchemaVersion uint32
	ChangeType    format.ChangeType
	Body          []byte
}

// decodeBodyIntoPrepared is the validating general-purpose compiled decoder.
func (c Codec) decodeBodyIntoPrepared(dst []Value, body []byte, schema *Schema, expectBitmap int, sink *Sink) ([]Value, error) {
	if len(body) < expectBitmap {
		return nil, fmt.Errorf("rowpack: truncated null bitmap")
	}
	bitmap := body[:expectBitmap]
	pos := expectBitmap
	// Unused high bits of the last byte must be zero. A schema without a
	// nullable column carries no bitmap, so there is nothing to check.
	if expectBitmap > 0 {
		if bits := len(schema.Columns) % 8; bits != 0 {
			if last := bitmap[expectBitmap-1]; last>>uint(bits) != 0 {
				return nil, fmt.Errorf("rowpack: non-zero unused null bitmap bits")
			}
		}
	}

	// Reuse the caller's slice capacity when it suffices; growth (if needed)
	// is a fresh allocation, matching Decode.
	row := dst
	if cap(row) < len(schema.Columns) {
		row = make([]Value, len(schema.Columns))
	}
	row = row[:len(schema.Columns)]
	hasBitmap := expectBitmap > 0
	for i, col := range schema.Columns {
		if hasBitmap && bitmap[i/8]&(1<<uint(i%8)) != 0 {
			row[i] = Null()
			continue
		}
		v, n, err := c.readValueInto(row[i], body[pos:], col, sink)
		if err != nil {
			return nil, fmt.Errorf("rowpack: decode column %d (%q): %w", i, col.Name, err)
		}
		pos += n
		row[i] = v
	}
	if pos != len(body) {
		return nil, fmt.Errorf("rowpack: %d trailing bytes after tuple", len(body)-pos)
	}
	return row, nil
}

// readValueInto decodes one value of column type col from b into the reuse
// slot: for Decimal columns the
// existing *big.Int is kept and reused (colored by the caller decoding into
// the same row slice across rows), avoiding a big.Int allocation per row. For
// all other types the value is freshly built. When sink is non-nil, String
// payloads are materialized through it instead of being copied.
func (c Codec) readValueInto(reuse Value, b []byte, col Column, sink *Sink) (Value, int, error) {
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
	case TypeDateTimeTZ:
		raw, ok := need(16)
		if !ok {
			return Value{}, 0, errors.New("truncated datetime_tz")
		}
		return Value{
			typ: TypeDateTimeTZ,
			i:   int64(binary.LittleEndian.Uint64(raw)),
			u:   uint64(binary.LittleEndian.Uint32(raw[8:])),
			tz:  int32(binary.LittleEndian.Uint32(raw[12:])),
		}, 16, nil
	case TypeString, TypeBytes:
		ln, pfx, ok := getVarLen(b)
		if !ok {
			return Value{}, 0, fmt.Errorf("truncated type %d length", col.Type)
		}
		if uint32(ln) > c.Limits.MaxValueBytes {
			return Value{}, 0, fmt.Errorf("type %d value of %d bytes exceeds limit %d", col.Type, ln, c.Limits.MaxValueBytes)
		}
		raw, ok := need(pfx + ln)
		if !ok {
			return Value{}, 0, fmt.Errorf("truncated type %d payload", col.Type)
		}
		payload := raw[pfx : pfx+ln]
		if col.Type == TypeString && !utf8.Valid(payload) {
			return Value{}, 0, errors.New("string is not valid UTF-8")
		}
		if col.Type == TypeString {
			if sink != nil && sink.String != nil {
				return Value{typ: TypeString, s: sink.String(payload)}, pfx + ln, nil
			}
			return String(string(payload)), pfx + ln, nil
		}
		if sink != nil && sink.Bytes != nil {
			return Value{typ: TypeBytes, by: sink.Bytes(payload)}, pfx + ln, nil
		}
		return bytesValueInto(reuse, payload), pfx + ln, nil
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
		raw, ok := need(12)
		if !ok {
			return Value{}, 0, errors.New("truncated datetime")
		}
		return Value{
			typ: TypeDateTime,
			i:   int64(binary.LittleEndian.Uint64(raw)),
			u:   uint64(binary.LittleEndian.Uint32(raw[8:])),
		}, 12, nil
	case TypeDecimal:
		ln, pfx, ok := getVarLen(b)
		if !ok {
			return Value{}, 0, errors.New("truncated decimal length")
		}
		if uint32(ln) > c.Limits.MaxValueBytes {
			return Value{}, 0, fmt.Errorf("decimal unscaled of %d bytes exceeds limit %d", ln, c.Limits.MaxValueBytes)
		}
		raw, ok := need(pfx + ln)
		if !ok {
			return Value{}, 0, errors.New("truncated decimal payload")
		}
		u := reuse.d.Unscaled
		if u == nil {
			u = new(big.Int)
		}
		if err := decodeBytesInto(u, raw[pfx:pfx+ln]); err != nil {
			return Value{}, 0, err
		}
		return Value{typ: TypeDecimal, d: Decimal{Unscaled: u, Scale: col.Scale}}, pfx + ln, nil
	}
	return Value{}, 0, fmt.Errorf("unsupported type %d", col.Type)
}

// bytesValueInto materializes a Bytes payload into the dst slot, reusing the
// slot's backing array when it is large enough (overwriting it, like Decimal
// reuse).
func bytesValueInto(reuse Value, payload []byte) Value {
	buf := reuse.by[:0]
	if cap(buf) < len(payload) {
		buf = make([]byte, 0, len(payload))
	}
	return Value{typ: TypeBytes, by: append(buf, payload...)}
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
		buf = binary.AppendUvarint(buf, uint64(len(v.s)))
		return append(buf, v.s...), nil
	case TypeBytes:
		if uint32(len(v.by)) > limits.MaxValueBytes {
			return nil, fmt.Errorf("bytes value of %d bytes exceeds limit %d", len(v.by), limits.MaxValueBytes)
		}
		buf = binary.AppendUvarint(buf, uint64(len(v.by)))
		return append(buf, v.by...), nil
	case TypeDate:
		return appendU32(buf, uint32(int32(v.i))), nil
	case TypeTime:
		if v.i < 0 || v.i >= MaxTimeOfDay {
			return nil, fmt.Errorf("time of day %d out of range", v.i)
		}
		return appendU64(buf, uint64(v.i)), nil
	case TypeDateTime:
		buf = appendU64(buf, uint64(v.i))
		return appendU32(buf, uint32(v.u)), nil
	case TypeDateTimeTZ:
		buf = appendU64(buf, uint64(v.i))
		buf = appendU32(buf, uint32(v.u))
		return appendU32(buf, uint32(v.tz)), nil
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
	case TypeInt64, TypeUint64, TypeFloat64, TypeTime:
		return 8
	case TypeDateTime:
		return 12
	case TypeDateTimeTZ:
		return 16
	}
	return 0
}

// getVarLen reads a uvarint length prefix at the start of b. It returns the
// decoded length, the number of bytes the prefix occupied and whether the
// prefix was readable. A truncated prefix, a prefix longer than 10 bytes or a
// length that cannot be represented as an int all report ok=false, so an
// untrusted body can never turn into a huge slice bound.
//
// The prefix width is returned because it varies with the value: string,
// bytes and decimal payloads are addressed relative to it rather than at a
// fixed 4-byte offset.
func getVarLen(b []byte) (ln int, prefix int, ok bool) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, false
	}
	if v > uint64(int(^uint(0)>>1)) {
		return 0, 0, false
	}
	return int(v), n, true
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
