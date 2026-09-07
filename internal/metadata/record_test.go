package metadata

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

func tableRecord() *Record {
	return &Record{
		RecordType:  uint32(fileformat.RecordTable),
		ObjectID:    2,
		Revision:    1,
		Namespace:   fileformat.NamespaceCore,
		ExternalKey: "users",
		Fields: []Field{
			{ID: TableTableName, WireType: fileformat.WireString, Value: "users"},
		},
	}
}

func columnRecord() *Record {
	return &Record{
		RecordType:  uint32(fileformat.RecordColumn),
		ObjectID:    3,
		ParentID:    2,
		Revision:    1,
		Namespace:   fileformat.NamespaceCore,
		ExternalKey: "users:1:id",
		Fields: []Field{
			{ID: ColColumnID, WireType: fileformat.WireSint, Value: int64(1)},
			{ID: ColColumnName, WireType: fileformat.WireString, Value: "id"},
			{ID: ColColumnType, WireType: fileformat.WireString, Value: "uint64"},
			{ID: ColNullable, WireType: fileformat.WireString, Value: "NO"},
			{ID: ColDataScale, WireType: fileformat.WireSint, Value: int64(0)},
		},
	}
}

func TestRecordRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rec    *Record
		schema KnownFieldSchema
	}{
		{"table", tableRecord(), CoreFieldSchemas[uint32(fileformat.RecordTable)]},
		{"column", columnRecord(), CoreFieldSchemas[uint32(fileformat.RecordColumn)]},
		{"unknown-ns", &Record{
			RecordType:  65537,
			ObjectID:    500,
			ParentID:    2,
			Revision:    1,
			Namespace:   "example.com.ext",
			ExternalKey: "ext",
			Critical:    false,
			Fields: []Field{
				{ID: 1, WireType: fileformat.WireString, Value: "v1"},
				{ID: 3, WireType: fileformat.WireString, Value: "v3"},
			},
		}, nil},
	} {
		enc, err := tc.rec.Encode(tc.schema)
		require.NoError(t, err, "%s: encode", tc.name)
		var got Record
		require.NoError(t, got.Decode(enc, tc.schema), "%s: decode", tc.name)
		enc2, err := got.Encode(tc.schema)
		require.NoError(t, err, "%s: re-encode", tc.name)
		require.True(t, bytes.Equal(enc, enc2), "%s: round-trip changed bytes", tc.name)
		require.Equal(t, tc.rec.ObjectID, got.ObjectID, "%s: envelope mismatch: %+v", tc.name, got)
		require.Equal(t, tc.rec.Namespace, got.Namespace, "%s: envelope mismatch: %+v", tc.name, got)
		require.Equal(t, tc.rec.ExternalKey, got.ExternalKey, "%s: envelope mismatch: %+v", tc.name, got)
		require.Equal(t, tc.rec.Revision, got.Revision, "%s: envelope mismatch: %+v", tc.name, got)
	}
}

func TestUnknownNonCriticalPassthrough(t *testing.T) {
	// A known record type carrying an unknown non-critical field (ID 99) with
	// a reserved wire type (1): the exact bytes must survive a decode/re-encode
	// round trip without the engine interpreting the value.
	enc1 := func() []byte {
		rec := tableRecord()
		rec.Fields = append(rec.Fields, Field{ID: 99, WireType: 1, raw: []byte{0x2A}})
		b, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
		require.NoError(t, err)
		return b
	}()
	var got Record
	require.NoError(t, got.Decode(enc1, CoreFieldSchemas[uint32(fileformat.RecordTable)]))
	f := got.FieldByID(99)
	require.NotNil(t, f, "reserved wire type field not passed through: %+v", f)
	require.Nil(t, f.Value, "reserved wire type field not passed through: %+v", f)
	require.Equal(t, "\x2a", string(f.raw), "reserved wire type field not passed through: %+v", f)
	enc2, err := got.Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	require.NoError(t, err)
	require.True(t, bytes.Equal(enc1, enc2), "unknown non-critical field not preserved losslessly")
}

func TestUnknownCriticalRejected(t *testing.T) {
	rec := tableRecord()
	rec.Fields = append(rec.Fields, Field{ID: 200, WireType: fileformat.WireString, Value: "x", Critical: true})
	enc, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	require.NoError(t, err)
	var got Record
	require.Error(t, got.Decode(enc, CoreFieldSchemas[uint32(fileformat.RecordTable)]), "unknown critical field accepted")
}

