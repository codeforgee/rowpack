package metadata

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func TestPayloadHeaderRoundtrip(t *testing.T) {
	h := PayloadHeader{ItemCount: 3, DirectoryBytes: 3 * DirectoryEntrySize, RecordsBytes: 128}
	dst := make([]byte, PayloadHeaderSize)
	if err := h.MarshalTo(dst); err != nil {
		t.Fatalf("MarshalTo: %v", err)
	}
	if string(dst[0:8]) != fileformat.MagicMetaPayload {
		t.Fatalf("magic %q, want %q", dst[0:8], fileformat.MagicMetaPayload)
	}
	if binary.LittleEndian.Uint32(dst[8:]) != 1 {
		t.Fatal("version must be 1")
	}
	if binary.LittleEndian.Uint32(dst[12:]) != DirectoryEntrySize {
		t.Fatal("directory entry size mismatch")
	}

	var got PayloadHeader
	if err := got.Unmarshal(dst); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != h {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", got, h)
	}
}

func TestPayloadHeaderUnmarshalErrors(t *testing.T) {
	valid := func() []byte {
		h := PayloadHeader{ItemCount: 1, DirectoryBytes: DirectoryEntrySize, RecordsBytes: 0}
		dst := make([]byte, PayloadHeaderSize)
		if err := h.MarshalTo(dst); err != nil {
			t.Fatal(err)
		}
		return dst
	}

	var h PayloadHeader
	if err := h.Unmarshal(make([]byte, PayloadHeaderSize-1)); err == nil {
		t.Fatal("truncated header should error")
	}
	bad := valid()
	bad[0] = 'X'
	if err := h.Unmarshal(bad); err == nil {
		t.Fatal("bad magic should error")
	}
	bad = valid()
	binary.LittleEndian.PutUint32(bad[8:], 2)
	if err := h.Unmarshal(bad); err == nil {
		t.Fatal("unsupported version should error")
	}
	bad = valid()
	binary.LittleEndian.PutUint32(bad[12:], 16)
	if err := h.Unmarshal(bad); err == nil {
		t.Fatal("bad directory entry size should error")
	}
	bad = valid()
	binary.LittleEndian.PutUint32(bad[20:], 2*DirectoryEntrySize) // != ItemCount * size
	if err := h.Unmarshal(bad); err == nil {
		t.Fatal("directory bytes mismatch should error")
	}
}

