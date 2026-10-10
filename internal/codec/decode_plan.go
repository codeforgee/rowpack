package codec

import (
	"encoding/binary"
	"math"
	"math/big"
	"unicode/utf8"
)

// decodeKind is the decode operation of one column. The values are dense so
// the decode loop's switch compiles to a jump table, and the mapping from
// Type is resolved once, in CompileDecoder, instead of per row and column.
type decodeKind uint8

const (
	decodeBool decodeKind = iota
	decodeInt8
	decodeInt16
	decodeInt32
	decodeInt64
	decodeUint8
	decodeUint16
	decodeUint32
	decodeUint64
	decodeFloat32
	decodeFloat64
	decodeDate
	decodeTime
	decodeDateTime
	decodeDateTimeTZ
	decodeString
	decodeBytes
	decodeDecimal
	decodeUnsupported
)

// decodeStep is the precompiled decode operation of one column: the type to
// stamp on the value, the opcode, the payload width (0 = length-prefixed) and
// the null bitmap position. Compiling it once removes the per-row/column type
// switch, fixedWidth lookup and column-struct walk from the hot decode loop.
type decodeStep struct {
	typ     Type
	kind    decodeKind
	width   int   // payload width, 0 = length-prefixed
	bitByte int   // null bitmap byte holding this column's bit
	bitMask byte  // that byte's mask for this column
	scale   int32 // Decimal scale
}

// compileDecodeSteps builds the column plan for a schema. fixed reports
// whether every column is non-nullable and fixed-width (the plan is then a
// pure sequence of bounded loads).
func compileDecodeSteps(schema *Schema) (steps []decodeStep, fixed bool) {
	steps = make([]decodeStep, len(schema.Columns))
	fixed = true
	for i := range schema.Columns {
		col := &schema.Columns[i]
		s := decodeStep{
			typ:     col.Type,
			width:   fixedWidth(col.Type),
			bitByte: i / 8,
			bitMask: 1 << uint(i%8),
			scale:   col.Scale,
		}
		switch col.Type {
		case TypeBool:
			s.kind, s.width = decodeBool, 1
		case TypeInt8:
			s.kind = decodeInt8
		case TypeInt16:
			s.kind = decodeInt16
		case TypeInt32:
			s.kind = decodeInt32
		case TypeInt64:
			s.kind = decodeInt64
		case TypeUint8:
			s.kind = decodeUint8
		case TypeUint16:
			s.kind = decodeUint16
		case TypeUint32:
			s.kind = decodeUint32
		case TypeUint64:
			s.kind = decodeUint64
		case TypeFloat32:
			s.kind = decodeFloat32
		case TypeFloat64:
			s.kind = decodeFloat64
		case TypeDate:
			s.kind = decodeDate
		case TypeTime:
			s.kind = decodeTime
		case TypeDateTime:
			s.kind = decodeDateTime
		case TypeDateTimeTZ:
			s.kind, s.width = decodeDateTimeTZ, 16
		case TypeString:
			s.kind, s.width = decodeString, 0
		case TypeBytes:
			s.kind, s.width = decodeBytes, 0
		case TypeDecimal:
			s.kind, s.width = decodeDecimal, 0
		default:
			s.kind, s.width = decodeUnsupported, 0
		}
		if s.width == 0 || col.Nullable {
			fixed = false
		}
		steps[i] = s
	}
	return steps, fixed
}

