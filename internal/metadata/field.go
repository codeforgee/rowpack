// Package metadata implements the RowPack v1 metadata TLV: the generic
// MetadataRecord envelope, its Field TLV values, the metadata block payload
// (header + directory), and the fixed field schemas of the 13 core record
// types. Unknown non-critical content is preserved losslessly; unknown
// critical content is rejected.
package metadata

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// MaxNesting is the default maximum nested FieldSet/Expression depth.
const MaxNesting = 32

// Field is one TLV field of a metadata record. Value holds the decoded Go
// value per WireType (see typeComments). Raw, when non-nil, holds the exact
// encoded value bytes of an unknown non-critical field and is written back
// verbatim for lossless passthrough.
type Field struct {
	ID       uint16
	WireType fileformat.WireType
	Critical bool
	Repeated bool
	Value    any
	raw      []byte
}

// typeComments documents the Go value type of Field.Value per WireType:
//
//	WireBool          bool
//	WireUint          uint64 (minimal 1/2/4/8 byte LE)
//	WireSint          int64  (minimal 1/2/4/8 byte two's complement LE)
//	WireString        string
//	WireBytes         []byte
//	WireObjectRef     uint64
//	WireStringList    []string
//	WireObjectRefList []uint64
//	WireExpression    []Field (nested)
//	WireFieldSet      []Field (nested)

// NewField builds a Field, choosing the wire type from the Go value when wt
// is zero.
func NewField(id uint16, value any) Field {
	f := Field{ID: id, Value: value}
	f.WireType = wireTypeOf(value)
	return f
}

func wireTypeOf(v any) fileformat.WireType {
	switch v.(type) {
	case bool:
		return fileformat.WireBool
	case uint64:
		return fileformat.WireUint
	case int64:
		return fileformat.WireSint
	case string:
		return fileformat.WireString
	case []byte:
		return fileformat.WireBytes
	case []string:
		return fileformat.WireStringList
	case []uint64:
		return fileformat.WireObjectRefList
	case []Field:
		return fileformat.WireFieldSet
	}
	return 0
}

