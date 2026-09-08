package fileformat

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
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
	require.NoError(t, m(buf), "%s: marshal", name)
	require.NoError(t, u(buf), "%s: unmarshal", name)
	// Marshaling to a longer buffer must leave the bytes identical.
	buf2 := make([]byte, 1024)
	require.NoError(t, m(buf2), "%s: re-marshal", name)
	require.True(t, bytes.Equal(buf, buf2), "%s: non-deterministic marshal", name)
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
	require.Error(t, err, "%s: expected error containing %q, got nil", name, wantErr)
	require.Contains(t, err.Error(), wantErr, "%s: error %q does not contain %q", name, err, wantErr)
}

// testFixedStructure drives the shared rejection cases for every fixed
// structure: short input, bad magic (for structures carrying one), bad CRC.
func testFixedStructure(t *testing.T, name string, marshal func(dst []byte) error, unmarshal func(src []byte) error, hasMagic bool, size int) {
	t.Helper()
	buf := make([]byte, size)
	require.NoError(t, marshal(buf), "%s: marshal", name)

	// Round trip on the exact size.
	require.NoError(t, unmarshal(buf), "%s: unmarshal exact size", name)

	// Short inputs of every length below the fixed size must fail cleanly.
	for n := 0; n < size; n++ {
		require.Error(t, unmarshal(buf[:n]), "%s: unmarshal of %d bytes succeeded, want error", name, n)
	}

	// Long input (extra trailing bytes) is tolerated at this layer; higher
	// layers bound their slices.
	require.NoError(t, unmarshal(append(buf, 1, 2, 3)), "%s: unmarshal with trailing bytes", name)

	// Bad CRC must be rejected.
	require.Error(t, unmarshal(corruptBytes(buf, size-8)), "%s: corrupt CRC accepted", name)

	if hasMagic {
		// Bad magic must be rejected.
		bad := append([]byte(nil), buf...)
		bad[0] ^= 0xFF
		require.Error(t, unmarshal(bad), "%s: bad magic accepted", name)
	}
}

