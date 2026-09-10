package metadata

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// fixCRC recomputes the trailing CRC after a test mutates encoded bytes.
func fixCRC(b []byte) {
	binary.LittleEndian.PutUint32(b[len(b)-4:], fileformat.CRC32C(b[:len(b)-4]))
}

func schemaFixture() KnownFieldSchema {
	return KnownFieldSchema{
		1: fileformat.WireString,
		2: fileformat.WireSint,
		3: fileformat.WireString,
	}
}

func sampleRecord() Record {
	return Record{
		RecordType:  uint32(fileformat.RecordTable),
		ObjectID:    42,
		ParentID:    7,
		Revision:    3,
		Critical:    true,
		Namespace:   "rowpack.meta.v1",
		ExternalKey: "ext",
		Fields: []Field{
			{ID: 1, WireType: fileformat.WireString, Value: "users"},
			{ID: 2, WireType: fileformat.WireSint, Value: int64(10)},
		},
	}
}

func TestRecordEncodeDecodeRoundtrip(t *testing.T) {
	schema := schemaFixture()
	rec := sampleRecord()
	enc, err := rec.Encode(schema)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	var r Record
	if err := r.Decode(enc, schema); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.RecordType != uint32(fileformat.RecordTable) || r.ObjectID != 42 ||
		r.ParentID != 7 || r.Revision != 3 || !r.Critical ||
		r.Namespace != "rowpack.meta.v1" || r.ExternalKey != "ext" {
		t.Fatalf("decoded record mismatch: %+v", r)
	}
	if len(r.Fields) != 2 {
		t.Fatalf("fields %d, want 2", len(r.Fields))
	}
	if r.Fields[0].ID != 1 || r.Fields[0].Value != "users" {
		t.Fatalf("field 0 mismatch: %+v", r.Fields[0])
	}
	if r.Fields[1].ID != 2 || r.Fields[1].Value != int64(10) {
		t.Fatalf("field 1 mismatch: %+v", r.Fields[1])
	}
	if got := r.FieldByID(1); got == nil || got.Value != "users" {
		t.Fatalf("FieldByID(1): %+v", got)
	}
	if got := r.FieldByID(99); got != nil {
		t.Fatalf("FieldByID(99) should be nil, got %+v", got)
	}
}

func TestRecordEncodeSortsFields(t *testing.T) {
	rec := sampleRecord()
	// Reverse order; Encode must sort by ID.
	rec.Fields = []Field{
		{ID: 2, WireType: fileformat.WireSint, Value: int64(10)},
		{ID: 1, WireType: fileformat.WireString, Value: "users"},
	}
	enc, err := rec.Encode(schemaFixture())
	if err != nil {
		t.Fatalf("Encode with unsorted input: %v", err)
	}
	var r Record
	if err := r.Decode(enc, schemaFixture()); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.Fields[0].ID != 1 || r.Fields[1].ID != 2 {
		t.Fatalf("fields not sorted: %+v", r.Fields)
	}
}

func TestRecordEncodeWireTypeFromSchema(t *testing.T) {
	rec := sampleRecord()
	// WireType left zero; Encode fills it from the schema.
	rec.Fields = []Field{{ID: 1, Value: "users"}, {ID: 2, Value: int64(10)}}
	enc, err := rec.Encode(schemaFixture())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var r Record
	if err := r.Decode(enc, schemaFixture()); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if r.Fields[0].WireType != fileformat.WireString {
		t.Fatalf("field wire type not filled from schema: %+v", r.Fields[0])
	}
}

func TestRecordEncodeSchemaMismatch(t *testing.T) {
	rec := sampleRecord()
	rec.Fields = []Field{{ID: 1, WireType: fileformat.WireSint, Value: int64(1)}}
	if _, err := rec.Encode(schemaFixture()); err == nil {
		t.Fatal("mismatched wire type against schema should error")
	}
}

func TestRecordUnknownFieldsPassthrough(t *testing.T) {
	rec := sampleRecord()
	known := KnownFieldSchema{1: fileformat.WireString}
	rec.Fields = []Field{
		{ID: 9, WireType: fileformat.WireString, Value: "unknown-noncritical"},
		{ID: 1, WireType: fileformat.WireString, Value: "users"},
	}
	enc, err := rec.Encode(known)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Decode with a schema: unknown non-critical field is kept raw, value nil.
	var r Record
	if err := r.Decode(enc, known); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	f := r.FieldByID(9)
	if f == nil {
		t.Fatal("unknown non-critical field lost")
	}
	if f.Value != nil {
		t.Fatalf("unknown field value %v, want nil", f.Value)
	}

	// Decode with nil schema keeps the value.
	var r2 Record
	if err := r2.Decode(enc, nil); err != nil {
		t.Fatalf("Decode nil schema: %v", err)
	}
	if got := r2.FieldByID(9); got == nil || got.Value != "unknown-noncritical" {
		t.Fatalf("nil-schema decode field 9: %+v", got)
	}

	// A critical unknown field must be rejected.
	rec.Fields = []Field{{ID: 9, WireType: fileformat.WireString, Value: "x", Critical: true}}
	enc, err = rec.Encode(nil)
	if err != nil {
		t.Fatalf("Encode critical unknown: %v", err)
	}
	if err := r.Decode(enc, known); err == nil {
		t.Fatal("unknown critical field should be rejected on decode")
	}
}

func TestRecordEncodeValidationErrors(t *testing.T) {
	rec := sampleRecord()
	rec.Namespace = ""
	if _, err := rec.Encode(nil); err == nil {
		t.Fatal("empty namespace should error")
	}
	rec = sampleRecord()
	rec.ObjectID = 0
	if _, err := rec.Encode(nil); err == nil {
		t.Fatal("zero object id should error")
	}
	rec = sampleRecord()
	if _, err := rec.Encode(nil); err != nil {
		t.Fatalf("valid record should encode: %v", err)
	}
}

