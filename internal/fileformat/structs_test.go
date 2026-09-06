package fileformat

import (
	"bytes"
	"strings"
	"testing"
)

// decodeFunc unmarshals src; encodeFunc marshals v into a fresh buffer.
type roundTripCase struct {
	name      string
	marshal   func(dst []byte) error
	unmarshal func(src []byte) error
}

// roundTrip verifies marshal -> unmarshal succeeds and that the CRC field
// makes the serialized form self-consistent.
func roundTrip(t *testing.T, name string, m func(dst []byte) error, u func(src []byte) error) {
	t.Helper()
	buf := make([]byte, 1024)
	if err := m(buf); err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	if err := u(buf); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	// Marshaling to a longer buffer must leave the bytes identical.
	buf2 := make([]byte, 1024)
	if err := m(buf2); err != nil {
		t.Fatalf("%s: re-marshal: %v", name, err)
	}
	if !bytes.Equal(buf, buf2) {
		t.Fatalf("%s: non-deterministic marshal", name)
	}
}

// corruptBytes flips one byte in buf at offset off (or at len/2 when off<0).
func corruptBytes(buf []byte, off int) []byte {
	if off < 0 {
		off = len(buf) / 2
	}
	cp := append([]byte(nil), buf...)
	cp[off] ^= 0xFF
	return cp
}

func testRejects(t *testing.T, name string, u func(src []byte) error, src []byte, wantErr string) {
	t.Helper()
	err := u(src)
	if err == nil {
		t.Fatalf("%s: expected error containing %q, got nil", name, wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("%s: error %q does not contain %q", name, err, wantErr)
	}
}

// testFixedStructure drives the shared rejection cases for every fixed
// structure: short input, bad magic (for structures carrying one), bad CRC.
func testFixedStructure(t *testing.T, name string, marshal func(dst []byte) error, unmarshal func(src []byte) error, hasMagic bool, size int) {
	t.Helper()
	buf := make([]byte, size)
	if err := marshal(buf); err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}

	// Round trip on the exact size.
	if err := unmarshal(buf); err != nil {
		t.Fatalf("%s: unmarshal exact size: %v", name, err)
	}

	// Short inputs of every length below the fixed size must fail cleanly.
	for n := 0; n < size; n++ {
		if err := unmarshal(buf[:n]); err == nil {
			t.Fatalf("%s: unmarshal of %d bytes succeeded, want error", name, n)
		}
	}

	// Long input (extra trailing bytes) is tolerated at this layer; higher
	// layers bound their slices.
	if err := unmarshal(append(buf, 1, 2, 3)); err != nil {
		t.Fatalf("%s: unmarshal with trailing bytes: %v", name, err)
	}

	// Bad CRC must be rejected.
	if err := unmarshal(corruptBytes(buf, size-8)); err == nil {
		t.Fatalf("%s: corrupt CRC accepted", name)
	}

	if hasMagic {
		// Bad magic must be rejected.
		bad := append([]byte(nil), buf...)
		bad[0] ^= 0xFF
		if err := unmarshal(bad); err == nil {
			t.Fatalf("%s: bad magic accepted", name)
		}
	}
}