func TestDirectoryEntryRoundtrip(t *testing.T) {
	e := DirectoryEntry{
		ObjectID:     0x1122334455667788,
		Revision:     4,
		RecordType:   9,
		RecordOffset: 64,
		RecordLength: 100,
		Operation:    fileformat.OperationUpsert,
		Critical:     true,
	}
	e.SetRecordCRC(0xCAFEBABE)

	dst := make([]byte, DirectoryEntrySize)
	if err := e.MarshalTo(dst); err != nil {
		t.Fatalf("MarshalTo: %v", err)
	}
	if dst[25]&fileformat.FlagCritical == 0 {
		t.Fatal("critical flag not written")
	}

	var got DirectoryEntry
	if err := got.Unmarshal(dst); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ObjectID != e.ObjectID || got.Revision != e.Revision ||
		got.RecordType != e.RecordType || got.RecordOffset != e.RecordOffset ||
		got.RecordLength != e.RecordLength || got.Operation != e.Operation ||
		!got.Critical {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.RecordCRC() != 0xCAFEBABE {
		t.Fatalf("recordCRC = %x, want cafebabe", got.RecordCRC())
	}

	// Marshalled entry is all-zero padding beyond the fixed fields.
	if dst[26] != 0 || dst[27] != 0 {
		t.Fatalf("padding bytes not zeroed: %x", dst[26:28])
	}
}

func TestDirectoryEntryMarshalErrors(t *testing.T) {
	e := DirectoryEntry{ObjectID: 1}
	if err := e.MarshalTo(make([]byte, DirectoryEntrySize-1)); err == nil {
		t.Fatal("short destination should error")
	}
	var got DirectoryEntry
	if err := got.Unmarshal(make([]byte, DirectoryEntrySize-1)); err == nil {
		t.Fatal("truncated entry should error")
	}
}

func TestPayloadBuildParseRoundtrip(t *testing.T) {
	rec1 := []byte("record-one")
	rec2 := []byte("record-two")
	entries := []DirectoryEntry{
		{ObjectID: TableSpaceEnd + 1, Operation: fileformat.OperationUpsert},
		{ObjectID: 7, Operation: fileformat.OperationDelete},
		{ObjectID: TableSpaceEnd + 2, Operation: fileformat.OperationUpsert},
	}
	records := [][]byte{rec1, nil, rec2}

	data, err := Build(entries, records)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(data) != PayloadHeaderSize+3*DirectoryEntrySize+len(rec1)+len(rec2) {
		t.Fatalf("payload length %d", len(data))
	}

	p, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(p.Entries) != 3 || len(p.Records) != 3 {
		t.Fatalf("entry/record counts %d/%d", len(p.Entries), len(p.Records))
	}
	if string(p.Records[0]) != "record-one" {
		t.Fatalf("record 0: %q", p.Records[0])
	}
	if p.Records[1] != nil {
		t.Fatalf("DELETE record should be nil, got %q", p.Records[1])
	}
	if string(p.Records[2]) != "record-two" {
		t.Fatalf("record 2: %q", p.Records[2])
	}
	if p.Entries[0].RecordOffset != 0 || p.Entries[0].RecordLength != uint32(len(rec1)) {
		t.Fatalf("entry 0 offsets: %+v", p.Entries[0])
	}
	if p.Entries[1].RecordOffset != 0 || p.Entries[1].RecordLength != 0 || p.Entries[1].RecordCRC() != 0 {
		t.Fatalf("DELETE entry must not carry record bytes: %+v", p.Entries[1])
	}
	if p.Entries[2].RecordOffset != uint32(len(rec1)) {
		t.Fatalf("entry 2 offset %d, want %d", p.Entries[2].RecordOffset, len(rec1))
	}
}

func TestPayloadParseErrors(t *testing.T) {
	build := func(entries []DirectoryEntry, records [][]byte) []byte {
		t.Helper()
		data, err := Build(entries, records)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	one := []DirectoryEntry{{ObjectID: 1, Operation: fileformat.OperationUpsert}}

	if _, err := Parse(nil); err == nil {
		t.Fatal("nil payload should error")
	}
	if _, err := Parse(make([]byte, PayloadHeaderSize+DirectoryEntrySize+5)); err == nil {
		t.Fatal("records-exceed-payload should error")
	}

	// Directory overruns the payload.
	hdrOnly := make([]byte, PayloadHeaderSize)
	h := PayloadHeader{ItemCount: 1, DirectoryBytes: DirectoryEntrySize, RecordsBytes: 0}
	if err := h.MarshalTo(hdrOnly); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(hdrOnly); err == nil {
		t.Fatal("directory exceeds payload should error")
	}

	// Trailing bytes: length mismatch.
	data := append(build(one, [][]byte{[]byte("ab")}), 0x00)
	if _, err := Parse(data); err == nil {
		t.Fatal("trailing byte should error")
	}

	// DELETE entry carrying record bytes.
	bad := build([]DirectoryEntry{{ObjectID: 1, Operation: fileformat.OperationDelete}},
		[][]byte{[]byte("ab")})
	// Flip the entry's operation byte to Upsert while keeping zero offset/crc,
	// so Parse rejects it as a DELETE with payload... instead flip a DELETE to
	// carry an offset: set RecordOffset on the DELETE entry.
	ent := bad[PayloadHeaderSize : PayloadHeaderSize+DirectoryEntrySize]
	binary.LittleEndian.PutUint32(ent[16:], 1) // RecordOffset != 0
	if _, err := Parse(bad); err == nil {
		t.Fatal("DELETE entry with record bytes should error")
	}

	// Corrupt record body (CRC mismatch).
	data = build(one, [][]byte{[]byte("ab")})
	recStart := PayloadHeaderSize + DirectoryEntrySize
	data[recStart] ^= 0xFF
	if _, err := Parse(data); err == nil {
		t.Fatal("record CRC mismatch should error")
	}

	// Record offset/length out of bounds.
	data = build(one, [][]byte{[]byte("ab")})
	ent = data[PayloadHeaderSize : PayloadHeaderSize+DirectoryEntrySize]
	binary.LittleEndian.PutUint32(ent[20:], 99) // RecordLength
	if _, err := Parse(data); err == nil {
		t.Fatal("record out of bounds should error")
	}
}

func TestPayloadBuildErrors(t *testing.T) {
	if _, err := Build([]DirectoryEntry{{}}, nil); err == nil {
		t.Fatal("entry/record count mismatch should error")
	}
	if _, err := Build(nil, nil); err != nil {
		t.Fatalf("empty payload should build: %v", err)
	}
}

func TestObjectIDAllocatorStableIDs(t *testing.T) {
	a := NewObjectIDAllocator()
	if a.next != TableSpaceEnd {
		t.Fatalf("allocator must start at TableSpaceEnd, got %d", a.next)
	}
	id1 := a.Alloc("rowpack.meta.v1", "col.users.name")
	id2 := a.Alloc("rowpack.meta.v1", "col.users.name")
	if id1 != id2 {
		t.Fatalf("same natural key must map to one ID: %d vs %d", id1, id2)
	}
	id3 := a.Alloc("rowpack.meta.v1", "col.users.age")
	if id3 == id1 || id3 != id1+1 {
		t.Fatalf("sequential allocation broken: %d then %d", id1, id3)
	}
	if id3 < TableSpaceEnd {
		t.Fatalf("non-table IDs must be >= TableSpaceEnd: %d", id3)
	}
	// Namespaces are isolated.
	if a.Alloc("other.ns", "col.users.name") == id1 {
		t.Fatal("different namespaces must not share IDs")
	}
}

func TestObjectIDAllocatorForce(t *testing.T) {
	a := NewObjectIDAllocator()
	a.Force(TableSpaceEnd+10, "existing")
	if got := a.Alloc("", "existing"); got != TableSpaceEnd+10 {
		t.Fatalf("Force should pin the key to its ID, got %d", got)
	}
	// Next fresh allocation skips the forced ID.
	next := a.Alloc("", "fresh")
	if next != TableSpaceEnd+11 {
		t.Fatalf("fresh alloc = %d, want %d", next, TableSpaceEnd+11)
	}
	// Force with a lower ID does not move the counter backwards.
	a.Force(5, "low")
	if got := a.Alloc("", "another"); got != TableSpaceEnd+12 {
		t.Fatalf("counter moved backwards: %d", got)
	}
	// Force does not overwrite an existing binding.
	a.Force(TableSpaceEnd+99, "existing")
	if got := a.Alloc("", "existing"); got != TableSpaceEnd+10 {
		t.Fatalf("Force overwrote existing binding: %d", got)
	}
}

func TestTableIDObjectID(t *testing.T) {
	if got := ObjectID(42); got != 42 {
		t.Fatalf("ObjectID(42) = %d", got)
	}
	id, err := TableID(42)
	if err != nil || id != 42 {
		t.Fatalf("TableID(42) = %d, %v", id, err)
	}
	if _, err := TableID(0); err == nil {
		t.Fatal("zero ObjectID should error")
	}
	if _, err := TableID(TableSpaceEnd); err == nil {
		t.Fatal("ObjectID beyond uint32 should error")
	}
	if _, err := TableID(uint64(1)<<32 - 1); err != nil {
		t.Fatalf("max uint32 should fit: %v", err)
	}
}

func TestNaturalKey(t *testing.T) {
	if naturalKey("ns", "key") != "ns\x00key" {
		t.Fatalf("naturalKey = %q", naturalKey("ns", "key"))
	}
	// The NUL separator prevents ("ns","key") from colliding with ("n","skey").
	if naturalKey("ns", "key") == naturalKey("n", "skey") {
		t.Fatal("natural key collision across namespace boundaries")
	}
	if !strings.HasPrefix(naturalKey("ns", "key"), "ns") {
		t.Fatal("unexpected prefix")
	}
}