// fieldsBytes returns the encoded length of a field sequence.
func fieldsBytes(fs []Field, depth int) (int, error) {
	total := 0
	for i := range fs {
		n, err := fieldEncodedLen(&fs[i], depth)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// fieldEncodedLen returns the total encoded length of one field.
func fieldEncodedLen(f *Field, depth int) (int, error) {
	vl, err := fieldValueLen(f, depth)
	if err != nil {
		return 0, err
	}
	return 8 + vl, nil
}

// fieldValueLen returns the encoded length of a field's value bytes.
func fieldValueLen(f *Field, depth int) (int, error) {
	if f.raw != nil {
		return len(f.raw), nil
	}
	if depth > MaxNesting {
		return 0, fmt.Errorf("rowpack: metadata nesting depth exceeds %d", MaxNesting)
	}
	switch f.WireType {
	case fileformat.WireBool:
		return 1, nil
	case fileformat.WireUint:
		if v, ok := f.Value.(uint64); ok {
			return uintWidth(v), nil
		}
		return 0, fmt.Errorf("rowpack: WireUint field %d has value %T", f.ID, f.Value)
	case fileformat.WireSint:
		if v, ok := f.Value.(int64); ok {
			return sintWidth(v), nil
		}
		return 0, fmt.Errorf("rowpack: WireSint field %d has value %T", f.ID, f.Value)
	case fileformat.WireString:
		if v, ok := f.Value.(string); ok {
			return len(v), nil
		}
		return 0, fmt.Errorf("rowpack: WireString field %d has value %T", f.ID, f.Value)
	case fileformat.WireBytes:
		if v, ok := f.Value.([]byte); ok {
			return len(v), nil
		}
		return 0, fmt.Errorf("rowpack: WireBytes field %d has value %T", f.ID, f.Value)
	case fileformat.WireObjectRef:
		return 8, nil
	case fileformat.WireStringList:
		vs, ok := f.Value.([]string)
		if !ok {
			return 0, fmt.Errorf("rowpack: WireStringList field %d has value %T", f.ID, f.Value)
		}
		n := 4
		for _, s := range vs {
			n += 4 + len(s)
		}
		return n, nil
	case fileformat.WireObjectRefList:
		vs, ok := f.Value.([]uint64)
		if !ok {
			return 0, fmt.Errorf("rowpack: WireObjectRefList field %d has value %T", f.ID, f.Value)
		}
		return 4 + 8*len(vs), nil
	case fileformat.WireExpression, fileformat.WireFieldSet:
		vs, ok := f.Value.([]Field)
		if !ok {
			return 0, fmt.Errorf("rowpack: WireFieldSet field %d has value %T", f.ID, f.Value)
		}
		n := 4
		for i := range vs {
			ln, err := fieldEncodedLen(&vs[i], depth+1)
			if err != nil {
				return 0, err
			}
			n += ln
		}
		return n, nil
	}
	return 0, fmt.Errorf("rowpack: unsupported wire type %d", f.WireType)
}

func uintWidth(v uint64) int {
	switch {
	case v <= 0xFF:
		return 1
	case v <= 0xFFFF:
		return 2
	case v <= 0xFFFFFFFF:
		return 4
	}
	return 8
}

func sintWidth(v int64) int {
	switch {
	case v >= -128 && v <= 127:
		return 1
	case v >= -32768 && v <= 32767:
		return 2
	case v >= -2147483648 && v <= 2147483647:
		return 4
	}
	return 8
}

// encodeField appends the encoded field to dst.
func encodeField(dst []byte, f *Field, depth int) ([]byte, error) {
	vl, err := fieldValueLen(f, depth)
	if err != nil {
		return nil, err
	}
	dst = appendU16(dst, f.ID)
	dst = append(dst, byte(f.WireType))
	var flags byte
	if f.Critical {
		flags |= fileformat.FieldFlagCritical
	}
	if f.Repeated {
		flags |= fileformat.FieldFlagRepeated
	}
	dst = append(dst, flags)
	dst = appendU32(dst, uint32(vl))
	val, err := encodeFieldValue(f, depth)
	if err != nil {
		return nil, err
	}
	return append(dst, val...), nil
}

func encodeFieldValue(f *Field, depth int) ([]byte, error) {
	if f.raw != nil {
		return f.raw, nil
	}
	if depth > MaxNesting {
		return nil, fmt.Errorf("rowpack: metadata nesting depth exceeds %d", MaxNesting)
	}
	switch f.WireType {
	case fileformat.WireBool:
		b, ok := f.Value.(bool)
		if !ok {
			return nil, fmt.Errorf("rowpack: WireBool field %d has value %T", f.ID, f.Value)
		}
		if b {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case fileformat.WireUint:
		v := f.Value.(uint64)
		w := uintWidth(v)
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], v)
		return tmp[:w], nil
	case fileformat.WireSint:
		v := f.Value.(int64)
		w := sintWidth(v)
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], uint64(v))
		return tmp[:w], nil
	case fileformat.WireString:
		return []byte(f.Value.(string)), nil
	case fileformat.WireBytes:
		return f.Value.([]byte), nil
	case fileformat.WireObjectRef:
		b := make([]byte, 8)
		binary.LittleEndian.PutUint64(b, f.Value.(uint64))
		return b, nil
	case fileformat.WireStringList:
		vs := f.Value.([]string)
		dst := appendU32(nil, uint32(len(vs)))
		for _, s := range vs {
			dst = appendU32(dst, uint32(len(s)))
			dst = append(dst, s...)
		}
		return dst, nil
	case fileformat.WireObjectRefList:
		vs := f.Value.([]uint64)
		dst := appendU32(nil, uint32(len(vs)))
		for _, v := range vs {
			dst = appendU64(dst, v)
		}
		return dst, nil
	case fileformat.WireExpression, fileformat.WireFieldSet:
		vs := f.Value.([]Field)
		dst := appendU32(nil, uint32(len(vs)))
		for i := range vs {
			var err error
			dst, err = encodeField(dst, &vs[i], depth+1)
			if err != nil {
				return nil, err
			}
		}
		return dst, nil
	}
	return nil, fmt.Errorf("rowpack: unsupported wire type %d", f.WireType)
}

// decodeField parses one field from src, returning the field, bytes consumed,
// and the next depth for nested values. The exact encoded value bytes are
// captured on the returned Field so unknown non-critical fields can be
// written back verbatim.
func decodeField(src []byte, depth int) (Field, int, error) {
	if depth > MaxNesting {
		return Field{}, 0, fmt.Errorf("rowpack: metadata nesting depth exceeds %d", MaxNesting)
	}
	if len(src) < 8 {
		return Field{}, 0, errors.New("rowpack: truncated field header")
	}
	f := Field{
		ID:       binary.LittleEndian.Uint16(src[0:]),
		WireType: fileformat.WireType(src[2]),
	}
	flags := src[3]
	f.Critical = flags&fileformat.FieldFlagCritical != 0
	f.Repeated = flags&fileformat.FieldFlagRepeated != 0
	vl := binary.LittleEndian.Uint32(src[4:])
	pos := 8
	if int(vl) > len(src)-pos {
		return Field{}, 0, fmt.Errorf("rowpack: field %d value length %d exceeds input", f.ID, vl)
	}
	f.raw = src[pos : pos+int(vl)]
	value, n, err := decodeFieldValue(f, src[pos:pos+int(vl)], depth)
	if err != nil {
		return Field{}, 0, err
	}
	pos += n
	if pos != 8+int(vl) {
		return Field{}, 0, fmt.Errorf("rowpack: field %d value %d bytes but parsed %d", f.ID, vl, n)
	}
	f.Value = value
	return f, pos, nil
}