// fastDecode is the plan-driven decode of one body: it returns ok=false on
// anything the plan cannot prove valid (truncation, invalid value, limits,
// unused bitmap bits, trailing bytes), and the caller then runs the generic
// validator, which produces the canonical error. Valid data never takes that
// path.
func (d Decoder) fastDecode(dst []Value, body []byte, sink *Sink) ([]Value, bool) {
	if len(body) < d.bitmapBytes {
		return nil, false
	}
	bitmap := body[:d.bitmapBytes]
	if d.tailBitmapMask != 0 && bitmap[d.bitmapBytes-1]&d.tailBitmapMask != 0 {
		return nil, false
	}
	payload := body[d.bitmapBytes:]

	steps := d.steps
	row := dst
	if cap(row) < len(steps) {
		row = make([]Value, len(steps))
	}
	row = row[:len(steps)]
	pos := 0
	// A schema without a nullable column carries no bitmap, so no column can
	// be NULL and the bitmap test is skipped entirely.
	hasBitmap := d.bitmapBytes > 0
	for i := range steps {
		s := &steps[i]
		// A set bit means NULL and consumes no payload, for nullable and
		// non-nullable columns alike (the generic decoder does the same).
		if hasBitmap && bitmap[s.bitByte]&s.bitMask != 0 {
			row[i] = Null()
			continue
		}
		if w := s.width; w != 0 {
			if len(payload)-pos < w {
				return nil, false
			}
			switch s.kind {
			case decodeBool:
				switch payload[pos] {
				case 0:
					row[i] = Value{typ: s.typ}
				case 1:
					row[i] = Value{typ: s.typ, b: true}
				default:
					return nil, false
				}
			case decodeInt8:
				row[i] = Value{typ: s.typ, i: int64(int8(payload[pos]))}
			case decodeInt16:
				row[i] = Value{typ: s.typ, i: int64(int16(binary.LittleEndian.Uint16(payload[pos:])))}
			case decodeInt32:
				row[i] = Value{typ: s.typ, i: int64(int32(binary.LittleEndian.Uint32(payload[pos:])))}
			case decodeInt64:
				row[i] = Value{typ: s.typ, i: int64(binary.LittleEndian.Uint64(payload[pos:]))}
			case decodeUint8:
				row[i] = Value{typ: s.typ, u: uint64(payload[pos])}
			case decodeUint16:
				row[i] = Value{typ: s.typ, u: uint64(binary.LittleEndian.Uint16(payload[pos:]))}
			case decodeUint32:
				row[i] = Value{typ: s.typ, u: uint64(binary.LittleEndian.Uint32(payload[pos:]))}
			case decodeUint64:
				row[i] = Value{typ: s.typ, u: binary.LittleEndian.Uint64(payload[pos:])}
			case decodeFloat32:
				row[i] = Value{typ: s.typ, f32: math.Float32frombits(binary.LittleEndian.Uint32(payload[pos:]))}
			case decodeFloat64:
				row[i] = Value{typ: s.typ, f64: math.Float64frombits(binary.LittleEndian.Uint64(payload[pos:]))}
			case decodeDate:
				row[i] = Value{typ: s.typ, i: int64(int32(binary.LittleEndian.Uint32(payload[pos:])))}
			case decodeTime:
				ns := int64(binary.LittleEndian.Uint64(payload[pos:]))
				if ns < 0 || ns >= MaxTimeOfDay {
					return nil, false
				}
				row[i] = Value{typ: s.typ, i: ns}
			case decodeDateTime:
				row[i] = Value{
					typ: s.typ,
					i:   int64(binary.LittleEndian.Uint64(payload[pos:])),
					u:   uint64(binary.LittleEndian.Uint32(payload[pos+8:])),
				}
			case decodeDateTimeTZ:
				row[i] = Value{
					typ: s.typ,
					i:   int64(binary.LittleEndian.Uint64(payload[pos:])),
					u:   uint64(binary.LittleEndian.Uint32(payload[pos+8:])),
					tz:  int32(binary.LittleEndian.Uint32(payload[pos+12:])),
				}
			}
			pos += w
			continue
		}
		switch s.kind {
		case decodeString, decodeBytes:
			ln, pfx, ok := getVarLen(payload[pos:])
			if !ok || uint32(ln) > d.codec.Limits.MaxValueBytes || ln > len(payload)-pos-pfx {
				return nil, false
			}
			raw := payload[pos+pfx : pos+pfx+ln]
			if s.kind == decodeString {
				if !utf8.Valid(raw) {
					return nil, false
				}
				if sink != nil && sink.String != nil {
					row[i] = Value{typ: s.typ, s: sink.String(raw)}
				} else {
					row[i] = Value{typ: s.typ, s: string(raw)}
				}
			} else {
				if sink != nil && sink.Bytes != nil {
					row[i] = Value{typ: s.typ, by: sink.Bytes(raw)}
				} else {
					row[i] = bytesValueInto(row[i], raw)
				}
			}
			pos += pfx + ln
		case decodeDecimal:
			ln, pfx, ok := getVarLen(payload[pos:])
			if !ok || uint32(ln) > d.codec.Limits.MaxValueBytes || ln > len(payload)-pos-pfx {
				return nil, false
			}
			raw := payload[pos+pfx : pos+pfx+ln]
			u := row[i].d.Unscaled
			if u == nil {
				u = new(big.Int)
			}
			if err := decodeBytesInto(u, raw); err != nil {
				return nil, false
			}
			row[i] = Value{typ: s.typ, d: Decimal{Unscaled: u, Scale: s.scale}}
			pos += pfx + ln
		default:
			return nil, false
		}
	}
	if pos != len(payload) {
		return nil, false
	}
	return row, true
}
