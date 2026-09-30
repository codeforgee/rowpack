// Package metadata implements the RowPack v1 metadata TLV: the generic
// MetadataRecord envelope, its Field TLV values, and the metadata block
// payload (header + directory). DefineSchema writes Table/Column schema
// records with fixed field schemas (see corefields.go). Unknown non-critical
// content is preserved losslessly; unknown critical content is rejected.
package metadata

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/codeforgee/rowpack/internal/format"
)

// Field is one TLV field of a metadata record. Value holds the decoded Go
// value: string for WireString, int64 for WireSint, nil for a non-critical
// field whose wire type the engine does not interpret (the exact encoded
// bytes are kept in raw and written back verbatim for lossless passthrough).
type Field struct {
	ID       uint16
	WireType format.WireType
	Critical bool
	Repeated bool
	Value    any
	raw      []byte
}

// errUnsupportedWireType marks a field whose wire type number is a reserved
// format value the engine does not interpret.
var errUnsupportedWireType = errors.New("rowpack: unsupported wire type")

// fieldsBytes returns the encoded length of a field sequence.
func fieldsBytes(fs []Field) (int, error) {
	total := 0
	for i := range fs {
		n, err := fieldEncodedLen(&fs[i])
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// fieldEncodedLen returns the total encoded length of one field.
func fieldEncodedLen(f *Field) (int, error) {
	vl, err := fieldValueLen(f)
	if err != nil {
		return 0, err
	}
	return 8 + vl, nil
}

// fieldValueLen returns the encoded length of a field's value bytes.
func fieldValueLen(f *Field) (int, error) {
	if f.raw != nil {
		return len(f.raw), nil
	}
	switch f.WireType {
	case format.WireSint:
		if v, ok := f.Value.(int64); ok {
			return sintWidth(v), nil
		}
		return 0, fmt.Errorf("rowpack: WireSint field %d has value %T", f.ID, f.Value)
	case format.WireString:
		if v, ok := f.Value.(string); ok {
			return len(v), nil
		}
		return 0, fmt.Errorf("rowpack: WireString field %d has value %T", f.ID, f.Value)
	}
	return 0, fmt.Errorf("%w %d on field %d", errUnsupportedWireType, f.WireType, f.ID)
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
func encodeField(dst []byte, f *Field) ([]byte, error) {
	vl, err := fieldValueLen(f)
	if err != nil {
		return nil, err
	}
	dst = appendU16(dst, f.ID)
	dst = append(dst, byte(f.WireType))
	var flags byte
	if f.Critical {
		flags |= format.FieldFlagCritical
	}
	if f.Repeated {
		flags |= format.FieldFlagRepeated
	}
	dst = append(dst, flags)
	dst = appendU32(dst, uint32(vl))
	val, err := encodeFieldValue(f)
	if err != nil {
		return nil, err
	}
	return append(dst, val...), nil
}

func encodeFieldValue(f *Field) ([]byte, error) {
	if f.raw != nil {
		return f.raw, nil
	}
	switch f.WireType {
	case format.WireSint:
		v := f.Value.(int64)
		w := sintWidth(v)
		var tmp [8]byte
		binary.LittleEndian.PutUint64(tmp[:], uint64(v))
		return tmp[:w], nil
	case format.WireString:
		return []byte(f.Value.(string)), nil
	}
	return nil, fmt.Errorf("%w %d on field %d", errUnsupportedWireType, f.WireType, f.ID)
}

// decodeField parses one field from src, returning the field and bytes
// consumed. The exact encoded value bytes are always captured on the
// returned Field so unknown non-critical fields can be written back
// verbatim.
func decodeField(src []byte) (Field, int, error) {
	if len(src) < 8 {
		return Field{}, 0, errors.New("rowpack: truncated field header")
	}
	f := Field{
		ID:       binary.LittleEndian.Uint16(src[0:]),
		WireType: format.WireType(src[2]),
	}
	flags := src[3]
	f.Critical = flags&format.FieldFlagCritical != 0
	f.Repeated = flags&format.FieldFlagRepeated != 0
	vl := binary.LittleEndian.Uint32(src[4:])
	pos := 8
	if int(vl) > len(src)-pos {
		return Field{}, 0, fmt.Errorf("rowpack: field %d value length %d exceeds input", f.ID, vl)
	}
	f.raw = src[pos : pos+int(vl)]
	value, err := decodeFieldValue(f, src[pos:pos+int(vl)])
	switch {
	case err == nil:
		f.Value = value
	case errors.Is(err, errUnsupportedWireType) && !f.Critical:
		// Reserved wire type on a non-critical field: keep the exact raw
		// bytes for lossless passthrough without interpreting the value.
		f.Value = nil
	default:
		return Field{}, 0, err
	}
	pos += int(vl)
	return f, pos, nil
}

func decodeFieldValue(f Field, src []byte) (any, error) {
	switch f.WireType {
	case format.WireSint:
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
			return nil, fmt.Errorf("rowpack: WireSint field %d length %d", f.ID, len(src))
		}
		return v, nil
	case format.WireString:
		return string(src), nil
	}
	return nil, fmt.Errorf("%w %d on field %d", errUnsupportedWireType, f.WireType, f.ID)
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