func TestRecordEncodeTooLarge(t *testing.T) {
	rec := sampleRecord()
	rec.Namespace = strings.Repeat("x", 1<<31)
	if _, err := rec.Encode(nil); err == nil {
		t.Fatal("oversized record should error")
	}
}

func TestRecordDecodeErrors(t *testing.T) {
	var r Record
	if err := r.Decode(nil, nil); err == nil {
		t.Fatal("nil input should error")
	}
	if err := r.Decode(make([]byte, 10), nil); err == nil {
		t.Fatal("truncated record should error")
	}

	rec := sampleRecord()
	enc, err := rec.Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Corrupt length: recordLen too small
	bad := append([]byte(nil), enc...)
	bad[0] = 4
	if err := r.Decode(bad, nil); err == nil {
		t.Fatal("recordLen too small should error")
	}

	// Corrupt length mismatch (larger than input)
	bad = append([]byte(nil), enc...)
	bad[0] = 0xFF
	if err := r.Decode(bad, nil); err == nil {
		t.Fatal("recordLen mismatch should error")
	}

	// Corrupt CRC
	bad = append([]byte(nil), enc...)
	bad[len(bad)-1] ^= 0xFF
	if err := r.Decode(bad, nil); err == nil {
		t.Fatal("CRC mismatch should error")
	}

	// Drop the trailing CRC bytes: recordLen no longer matches the input.
	short := enc[:len(enc)-4]
	if err := r.Decode(short, nil); err == nil {
		t.Fatal("short payload vs recordLen should error")
	}
}

func TestRecordDecodeFieldRegionErrors(t *testing.T) {
	rec := sampleRecord()
	enc, err := rec.Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Inflate field count beyond actual fields (CRC fixed so the check is
	// actually reached instead of failing on CRC first).
	bad := append([]byte(nil), enc...)
	bad[40] = 99
	fixCRC(bad)
	if err := rec.Decode(bad, nil); err == nil {
		t.Fatal("impossible field count should error")
	}

	// Corrupt fieldsLen so it no longer matches the derived value.
	bad = append([]byte(nil), enc...)
	binary.LittleEndian.PutUint32(bad[44:], 3)
	fixCRC(bad)
	if err := rec.Decode(bad, nil); err == nil {
		t.Fatal("fieldsLen mismatch should error")
	}

	// Truncate a field's value bytes while keeping the header counts and CRC
	// consistent: the declared value length must exceed the remaining input.
	rec2 := sampleRecord()
	rec2.Fields = []Field{{ID: 1, WireType: fileformat.WireString, Value: "abcd"}}
	enc2, err := rec2.Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Keep header(48) + field header(8) + 3 of the 4 value bytes, then append
	// a fresh CRC so the record passes envelope validation.
	bad = append([]byte(nil), enc2[:48+8+3]...)
	binary.LittleEndian.PutUint32(bad[0:], uint32(len(bad))+4)
	binary.LittleEndian.PutUint32(bad[44:], uint32(len(bad)-48))
	bad = binary.LittleEndian.AppendUint32(bad, fileformat.CRC32C(bad))
	if err := rec2.Decode(bad, nil); err == nil {
		t.Fatal("truncated field value should error")
	}
}

func TestRecordDecodeKnownFieldRepeated(t *testing.T) {
	// Encode refuses duplicate non-Repeated fields (canonical check), so the
	// duplicate input must be crafted by duplicating one field's bytes and
	// fixing the counts + CRC.
	rec := sampleRecord()
	rec.Fields = []Field{{ID: 1, WireType: fileformat.WireString, Value: "a"}}
	enc, err := rec.Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	nsLen := int(binary.LittleEndian.Uint32(enc[32:]))
	ekLen := int(binary.LittleEndian.Uint32(enc[36:]))
	fieldsStart := RecordEnvelopeHeaderSize + nsLen + ekLen
	recordLen := int(binary.LittleEndian.Uint32(enc))
	fieldsLen := int(binary.LittleEndian.Uint32(enc[44:]))
	firstField := enc[fieldsStart : fieldsStart+fieldsLen]

	out := append([]byte(nil), enc[:fieldsStart]...)
	out = append(out, firstField...)
	out = append(out, firstField...)
	out = append(out, enc[fieldsStart+fieldsLen:recordLen-4]...)
	binary.LittleEndian.PutUint32(out[0:], uint32(len(out))+4)
	binary.LittleEndian.PutUint32(out[40:], 2) // fieldCount
	binary.LittleEndian.PutUint32(out[44:], uint32(fieldsLen*2))
	fixCRC(out)
	if err := rec.Decode(out, schemaFixture()); err == nil {
		t.Fatal("duplicate non-Repeated known field should error")
	}
}

func TestRecordDecodeKnownFieldWireType(t *testing.T) {
	// Known field with a non-canonical wire type must be rejected.
	rec := sampleRecord()
	rec.Fields = []Field{{ID: 1, WireType: fileformat.WireSint, Value: int64(1)}}
	enc, err := rec.Encode(nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if err := rec.Decode(enc, schemaFixture()); err == nil {
		t.Fatal("wrong wire type for known field should error")
	}
}

func TestEnsureSingle(t *testing.T) {
	fields := []Field{
		{ID: 1, WireType: fileformat.WireString, Value: "a"},
		{ID: 1, WireType: fileformat.WireString, Value: "b"},
	}
	if err := ensureSingle(fields, 1); err == nil {
		t.Fatal("duplicate should error")
	}
	if err := ensureSingle(fields, 0); err != nil {
		t.Fatalf("first occurrence should pass: %v", err)
	}
}