func TestDataFileHeader(t *testing.T) {
	h := &DataFileHeader{FileHeader: FileHeader{
		StoreUUID:          [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		CreatedUnixNano:    1700000000123456789,
		RequiredFeatures:   RequiredFeaturesV1,
		OptionalFeatures:   0,
		DefaultBlockSize:   256 << 10,
		DefaultCompression: CompressionZstd,
		DefaultRowEncoding: RowEncodingTypedTuple,
		Flags:              0,
	}}
	roundTrip(t, "DataFileHeader", h.MarshalTo, h.Unmarshal)

	var got DataFileHeader
	if err := got.Unmarshal(mustMarshal(t, h)); err != nil {
		t.Fatal(err)
	}
	if got.StoreUUID != h.StoreUUID || got.CreatedUnixNano != h.CreatedUnixNano ||
		got.RequiredFeatures != h.RequiredFeatures || got.DefaultBlockSize != h.DefaultBlockSize ||
		got.DefaultCompression != h.DefaultCompression || got.DefaultRowEncoding != h.DefaultRowEncoding {
		t.Fatalf("field mismatch: %+v vs %+v", got, h)
	}

	testFixedStructure(t, "DataFileHeader", h.MarshalTo, h.Unmarshal, true, DataFileHeaderSize)

	// Unknown major version must be rejected.
	buf := mustMarshal(t, h)
	putU16(buf[8:], 99)
	testRejects(t, "DataFileHeader major", h.Unmarshal, buf, "unsupported version")

	// Bad size field.
	buf = mustMarshal(t, h)
	putU32(buf[12:], 64)
	testRejects(t, "DataFileHeader size", h.Unmarshal, buf, "bad size")

	// Feature bit check.
	got2 := h
	got2.RequiredFeatures = 1 << 60
	if err := got2.CheckVersion(); err == nil {
		t.Fatal("unknown required feature bit accepted")
	}
}

func TestIndexFileHeader(t *testing.T) {
	h := &IndexFileHeader{FileHeader: FileHeader{
		StoreUUID:          [16]byte{16, 15, 14, 13, 12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1},
		CreatedUnixNano:    1600000000000000000,
		RequiredFeatures:   RequiredFeaturesV1,
		DefaultBlockSize:   256 << 10,
		DefaultCompression: CompressionZstd,
		DefaultRowEncoding: RowEncodingTypedTuple,
	}}
	roundTrip(t, "IndexFileHeader", h.MarshalTo, h.Unmarshal)
	testFixedStructure(t, "IndexFileHeader", h.MarshalTo, h.Unmarshal, true, IndexFileHeaderSize)
}

func TestSnapshotHeader(t *testing.T) {
	h := &SnapshotHeader{
		SnapshotType:     SnapshotDelta,
		AllowEmpty:       true,
		SnapshotID:       42,
		ParentSnapshotID: 41,
		CreatedUnixNano:  1700000000000000001,
		FirstBlockID:     100,
		WriterNonce:      0xDEADBEEF,
	}
	roundTrip(t, "SnapshotHeader", h.MarshalTo, h.Unmarshal)
	testFixedStructure(t, "SnapshotHeader", h.MarshalTo, h.Unmarshal, true, SnapshotHeaderSize)

	var got SnapshotHeader
	if err := got.Unmarshal(mustMarshal(t, h)); err != nil {
		t.Fatal(err)
	}
	if got.SnapshotType != h.SnapshotType || !got.AllowEmpty || got.SnapshotID != 42 ||
		got.ParentSnapshotID != 41 || got.FirstBlockID != 100 || got.WriterNonce != 0xDEADBEEF {
		t.Fatalf("field mismatch: %+v vs %+v", got, h)
	}
}

func TestSnapshotFooter(t *testing.T) {
	f := &SnapshotFooter{
		SnapshotType:        SnapshotFull,
		SnapshotID:          42,
		ParentSnapshotID:    0,
		SnapshotStartOffset: 128,
		SnapshotEndOffset:   128 + 96 + 4096 + 96,
		FirstBlockID:        100,
		BlockCount:          4,
		MetadataBlockCount:  1,
		RowRecordCount:      10000,
		RawBytes:            1 << 20,
		BlocksCRC32C:        0xC0FFEE,
	}
	roundTrip(t, "SnapshotFooter", f.MarshalTo, f.Unmarshal)
	testFixedStructure(t, "SnapshotFooter", f.MarshalTo, f.Unmarshal, true, SnapshotFooterSize)
}

func TestBlockHeader(t *testing.T) {
	h := &BlockHeader{
		BlockKind:   BlockKindRows,
		Compression: CompressionZstd,
		BlockID:     7,
		SnapshotID:  42,
		TableID:     3,
		ItemCount:   512,
		RawSize:     260000,
		StoredSize:  100000,
		RawCRC32C:   0x12345678,
	}
	roundTrip(t, "BlockHeader", h.MarshalTo, h.Unmarshal)
	testFixedStructure(t, "BlockHeader", h.MarshalTo, h.Unmarshal, true, BlockHeaderSize)

	var got BlockHeader
	if err := got.Unmarshal(mustMarshal(t, h)); err != nil {
		t.Fatal(err)
	}
	if got.RawCRC32C != 0x12345678 || got.ItemCount != 512 || got.BlockKind != BlockKindRows {
		t.Fatalf("field mismatch: %+v", got)
	}
}

func TestIndexTxnHeader(t *testing.T) {
	h := &IndexTxnHeader{
		TxnSequence:        1,
		SnapshotID:         42,
		DataSnapshotStart:  128,
		DataSnapshotEnd:    128 + 96 + 4096 + 96,
		MetadataEntryCount: 3,
		BlockEntryCount:    4,
		RowEntryCount:      1000,
		BodyBytes:          3*48 + 4*56 + 1000*40,
	}
	roundTrip(t, "IndexTxnHeader", h.MarshalTo, h.Unmarshal)
	testFixedStructure(t, "IndexTxnHeader", h.MarshalTo, h.Unmarshal, true, IndexTxnHeaderSize)
}

func TestIndexEntries(t *testing.T) {
	tests := []struct {
		name      string
		marshal   func(dst []byte) error
		unmarshal func(src []byte) error
		size      int
	}{
		{"SnapshotIndexEntry", (&SnapshotIndexEntry{
			SnapshotID: 42, ParentSnapshotID: 41, SnapshotType: SnapshotDelta,
			BlockCount: 4, RowRecordCount: 1000, DataStart: 128, DataEnd: 4096,
			CreatedUnixNano: 1700000000000000000, DataFooterCRC32C: 0xABCD,
		}).MarshalTo, (&SnapshotIndexEntry{}).Unmarshal, SnapshotIndexEntrySize},

		{"MetadataIndexEntry", (&MetadataIndexEntry{
			SnapshotID: 42, ObjectID: 1001, Revision: 2, RecordType: 3,
			BlockID: 7, ItemOrdinal: 5, Operation: OperationUpsert, Critical: true,
		}).MarshalTo, (&MetadataIndexEntry{}).Unmarshal, MetadataIndexEntrySize},

		{"BlockIndexEntry", (&BlockIndexEntry{
			BlockID: 7, SnapshotID: 42, TableID: 3, BlockKind: BlockKindRows,
			Compression: CompressionZstd, DataOffset: 4096, RawSize: 260000,
			StoredSize: 100000, ItemCount: 512, RawCRC32C: 0xDEAD,
		}).MarshalTo, (&BlockIndexEntry{}).Unmarshal, BlockIndexEntrySize},

		{"RowIndexEntry", (&RowIndexEntry{
			SnapshotID: 42, TableID: 3, ChangeType: ChangeInsert,
			RowID: 1001, BlockID: 7, ItemOrdinal: 3,
		}).MarshalTo, (&RowIndexEntry{}).Unmarshal, RowIndexEntrySize},
	}

	for _, tt := range tests {
		testFixedStructure(t, tt.name, tt.marshal, tt.unmarshal, false, tt.size)
	}
}

func TestIndexTxnFooter(t *testing.T) {
	f := &IndexTxnFooter{
		TxnSequence:      1,
		SnapshotID:       42,
		TxnStartOffset:   128,
		TxnEndOffset:     128 + 80 + 1000 + 80,
		DataSnapshotEnd:  4096,
		BodyCRC32C:       0x1111,
		DataFooterCRC32C: 0x2222,
	}
	roundTrip(t, "IndexTxnFooter", f.MarshalTo, f.Unmarshal)
	testFixedStructure(t, "IndexTxnFooter", f.MarshalTo, f.Unmarshal, true, IndexTxnFooterSize)
}

// mustMarshal marshals h into a fresh Size()-sized buffer.
func mustMarshal(t *testing.T, m interface{ MarshalTo([]byte) error }) []byte {
	t.Helper()
	buf := make([]byte, 1024)
	if err := m.MarshalTo(buf); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return buf
}
