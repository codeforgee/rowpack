package metadata

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/codeforgee/rowpack/internal/format"
)

// RecordEnvelopeHeaderSize is the fixed header size before namespace bytes.
const RecordEnvelopeHeaderSize = 48

// Record is one generic metadata record: the envelope plus its fields.
type Record struct {
	RecordType  uint32
	ObjectID    uint64
	ParentID    uint64
	Revision    uint32
	Critical    bool
	Namespace   string
	ExternalKey string
	Fields      []Field
}

// KnownFieldSchema is the canonical set of fields of a known record type.
// The key is the FieldID; the value is the required wire type. Fields not in
// the schema are unknown to the implementation.
type KnownFieldSchema map[uint16]format.WireType

// Encode serializes the record. The schema is used to validate known field
// wire types; unknown fields (not in the schema) are passed through verbatim.
// Fields are written in canonical (sorted-by-ID) order.
func (r *Record) Encode(schema KnownFieldSchema) ([]byte, error) {
	if r.Namespace == "" {
		return nil, errors.New("rowpack: metadata record namespace is empty")
	}
	if r.ObjectID == 0 {
		return nil, errors.New("rowpack: metadata record object id is zero")
	}
	fields := make([]Field, len(r.Fields))
	copy(fields, r.Fields)
	sortFields(fields)
	if err := checkCanonical(fields); err != nil {
		return nil, err
	}
	if schema != nil {
		for i := range fields {
			if wt, ok := schema[fields[i].ID]; ok {
				if fields[i].WireType == 0 {
					fields[i].WireType = wt
				} else if fields[i].WireType != wt {
					return nil, fmt.Errorf("rowpack: field %d of record %d uses wire type %d, want %d", fields[i].ID, r.RecordType, fields[i].WireType, wt)
				}
			}
		}
	}
	fieldsLen, err := fieldsBytes(fields)
	if err != nil {
		return nil, err
	}
	total := RecordEnvelopeHeaderSize + len(r.Namespace) + len(r.ExternalKey) + fieldsLen + 4
	if total > 1<<31 {
		return nil, errors.New("rowpack: metadata record too large")
	}
	dst := make([]byte, 0, total)
	dst = appendU32(dst, uint32(total))
	dst = appendU32(dst, r.RecordType)
	dst = appendU64(dst, r.ObjectID)
	dst = appendU64(dst, r.ParentID)
	dst = appendU32(dst, r.Revision)
	var flags uint32
	if r.Critical {
		flags |= format.FlagCritical
	}
	dst = appendU32(dst, flags)
	dst = appendU32(dst, uint32(len(r.Namespace)))
	dst = appendU32(dst, uint32(len(r.ExternalKey)))
	dst = appendU32(dst, uint32(len(fields)))
	dst = appendU32(dst, uint32(fieldsLen))
	dst = append(dst, r.Namespace...)
	dst = append(dst, r.ExternalKey...)
	for i := range fields {
		dst, err = encodeField(dst, &fields[i])
		if err != nil {
			return nil, err
		}
	}
	// CRC over everything except the trailing 4 CRC bytes.
	crc := format.CRC32C(dst)
	dst = appendU32(dst, crc)
	return dst, nil
}

