package format

import (
	"bytes"
	"reflect"
	"testing"
)

// TestFrozenSizes locks every fixed structure size. These values are part of
// the on-disk format contract: changing any of them is a format break that
// must go through versioning, not a routine refactor.
func TestFrozenSizes(t *testing.T) {
	want := map[string]int{
		"DataFileHeaderSize":     DataFileHeaderSize,
		"SnapshotHeaderSize":     SnapshotHeaderSize,
		"SnapshotFooterSize":     SnapshotFooterSize,
		"BlockHeaderSize":        BlockHeaderSize,
		"RowDirectoryEntrySize":  RowDirectoryEntrySize,
		"MetaPayloadHeaderSize":  MetaPayloadHeaderSize,
		"MetaDirectoryEntrySize": MetaDirectoryEntrySize,
		"IndexTxnHeaderSize":     IndexTxnHeaderSize,
		"IndexTxnFooterSize":     IndexTxnFooterSize,
		"IndexChunkHeaderSize":   IndexChunkHeaderSize,
		"IndexChunkDirEntrySize": IndexChunkDirEntrySize,
		"SnapshotIndexEntrySize": SnapshotIndexEntrySize,
		"MetadataIndexEntrySize": MetadataIndexEntrySize,
		"BlockIndexEntrySize":    BlockIndexEntrySize,
		"RowIndexEntrySize":      RowIndexEntrySize,
	}
	sizes := map[string]int{
		"DataFileHeaderSize":     128,
		"SnapshotHeaderSize":     96,
		"SnapshotFooterSize":     144,
		"BlockHeaderSize":        64,
		"RowDirectoryEntrySize":  24,
		"MetaPayloadHeaderSize":  32,
		"MetaDirectoryEntrySize": 32,
		"IndexTxnHeaderSize":     80,
		"IndexTxnFooterSize":     80,
		"IndexChunkHeaderSize":   64,
		"IndexChunkDirEntrySize": 32,
		"SnapshotIndexEntrySize": 72,
		"MetadataIndexEntrySize": 48,
		"BlockIndexEntrySize":    56,
		"RowIndexEntrySize":      40,
	}
	for name, got := range want {
		if sizes[name] != int(got) {
			t.Errorf("%s = %d, frozen value is %d; format break needs versioning", name, got, sizes[name])
		}
	}
}

// TestFrozenMagics locks the ASCII magics. All are exactly 8 bytes and must
// never collide with each other.
func TestFrozenMagics(t *testing.T) {
	magics := []string{
		MagicDataFile, MagicSnapshotHdr, MagicSnapshotFtr, MagicBlockHdr,
		MagicMetaPayload, MagicIndexTxnHdr, MagicIndexTxnFtr,
		MagicIndexChunkHdr,
	}
	seen := map[string]bool{}
	for _, m := range magics {
		if len(m) != 8 {
			t.Errorf("magic %q must be exactly 8 bytes, got %d", m, len(m))
		}
		if seen[m] {
			t.Errorf("duplicate magic %q", m)
		}
		seen[m] = true
	}
}

