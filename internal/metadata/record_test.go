package metadata

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func headerRecord() *Record {
	return &Record{
		RecordType:  uint32(fileformat.RecordHeader),
		ObjectID:    1,
		Revision:    1,
		Namespace:   fileformat.NamespaceCore,
		ExternalKey: "mysql:production",
		Fields: []Field{
			{ID: HeaderDBType, WireType: fileformat.WireString, Value: "mysql"},
			{ID: HeaderDBName, WireType: fileformat.WireString, Value: "production"},
			{ID: HeaderDBVersion, WireType: fileformat.WireString, Value: "8.4.2"},
			{ID: HeaderCharset, WireType: fileformat.WireString, Value: "utf8mb4"},
			{ID: HeaderSnapshotType, WireType: fileformat.WireSint, Value: int64(1)},
		},
	}
}

func tableRecord() *Record {
	return &Record{
		RecordType:  uint32(fileformat.RecordTable),
		ObjectID:    2,
		Revision:    1,
		Namespace:   fileformat.NamespaceCore,
		ExternalKey: "app.users",
		Fields: []Field{
			{ID: TableTableName, WireType: fileformat.WireString, Value: "users"},
			{ID: TableSchema, WireType: fileformat.WireString, Value: "app"},
			{ID: TableTotalRows, WireType: fileformat.WireSint, Value: int64(100000)},
			{ID: TableBytes, WireType: fileformat.WireSint, Value: int64(8388608)},
		},
	}
}

func TestRecordRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rec    *Record
		schema KnownFieldSchema
	}{
		{"header", headerRecord(), CoreFieldSchemas[uint32(fileformat.RecordHeader)]},
		{"table", tableRecord(), CoreFieldSchemas[uint32(fileformat.RecordTable)]},
		{"unknown-ns", &Record{
			RecordType:  65537,
			ObjectID:    500,
			ParentID:    2,
			Revision:    1,
			Namespace:   "com.mysql",
			ExternalKey: "ext",
			Critical:    false,
			Fields: []Field{
				{ID: 1, WireType: fileformat.WireString, Value: "v1"},
				{ID: 3, WireType: fileformat.WireUint, Value: uint64(300)},
			},
		}, nil},
	} {
		enc, err := tc.rec.Encode(tc.schema)
		if err != nil {
			t.Fatalf("%s: encode: %v", tc.name, err)
		}
		var got Record
		if err := got.Decode(enc, tc.schema); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		enc2, err := got.Encode(tc.schema)
		if err != nil {
			t.Fatalf("%s: re-encode: %v", tc.name, err)
		}
		if !bytes.Equal(enc, enc2) {
			t.Fatalf("%s: round-trip changed bytes", tc.name)
		}
		if got.ObjectID != tc.rec.ObjectID || got.Namespace != tc.rec.Namespace || got.ExternalKey != tc.rec.ExternalKey || got.Revision != tc.rec.Revision {
			t.Fatalf("%s: envelope mismatch: %+v", tc.name, got)
		}
	}
}

func TestRecordFieldSetRoundTrip(t *testing.T) {
	props := []Field{
		{ID: 1, WireType: fileformat.WireString, Value: "collation"},
		{ID: 2, WireType: fileformat.WireString, Value: "utf8mb4_0900_ai_ci"},
	}
	rec := &Record{
		RecordType: uint32(fileformat.RecordHeader),
		ObjectID:   1,
		Revision:   1,
		Namespace:  fileformat.NamespaceCore,
		Fields: []Field{
			{ID: HeaderProperties, WireType: fileformat.WireFieldSet, Value: props},
			{ID: HeaderIncludes, WireType: fileformat.WireStringList, Value: []string{"a", "b", "c"}},
		},
	}
	enc, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	if err != nil {
		t.Fatal(err)
	}
	var got Record
	if err := got.Decode(enc, CoreFieldSchemas[uint32(fileformat.RecordHeader)]); err != nil {
		t.Fatal(err)
	}
	f := got.FieldByID(HeaderProperties)
	sub, ok := f.Value.([]Field)
	if !ok || len(sub) != 2 || sub[1].Value.(string) != "utf8mb4_0900_ai_ci" {
		t.Fatalf("field set not preserved: %#v", f)
	}
	il, _ := got.FieldByID(HeaderIncludes).Value.([]string)
	if len(il) != 3 || il[2] != "c" {
		t.Fatalf("string list not preserved: %#v", il)
	}
}

func TestUnknownNonCriticalPassthrough(t *testing.T) {
	// A known record type carrying an unknown non-critical field (ID 99).
	enc1 := func() []byte {
		rec := headerRecord()
		rec.Fields = append(rec.Fields,
			Field{ID: 99, WireType: fileformat.WireBytes, Value: []byte{0xDE, 0xAD, 0xBE, 0xEF}})
		b, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
		if err != nil {
			t.Fatal(err)
		}
		return b
	}()
	var got Record
	if err := got.Decode(enc1, CoreFieldSchemas[uint32(fileformat.RecordHeader)]); err != nil {
		t.Fatal(err)
	}
	enc2, err := got.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc1, enc2) {
		t.Fatal("unknown non-critical field not preserved losslessly")
	}
}