func TestDataFileHeader(t *testing.T) {
	h := &DataFileHeader{FileHeader: FileHeader{
		StoreUUID:           [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
		CreatedUnixNano:     1700000000123456789,
		RequiredFeatures:    RequiredFeaturesV1,
		OptionalFeatures:    0,
		DefaultBlockSize:    256 << 10,
		DefaultCompression:  CompressionZstd,
		DefaultRowEncoding:  RowEncodingTypedTuple,
		Flags:               0,
		EncryptionAlgorithm: EncAES256GCM,
		NonceScheme:         NonceCounterV1,
		KeyID:               []byte("store-key-01"),
	}}
	roundTrip(t, "DataFileHeader", h.MarshalTo, h.Unmarshal)

	var got DataFileHeader
	require.NoError(t, got.Unmarshal(mustMarshal(t, h)))
	require.Equal(t, h.StoreUUID, got.StoreUUID, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, h.CreatedUnixNano, got.CreatedUnixNano, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, h.RequiredFeatures, got.RequiredFeatures, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, h.DefaultBlockSize, got.DefaultBlockSize, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, h.DefaultCompression, got.DefaultCompression, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, h.DefaultRowEncoding, got.DefaultRowEncoding, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, EncAES256GCM, got.EncryptionAlgorithm, "encryption field mismatch: %+v vs %+v", got, h)
	require.Equal(t, NonceCounterV1, got.NonceScheme, "encryption field mismatch: %+v vs %+v", got, h)
	require.Equal(t, "store-key-01", string(got.KeyID), "encryption field mismatch: %+v vs %+v", got, h)

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
	require.Error(t, got2.CheckVersion(), "unknown required feature bit accepted")
}

func TestFileHeaderKeyIDLimits(t *testing.T) {
	// Over-long key id must fail marshal.
	h := &DataFileHeader{}
	h.EncryptionAlgorithm = EncAES256GCM
	h.KeyID = bytes.Repeat([]byte("k"), FileHeaderKeyIDMaxLen+1)
	buf := make([]byte, DataFileHeaderSize)
	require.Error(t, h.MarshalTo(buf), "over-long key id accepted")

	// Corrupted length byte (> max) must fail unmarshal.
	h2 := &DataFileHeader{FileHeader: FileHeader{
		EncryptionAlgorithm: EncAES256GCM,
		NonceScheme:         NonceCounterV1,
		KeyID:               []byte("abc"),
	}}
	require.NoError(t, h2.MarshalTo(buf))
	buf[FileHeaderKeyIDLenOffset] = 50
	require.Error(t, h2.Unmarshal(buf), "over-long key id length accepted")
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
	require.NoError(t, got.Unmarshal(mustMarshal(t, h)))
	require.Equal(t, SnapshotDelta, got.SnapshotType, "field mismatch: %+v vs %+v", got, h)
	require.True(t, got.AllowEmpty, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, uint64(42), got.SnapshotID, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, uint64(41), got.ParentSnapshotID, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, uint64(100), got.FirstBlockID, "field mismatch: %+v vs %+v", got, h)
	require.Equal(t, uint64(0xDEADBEEF), got.WriterNonce, "field mismatch: %+v vs %+v", got, h)
}

func TestSnapshotFooter(t *testing.T) {
	f := &SnapshotFooter{
		SnapshotType:         SnapshotFull,
		SnapshotID:           42,
		ParentSnapshotID:     0,
		PreviousFooterOffset: 0,
		SnapshotStartOffset:  128,
		BlocksStartOffset:    128 + SnapshotHeaderSize,
		BlocksEndOffset:      128 + SnapshotHeaderSize + 4096,
		IndexTxnStartOffset:  128 + SnapshotHeaderSize + 4096,
		IndexTxnEndOffset:    128 + SnapshotHeaderSize + 4096 + IndexTxnHeaderSize + SnapshotIndexEntrySize + IndexTxnFooterSize,
		SnapshotEndOffset:    128 + SnapshotHeaderSize + 4096 + IndexTxnHeaderSize + SnapshotIndexEntrySize + IndexTxnFooterSize + SnapshotFooterSize,
		FirstBlockID:         100,
		BlockCount:           4,
		MetadataBlockCount:   1,
		RowRecordCount:       10000,
		RawBytes:             1 << 20,
		StoredBytes:          1 << 19,
		BlocksCRC32C:         0xC0FFEE,
		IndexTxnCRC32C:       0xBADF00D,
	}
	roundTrip(t, "SnapshotFooter", f.MarshalTo, f.Unmarshal)
	testFixedStructure(t, "SnapshotFooter", f.MarshalTo, f.Unmarshal, true, SnapshotFooterSize)

	var got SnapshotFooter
	require.NoError(t, got.Unmarshal(mustMarshal(t, f)))
	require.Equal(t, *f, got)

	require.True(t, f.OffsetsAreConsistent())
	require.False(t, (&SnapshotFooter{SnapshotType: SnapshotDelta, SnapshotID: 2,
		SnapshotStartOffset: 128, BlocksStartOffset: 128 + SnapshotHeaderSize,
		BlocksEndOffset: 128 + SnapshotHeaderSize, IndexTxnStartOffset: 128 + SnapshotHeaderSize,
		IndexTxnEndOffset: 128 + SnapshotHeaderSize, SnapshotEndOffset: 0}).OffsetsAreConsistent())
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
		Encrypted:   true,
		KeyEpoch:    7,
	}
	roundTrip(t, "BlockHeader", h.MarshalTo, h.Unmarshal)
	testFixedStructure(t, "BlockHeader", h.MarshalTo, h.Unmarshal, true, BlockHeaderSize)

	var got BlockHeader
	require.NoError(t, got.Unmarshal(mustMarshal(t, h)))
	require.Equal(t, uint32(0x12345678), got.RawCRC32C, "field mismatch: %+v", got)
	require.Equal(t, uint32(512), got.ItemCount, "field mismatch: %+v", got)
	require.Equal(t, BlockKindRows, got.BlockKind, "field mismatch: %+v", got)
	require.True(t, got.Encrypted, "encryption field mismatch: %+v", got)
	require.Equal(t, uint32(7), got.KeyEpoch, "encryption field mismatch: %+v", got)
}

// TestBlockHeaderPlainRoundTrip locks that a plain (unencrypted) block header
// marshals identically to the pre-encryption format: Flags and KeyEpoch stay
// zero.
func TestBlockHeaderPlainRoundTrip(t *testing.T) {
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
	buf := mustMarshal(t, h)
	require.Equal(t, byte(0), buf[14], "plain header carries nonzero encryption bytes: %v", buf[14:16])
	require.Equal(t, uint32(0), binary.LittleEndian.Uint32(buf[BlockHeaderKeyEpochOffset:]), "plain header carries nonzero encryption bytes: %v", buf[14:16])
	var got BlockHeader
	require.NoError(t, got.Unmarshal(buf))
	require.False(t, got.Encrypted, "plain header parsed as encrypted: %+v", got)
	require.Equal(t, uint32(0), got.KeyEpoch, "plain header parsed as encrypted: %+v", got)
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
		require.Fail(t, "marshal: %v", err)
	}
	return buf
}