// TestFrozenEnums locks enum values used on disk. Renumbering any of these
// silently corrupts how old files are read.
func TestFrozenEnums(t *testing.T) {
	checks := []struct {
		name string
		got  uint16
		want uint16
	}{
		{"TypeBool", uint16(TypeBool), 1}, {"TypeInt8", uint16(TypeInt8), 2},
		{"TypeInt16", uint16(TypeInt16), 3}, {"TypeInt32", uint16(TypeInt32), 4},
		{"TypeInt64", uint16(TypeInt64), 5}, {"TypeUint8", uint16(TypeUint8), 6},
		{"TypeUint16", uint16(TypeUint16), 7}, {"TypeUint32", uint16(TypeUint32), 8},
		{"TypeUint64", uint16(TypeUint64), 9}, {"TypeFloat32", uint16(TypeFloat32), 10},
		{"TypeFloat64", uint16(TypeFloat64), 11}, {"TypeString", uint16(TypeString), 12},
		{"TypeBytes", uint16(TypeBytes), 13}, {"TypeDate", uint16(TypeDate), 14},
		{"TypeTime", uint16(TypeTime), 15}, {"TypeDateTime", uint16(TypeDateTime), 16},
		{"TypeDecimal", uint16(TypeDecimal), 17},
		{"RecordTable", uint16(RecordTable), 1}, {"RecordColumn", uint16(RecordColumn), 2},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, frozen value is %d", c.name, c.got, c.want)
		}
	}
	e := []struct {
		name string
		got  uint8
		want uint8
	}{
		{"ChangeInsert", uint8(ChangeInsert), 1}, {"ChangeUpdate", uint8(ChangeUpdate), 2},
		{"ChangeDelete", uint8(ChangeDelete), 3},
		{"OperationUpsert", uint8(OperationUpsert), 1}, {"OperationDelete", uint8(OperationDelete), 2},
		{"BlockKindRows", uint8(BlockKindRows), 1}, {"BlockKindMetadata", uint8(BlockKindMetadata), 2},
		{"CompressionNone", uint8(CompressionNone), 0}, {"CompressionZstd", uint8(CompressionZstd), 1},
		{"SnapshotFull", uint8(SnapshotFull), 1}, {"SnapshotDelta", uint8(SnapshotDelta), 2},
		{"EncNone", uint8(EncNone), 0}, {"EncAES256GCM", uint8(EncAES256GCM), 1},
		{"WireBool", uint8(WireBool), 1}, {"WireUint", uint8(WireUint), 2},
		{"WireSint", uint8(WireSint), 3}, {"WireString", uint8(WireString), 4},
		{"WireBytes", uint8(WireBytes), 5},
	}
	for _, c := range e {
		if c.got != c.want {
			t.Errorf("%s = %d, frozen value is %d", c.name, c.got, c.want)
		}
	}
}

// TestVersionConstants locks the format major. v1 is the single-file line and
// the only openable line.
func TestVersionConstants(t *testing.T) {
	if VersionMajor != 1 || VersionMinor != 0 {
		t.Fatalf("VersionMajor/Minor = %d.%d, frozen at 1.0", VersionMajor, VersionMinor)
	}
	if RequiredFeaturesV1 == 0 {
		t.Fatal("RequiredFeaturesV1 must not be zero")
	}
}