func decodeFieldValue(f Field, src []byte, depth int) (any, int, error) {
	switch f.WireType {
	case fileformat.WireBool:
		if len(src) != 1 {
			return nil, 0, fmt.Errorf("rowpack: WireBool field %d length %d", f.ID, len(src))
		}
		if src[0] > 1 {
			return nil, 0, fmt.Errorf("rowpack: WireBool field %d has byte 0x%02x", f.ID, src[0])
		}
		return src[0] == 1, 1, nil
	case fileformat.WireUint:
		var v uint64
		switch len(src) {
		case 1:
			v = uint64(src[0])
		case 2:
			v = uint64(binary.LittleEndian.Uint16(src))
		case 4:
			v = uint64(binary.LittleEndian.Uint32(src))
		case 8:
			v = binary.LittleEndian.Uint64(src)
		default:
			return nil, 0, fmt.Errorf("rowpack: WireUint field %d length %d", f.ID, len(src))
		}
		return v, len(src), nil
	case fileformat.WireSint:
		var v int64
		switch len(src) {
		case 1:
			v = int64(int8(src[0]))
		case 2:
			v = int64(int16(binary.LittleEndian.Uint16(src)))
		case 4:
			v = int64(int32(binary.LittleEndian.Uint32(src)))
		case 8:
			v = int64(binary.LittleEndian.Uint64(src))
		default:
			return nil, 0, fmt.Errorf("rowpack: WireSint field %d length %d", f.ID, len(src))
		}
		return v, len(src), nil
	case fileformat.WireString:
		return string(src), len(src), nil
	case fileformat.WireBytes:
		return src, len(src), nil
	case fileformat.WireObjectRef:
		if len(src) != 8 {
			return nil, 0, fmt.Errorf("rowpack: WireObjectRef field %d length %d", f.ID, len(src))
		}
		return binary.LittleEndian.Uint64(src), 8, nil
	case fileformat.WireStringList:
		return decodeStringList(src)
	case fileformat.WireObjectRefList:
		if len(src) < 4 || (len(src)-4)%8 != 0 {
			return nil, 0, fmt.Errorf("rowpack: WireObjectRefList field %d length %d", f.ID, len(src))
		}
		n := int(binary.LittleEndian.Uint32(src))
		if 4+8*n != len(src) {
			return nil, 0, fmt.Errorf("rowpack: WireObjectRefList field %d count mismatch", f.ID)
		}
		out := make([]uint64, n)
		for i := 0; i < n; i++ {
			out[i] = binary.LittleEndian.Uint64(src[4+8*i:])
		}
		return out, len(src), nil
	case fileformat.WireExpression, fileformat.WireFieldSet:
		return decodeFieldSet(src, depth+1)
	}
	return nil, 0, fmt.Errorf("rowpack: unsupported wire type %d", f.WireType)
}

func decodeStringList(src []byte) (any, int, error) {
	if len(src) < 4 {
		return nil, 0, errors.New("rowpack: truncated string list")
	}
	n := int(binary.LittleEndian.Uint32(src))
	out := make([]string, 0, n)
	pos := 4
	for i := 0; i < n; i++ {
		if len(src)-pos < 4 {
			return nil, 0, errors.New("rowpack: truncated string list item length")
		}
		ln := int(binary.LittleEndian.Uint32(src[pos:]))
		pos += 4
		if ln > len(src)-pos {
			return nil, 0, errors.New("rowpack: truncated string list item")
		}
		out = append(out, string(src[pos:pos+ln]))
		pos += ln
	}
	if pos != len(src) {
		return nil, 0, errors.New("rowpack: trailing bytes in string list")
	}
	return out, pos, nil
}

func decodeFieldSet(src []byte, depth int) (any, int, error) {
	if len(src) < 4 {
		return nil, 0, errors.New("rowpack: truncated field set count")
	}
	n := int(binary.LittleEndian.Uint32(src))
	pos := 4
	out := make([]Field, 0, n)
	for i := 0; i < n; i++ {
		if pos >= len(src) {
			return nil, 0, errors.New("rowpack: truncated field set item")
		}
		f, used, err := decodeField(src[pos:], depth)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, f)
		pos += used
	}
	if pos != len(src) {
		return nil, 0, errors.New("rowpack: trailing bytes in field set")
	}
	return out, pos, nil
}

// checkCanonical validates canonical ordering: fields must be sorted by ID
// ascending, and only fields with the Repeated flag may repeat.
func checkCanonical(fs []Field) error {
	for i := 1; i < len(fs); i++ {
		if fs[i].ID < fs[i-1].ID {
			return fmt.Errorf("rowpack: fields not sorted by ID (%d after %d)", fs[i].ID, fs[i-1].ID)
		}
		if fs[i].ID == fs[i-1].ID && !fs[i].Repeated {
			return fmt.Errorf("rowpack: field %d repeated without Repeated flag", fs[i].ID)
		}
	}
	return nil
}

func appendU16(dst []byte, v uint16) []byte {
	return append(dst, byte(v), byte(v>>8))
}
func appendU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
func appendU64(dst []byte, v uint64) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24),
		byte(v>>32), byte(v>>40), byte(v>>48), byte(v>>56))
}