func TestRecordRejectsBadInput(t *testing.T) {
	rec := tableRecord()
	enc, _ := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	lim := len(enc)

	// Every truncation prefix must fail.
	for n := 0; n < lim; n++ {
		var r Record
		require.Error(t, r.Decode(enc[:n], nil), "accepted truncated record of %d/%d bytes", n, lim)
	}
	// Trailing bytes must fail.
	require.Error(t, (&Record{}).Decode(append(enc, 0), nil), "accepted trailing byte after record")
	// Corrupt CRC.
	bad := append([]byte(nil), enc...)
	bad[len(bad)-1] ^= 0xFF
	require.Error(t, (&Record{}).Decode(bad, nil), "accepted bad record CRC")
	// Non-canonical field order: encode sorts canonically, so feed reversed
	// order and expect decode to still succeed after canonical re-encode.
	rec2 := columnRecord()
	rec2.Fields = []Field{
		{ID: ColColumnName, WireType: fileformat.WireString, Value: "x"},
		{ID: ColColumnID, WireType: fileformat.WireSint, Value: int64(1)},
	}
	enc2, err := rec2.Encode(CoreFieldSchemas[uint32(fileformat.RecordColumn)])
	if err == nil {
		// Encode should have sorted canonically; decoding must then succeed.
		var r Record
		require.NoError(t, r.Decode(enc2, CoreFieldSchemas[uint32(fileformat.RecordColumn)]), "canonical sort not enforced: %v", err)
	}
}

func TestWireTypeMismatchRejected(t *testing.T) {
	rec := tableRecord()
	rec.Fields = []Field{{ID: TableTableName, WireType: fileformat.WireSint, Value: int64(1)}}
	_, err := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	require.Error(t, err, "accepted wrong wire type for known field")
}

func TestPayloadBuildParse(t *testing.T) {
	recs := []*Record{tableRecord(), columnRecord()}
	entries := make([]DirectoryEntry, 0, len(recs))
	bodies := make([][]byte, 0, len(recs))
	for _, r := range recs {
		b, err := r.Encode(CoreFieldSchemas[r.RecordType])
		require.NoError(t, err)
		entries = append(entries, DirectoryEntry{
			ObjectID: r.ObjectID, Revision: r.Revision, RecordType: r.RecordType,
			Operation: fileformat.OperationUpsert,
		})
		bodies = append(bodies, b)
	}
	payload, err := Build(entries, bodies)
	require.NoError(t, err)
	p, err := Parse(payload)
	require.NoError(t, err)
	require.Len(t, p.Entries, 2, "parsed %d entries / %d records", len(p.Entries), len(p.Records))
	require.Len(t, p.Records, 2, "parsed %d entries / %d records", len(p.Entries), len(p.Records))
	for i := range p.Entries {
		require.True(t, bytes.Equal(p.Records[i], bodies[i]), "record %d bytes differ", i)
		require.Equal(t, entries[i].ObjectID, p.Entries[i].ObjectID, "entry %d object id mismatch", i)
	}
	// DELETE entry handling.
	del := append([]DirectoryEntry(nil), entries...)
	delBodies := append([][]byte(nil), bodies...)
	del = append(del, DirectoryEntry{ObjectID: 99, Revision: 1, RecordType: 3, Operation: fileformat.OperationDelete})
	delBodies = append(delBodies, nil)
	payload2, err := Build(del, delBodies)
	require.NoError(t, err)
	p2, err := Parse(payload2)
	require.NoError(t, err)
	last := p2.Entries[len(p2.Entries)-1]
	require.Equal(t, fileformat.OperationDelete, last.Operation, "DELETE entry not normalized: %+v", last)
	require.Zero(t, last.RecordLength, "DELETE entry not normalized: %+v", last)
	require.Zero(t, last.RecordCRC(), "DELETE entry not normalized: %+v", last)
	require.Nil(t, p2.Records[len(p2.Records)-1], "DELETE record body not nil")
}

func TestPayloadRejects(t *testing.T) {
	rec := tableRecord()
	b, _ := rec.Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	payload, _ := Build([]DirectoryEntry{{ObjectID: 2, Revision: 1, RecordType: 2, Operation: fileformat.OperationUpsert}}, [][]byte{b})
	// Truncations.
	for n := 0; n < len(payload); n++ {
		_, err := Parse(payload[:n])
		require.Error(t, err, "accepted truncated payload of %d/%d", n, len(payload))
	}
	// Corrupt record CRC inside payload.
	bad := append([]byte(nil), payload...)
	bad[len(bad)-1] ^= 0xFF
	_, err := Parse(bad)
	require.Error(t, err, "accepted payload with corrupt record CRC")
	// Corrupt entry CRC area is inside record CRC; also corrupt directory count field.
	bad2 := append([]byte(nil), payload...)
	bad2[16] ^= 0xFF // ItemCount
	_, err = Parse(bad2)
	require.Error(t, err, "accepted payload with corrupt item count")
}