// Decode parses and validates a record envelope. known may be nil for unknown
// record types; in that case unknown fields are preserved. It validates the
// trailing CRC and canonical field ordering.
func (r *Record) Decode(src []byte, known KnownFieldSchema) error {
	if len(src) < RecordEnvelopeHeaderSize+4 {
		return errors.New("rowpack: truncated metadata record")
	}
	recordLen := int(binary.LittleEndian.Uint32(src[0:]))
	if recordLen < RecordEnvelopeHeaderSize+4 {
		return fmt.Errorf("rowpack: metadata record length %d too small", recordLen)
	}
	if recordLen != len(src) {
		return fmt.Errorf("rowpack: metadata record length %d does not match input %d", recordLen, len(src))
	}
	body := src[:recordLen]
	// Verify CRC over all bytes except the trailing 4.
	stored := binary.LittleEndian.Uint32(body[recordLen-4:])
	computed := format.CRC32C(body[:recordLen-4])
	if stored != computed {
		return fmt.Errorf("rowpack: metadata record CRC mismatch: stored 0x%08x computed 0x%08x", stored, computed)
	}
	r.RecordType = binary.LittleEndian.Uint32(body[4:])
	r.ObjectID = binary.LittleEndian.Uint64(body[8:])
	r.ParentID = binary.LittleEndian.Uint64(body[16:])
	r.Revision = binary.LittleEndian.Uint32(body[24:])
	r.Critical = binary.LittleEndian.Uint32(body[28:])&format.FlagCritical != 0
	nsLen := int(binary.LittleEndian.Uint32(body[32:]))
	ekLen := int(binary.LittleEndian.Uint32(body[36:]))
	fieldCount := int(binary.LittleEndian.Uint32(body[40:]))
	fieldsLen := int(binary.LittleEndian.Uint32(body[44:]))

	fieldsEnd := recordLen - 4
	headerEnd := RecordEnvelopeHeaderSize + nsLen + ekLen
	if headerEnd > fieldsEnd {
		return errors.New("rowpack: metadata record namespace/ekey lengths exceed record")
	}
	r.Namespace = string(body[RecordEnvelopeHeaderSize : RecordEnvelopeHeaderSize+nsLen])
	r.ExternalKey = string(body[RecordEnvelopeHeaderSize+nsLen : headerEnd])
	if fieldsLen != fieldsEnd-headerEnd {
		return fmt.Errorf("rowpack: metadata record fields length %d, want %d", fieldsLen, fieldsEnd-headerEnd)
	}
	pos := headerEnd
	fields := make([]Field, 0, fieldCount)
	for i := 0; i < fieldCount; i++ {
		if pos >= fieldsEnd {
			return fmt.Errorf("rowpack: metadata record field %d exceeds fields region", i)
		}
		f, used, err := decodeField(body[pos:fieldsEnd])
		if err != nil {
			return err
		}
		pos += used
		fields = append(fields, f)
	}
	if pos != fieldsEnd {
		return fmt.Errorf("rowpack: metadata record fields end at %d, want %d", pos, fieldsEnd)
	}
	if err := checkCanonical(fields); err != nil {
		return err
	}
	// Validate known fields against the schema; reject unknown critical fields.
	if known != nil {
		for i := range fields {
			if wt, ok := known[fields[i].ID]; ok {
				if fields[i].WireType != wt {
					return fmt.Errorf("rowpack: field %d of record %d wire type %d, want %d", fields[i].ID, r.RecordType, fields[i].WireType, wt)
				}
				if !fields[i].Repeated {
					if err := ensureSingle(fields, i); err != nil {
						return err
					}
				}
			} else if fields[i].Critical {
				return fmt.Errorf("rowpack: unknown critical field %d of record %d", fields[i].ID, r.RecordType)
			} else {
				// Unknown non-critical field: keep the exact raw value bytes for
				// lossless passthrough and drop the parsed value.
				fields[i].Value = nil
			}
		}
	}
	r.Fields = fields
	return nil
}

// ensureSingle rejects repeated occurrences of a non-Repeated known field.
func ensureSingle(fields []Field, idx int) error {
	for i := 0; i < idx; i++ {
		if fields[i].ID == fields[idx].ID {
			return fmt.Errorf("rowpack: field %d appears multiple times without Repeated flag", fields[idx].ID)
		}
	}
	return nil
}

func sortFields(fs []Field) {
	// Insertion sort by ID, stable (order within same ID preserved).
	for i := 1; i < len(fs); i++ {
		for j := i; j > 0 && fs[j].ID < fs[j-1].ID; j-- {
			fs[j], fs[j-1] = fs[j-1], fs[j]
		}
	}
}

// FieldByID returns the first field with the given ID, or nil.
func (r *Record) FieldByID(id uint16) *Field {
	for i := range r.Fields {
		if r.Fields[i].ID == id {
			return &r.Fields[i]
		}
	}
	return nil
}