// TestFixedStructureRoundTrip marshals and unmarshals every fixed structure
// with non-trivial field values and requires an exact round trip. This is the
// cross-check underneath the golden files: it catches field-order bugs that
// byte-equality against a recorded file cannot localize.
func TestFixedStructureRoundTrip(t *testing.T) {
	var (
		uuid = [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	)

	// DataFileHeader.
	{
		var h DataFileHeader
		h.FileHeader = FileHeader{
			StoreUUID: uuid, CreatedUnixNano: 1757400000000000000,
			RequiredFeatures: RequiredFeaturesV1, OptionalFeatures: 0,
			DefaultBlockSize: 262144, DefaultCompression: CompressionZstd,
			DefaultRowEncoding: RowEncodingTypedTuple, Flags: 3,
		}
		marshalRoundTrip(t, &h, h.Size())
	}
	// KeyID round trip (encryption header fields).
	{
		var h DataFileHeader
		h.FileHeader = FileHeader{
			StoreUUID: uuid, CreatedUnixNano: 1,
			RequiredFeatures: RequiredFeaturesV1, DefaultBlockSize: 1024,
			DefaultCompression: CompressionZstd, DefaultRowEncoding: RowEncodingTypedTuple,
			EncryptionAlgorithm: EncAES256GCM, NonceScheme: NonceCounterV1,
			KeyID: []byte("k1"),
		}
		marshalRoundTrip(t, &h, h.Size())
	}

	snapshotHdr := SnapshotHeader{
		SnapshotType: SnapshotFull, AllowEmpty: true,
		SnapshotID: 7, ParentSnapshotID: 3, CreatedUnixNano: 42,
		FirstBlockID: 9, WriterNonce: 0xDEADBEEF,
	}
	marshalRoundTrip(t, &snapshotHdr, snapshotHdr.Size())

	footer := SnapshotFooter{
		SnapshotType: SnapshotDelta, SnapshotID: 7, ParentSnapshotID: 3,
		PreviousFooterOffset: 1000, SnapshotStartOffset: 128, BlocksStartOffset: 224,
		BlocksEndOffset: 8192, IndexTxnStartOffset: 8192, IndexTxnEndOffset: 12000,
		SnapshotEndOffset: 12144, FirstBlockID: 9, BlockCount: 12,
		MetadataBlockCount: 2, RowRecordCount: 999, RawBytes: 81920,
		StoredBytes: 81972, BlocksCRC32C: 0x11111111, IndexTxnCRC32C: 0x22222222,
	}
	marshalRoundTrip(t, &footer, footer.Size())

	blockHdr := BlockHeader{
		BlockKind: BlockKindRows, Compression: CompressionZstd, BlockID: 5,
		SnapshotID: 2, TableID: 1, ItemCount: 1000, RawSize: 4096,
		StoredSize: 1024, RawCRC32C: 0xABCDEF01, Encrypted: true, KeyEpoch: 3,
	}
	marshalRoundTrip(t, &blockHdr, blockHdr.Size())

	rowDir := RowDirectoryEntry{RowID: 1001, RecordOffset: 77, RecordLength: 88, ChangeType: ChangeUpdate, SchemaVersion: 2}
	marshalRoundTrip(t, &rowDir, rowDir.Size())

	txnHdr := IndexTxnHeader{
		TxnSequence: 12, SnapshotID: 7, DataSnapshotStart: 128, DataSnapshotEnd: 20000,
		MetadataEntryCount: 3, BlockEntryCount: 10, RowEntryCount: 5000, BodyBytes: 640,
	}
	marshalRoundTrip(t, &txnHdr, txnHdr.Size())

	txnFtr := IndexTxnFooter{
		TxnSequence: 12, SnapshotID: 7, TxnStartOffset: 123, TxnEndOffset: 456,
		DataSnapshotEnd: 20000, BodyCRC32C: 0xBBBBBBBB, DataFooterCRC32C: 0xAAAAAAAA,
	}
	marshalRoundTrip(t, &txnFtr, txnFtr.Size())

	chunkHdr := IndexChunkHeader{
		EntryKind: IndexChunkKindRow, Compression: IndexChunkCompressionZstd,
		Encryption: IndexChunkEncryptionNone, Flags: 0, ChunkSequence: 1,
		EntryCount: 100, FirstEntryOrdinal: 0, RawBytes: 2048, StoredBytes: 900,
		KeyEpoch: 0, PayloadCRC32C: 0x11223344,
	}
	marshalRoundTrip(t, &chunkHdr, chunkHdr.Size())

	snapEntry := SnapshotIndexEntry{
		SnapshotID: 7, ParentSnapshotID: 3, SnapshotType: SnapshotDelta,
		BlockCount: 10, RowRecordCount: 5000, DataStart: 128, DataEnd: 20000, CreatedUnixNano: 99,
	}
	marshalRoundTrip(t, &snapEntry, snapEntry.Size())

	metaEntry := MetadataIndexEntry{
		SnapshotID: 7, ObjectID: 100, Revision: 1, RecordType: uint32(RecordTable),
		BlockID: 4, ItemOrdinal: 2, Operation: OperationUpsert, Critical: true,
	}
	marshalRoundTrip(t, &metaEntry, metaEntry.Size())

	blockEntry := BlockIndexEntry{
		BlockID: 5, SnapshotID: 2, TableID: 1, BlockKind: BlockKindRows,
		Compression: CompressionZstd, DataOffset: 4096, RawSize: 4096,
		StoredSize: 1024, ItemCount: 1000, RawCRC32C: 0xABCDEF01,
	}
	marshalRoundTrip(t, &blockEntry, blockEntry.Size())

	rowEntry := RowIndexEntry{
		SnapshotID: 2, TableID: 1, ChangeType: ChangeInsert, RowID: 1001,
		BlockID: 5, ItemOrdinal: 7,
	}
	marshalRoundTrip(t, &rowEntry, rowEntry.Size())
}

// marshalRoundTrip marshals a fixed structure, unmarshals it into a fresh
// zero value and requires the re-encoded bytes to be identical: the serialized
// form must be a fixed point (CRC fields included). It also asserts that two
// marshals of the same value are byte-identical.
func marshalRoundTrip(t *testing.T, m interface {
	Size() int
	MarshalTo(dst []byte) error
	Unmarshal(src []byte) error
}, size int) {
	t.Helper()
	buf := make([]byte, size)
	if err := m.MarshalTo(buf); err != nil {
		t.Fatalf("%T marshal: %v", m, err)
	}
	again := make([]byte, size)
	if err := m.MarshalTo(again); err != nil {
		t.Fatalf("%T re-marshal: %v", m, err)
	}
	if !bytes.Equal(buf, again) {
		t.Fatalf("%T marshal not deterministic", m)
	}
	// Unmarshal into a fresh zero value, then re-encode and compare.
	cp := reflect.New(reflect.TypeOf(m).Elem()).Interface().(interface {
		Size() int
		MarshalTo(dst []byte) error
		Unmarshal(src []byte) error
	})
	if err := cp.Unmarshal(buf); err != nil {
		t.Fatalf("%T unmarshal: %v", m, err)
	}
	rebuf := make([]byte, size)
	if err := cp.MarshalTo(rebuf); err != nil {
		t.Fatalf("%T re-encode after unmarshal: %v", m, err)
	}
	if !bytes.Equal(buf, rebuf) {
		t.Fatalf("%T round trip changed bytes: %x vs %x", m, buf, rebuf)
	}
}

// TestCRCKnownAnswer locks the CRC-32C implementation against a reference
// value, so accidental table swaps or endianness bugs surface immediately.
func TestCRCKnownAnswer(t *testing.T) {
	// CRC-32C("123456789") = 0xE3069283 (Castagnoli, standard test vector).
	if got := CRC32C([]byte("123456789")); got != 0xE3069283 {
		t.Fatalf("CRC32C(\"123456789\") = %08x, want e3069283", got)
	}
	// Concat == CRC over the concatenation.
	a, b := []byte("1234"), []byte("56789")
	if got := CRC32CConcat(CRC32C(a), b); got != CRC32C([]byte("123456789")) {
		t.Fatalf("CRC32CConcat = %08x, want %08x", got, CRC32C([]byte("123456789")))
	}
}

// TestCRCFieldZeroing verifies the fixed-structure CRC rule: the CRC field is
// covered as zero by the checksum, and verifyCRC does not modify the buffer.
func TestCRCFieldZeroing(t *testing.T) {
	var h BlockHeader
	h.BlockKind = BlockKindRows
	h.BlockID = 1
	var buf [BlockHeaderSize]byte
	if err := h.MarshalTo(buf[:]); err != nil {
		t.Fatal(err)
	}
	stored := binaryUint32(buf[52:])
	before := append([]byte(nil), buf[:]...)
	got, err := verifyCRC(buf[:], 52)
	if err != nil {
		t.Fatal(err)
	}
	if got != stored {
		t.Fatalf("verifyCRC returned %08x, stored %08x", got, stored)
	}
	if !bytes.Equal(buf[:], before) {
		t.Fatal("verifyCRC modified its input buffer")
	}
	// Corrupting a single payload byte must break verification.
	buf[40] ^= 0xFF
	if _, err := verifyCRC(buf[:], 52); err == nil {
		t.Fatal("verifyCRC accepted a corrupted header")
	}
}

func binaryUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// TestVersionGates locks Open-time version handling: unknown required feature
// bits and wrong majors are rejected.
func TestVersionGates(t *testing.T) {
	var h DataFileHeader
	h.FileHeader = FileHeader{RequiredFeatures: RequiredFeaturesV1, DefaultBlockSize: 1024}
	if err := h.CheckVersion(); err != nil {
		t.Fatalf("v1 features must be accepted: %v", err)
	}
	h.RequiredFeatures |= 1 << 20 // unknown required bit
	if err := h.CheckVersion(); err == nil {
		t.Fatal("unknown required feature bit must be rejected")
	}
	// Marshal with the bad header still works (it is a writer-side construct);
	// Unmarshal of a major-3 header must fail with IsVersionError.
	var good DataFileHeader
	good.FileHeader = FileHeader{RequiredFeatures: RequiredFeaturesV1, DefaultBlockSize: 1024}
	var buf [DataFileHeaderSize]byte
	if err := good.MarshalTo(buf[:]); err != nil {
		t.Fatal(err)
	}
	buf[8], buf[9] = 0, 3 // major = 3
	var got DataFileHeader
	if err := got.Unmarshal(buf[:]); err == nil {
		t.Fatal("major 3 must be rejected")
	} else if !IsVersionError(err) {
		t.Fatalf("major rejection must be a version error, got %T: %v", err, err)
	}
}