func TestUnknownCriticalRejected(t *testing.T) {
	rec := headerRecord()
	rec.Fields = append(rec.Fields, Field{ID: 200, WireType: fileformat.WireString, Value: "x", Critical: true})
	enc, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	if err != nil {
		t.Fatal(err)
	}
	var got Record
	if err := got.Decode(enc, CoreFieldSchemas[uint32(fileformat.RecordHeader)]); err == nil {
		t.Fatal("unknown critical field accepted")
	}
}

func TestRecordRejectsBadInput(t *testing.T) {
	rec := headerRecord()
	enc, _ := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	lim := len(enc)

	// Every truncation prefix must fail.
	for n := 0; n < lim; n++ {
		var r Record
		if err := r.Decode(enc[:n], nil); err == nil {
			t.Fatalf("accepted truncated record of %d/%d bytes", n, lim)
		}
	}
	// Trailing bytes must fail.
	if err := (&Record{}).Decode(append(enc, 0), nil); err == nil {
		t.Fatal("accepted trailing byte after record")
	}
	// Corrupt CRC.
	bad := append([]byte(nil), enc...)
	bad[len(bad)-1] ^= 0xFF
	if err := (&Record{}).Decode(bad, nil); err == nil {
		t.Fatal("accepted bad record CRC")
	}
	// Non-canonical field order: swap first two fields' values.
	// Field 1 (DBType) and field 3 (DBName) both 4-byte-ish... craft manually.
	rec2 := headerRecord()
	rec2.Fields = []Field{
		{ID: HeaderDBName, WireType: fileformat.WireString, Value: "x"},
		{ID: HeaderDBType, WireType: fileformat.WireString, Value: "y"},
	}
	enc2, err := rec2.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	if err == nil {
		// Encode should have sorted canonically; decoding must then succeed.
		var r Record
		if err := r.Decode(enc2, CoreFieldSchemas[uint32(fileformat.RecordHeader)]); err != nil {
			t.Fatalf("canonical sort not enforced: %v", err)
		}
	}
}

func TestWireTypeMismatchRejected(t *testing.T) {
	rec := headerRecord()
	rec.Fields = []Field{{ID: HeaderDBType, WireType: fileformat.WireSint, Value: int64(1)}}
	if _, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)]); err == nil {
		t.Fatal("accepted wrong wire type for known field")
	}
}

func TestPayloadBuildParse(t *testing.T) {
	recs := []*Record{headerRecord(), tableRecord()}
	entries := make([]DirectoryEntry, 0, len(recs))
	bodies := make([][]byte, 0, len(recs))
	for _, r := range recs {
		b, err := r.Encode(CoreFieldSchemas[r.RecordType])
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, DirectoryEntry{
			ObjectID: r.ObjectID, Revision: r.Revision, RecordType: r.RecordType,
			Operation: fileformat.OperationUpsert,
		})
		bodies = append(bodies, b)
	}
	payload, err := Build(entries, bodies)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 2 || len(p.Records) != 2 {
		t.Fatalf("parsed %d entries / %d records", len(p.Entries), len(p.Records))
	}
	for i := range p.Entries {
		if !bytes.Equal(p.Records[i], bodies[i]) {
			t.Fatalf("record %d bytes differ", i)
		}
		if p.Entries[i].ObjectID != entries[i].ObjectID {
			t.Fatalf("entry %d object id mismatch", i)
		}
	}
	// DELETE entry handling.
	del := append([]DirectoryEntry(nil), entries...)
	delBodies := append([][]byte(nil), bodies...)
	del = append(del, DirectoryEntry{ObjectID: 99, Revision: 1, RecordType: 3, Operation: fileformat.OperationDelete})
	delBodies = append(delBodies, nil)
	payload2, err := Build(del, delBodies)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Parse(payload2)
	if err != nil {
		t.Fatal(err)
	}
	last := p2.Entries[len(p2.Entries)-1]
	if last.Operation != fileformat.OperationDelete || last.RecordLength != 0 || last.RecordCRC() != 0 {
		t.Fatalf("DELETE entry not normalized: %+v", last)
	}
	if p2.Records[len(p2.Records)-1] != nil {
		t.Fatal("DELETE record body not nil")
	}
}

func TestPayloadRejects(t *testing.T) {
	rec := headerRecord()
	b, _ := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	payload, _ := Build([]DirectoryEntry{{ObjectID: 1, Revision: 1, RecordType: 1, Operation: fileformat.OperationUpsert}}, [][]byte{b})
	// Truncations.
	for n := 0; n < len(payload); n++ {
		if _, err := Parse(payload[:n]); err == nil {
			t.Fatalf("accepted truncated payload of %d/%d", n, len(payload))
		}
	}
	// Corrupt record CRC inside payload.
	bad := append([]byte(nil), payload...)
	bad[len(bad)-1] ^= 0xFF
	if _, err := Parse(bad); err == nil {
		t.Fatal("accepted payload with corrupt record CRC")
	}
	// Corrupt entry CRC area is inside record CRC; also corrupt directory count field.
	bad2 := append([]byte(nil), payload...)
	bad2[16] ^= 0xFF // ItemCount
	if _, err := Parse(bad2); err == nil {
		t.Fatal("accepted payload with corrupt item count")
	}
}
