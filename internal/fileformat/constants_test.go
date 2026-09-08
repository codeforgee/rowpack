package fileformat

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFrozenEnums protects the published v1 enum values. These numbers are
// written to disk or exported by the public API and must not change without a
// format version bump.
func TestFrozenEnums(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"VersionMajor", VersionMajor, 2},
		{"VersionMinor", VersionMinor, 0},
		{"SnapshotFull", SnapshotFull, SnapshotType(1)},
		{"SnapshotDelta", SnapshotDelta, SnapshotType(2)},
		{"BlockKindRows", BlockKindRows, BlockKind(1)},
		{"BlockKindMetadata", BlockKindMetadata, BlockKind(2)},
		{"CompressionNone", CompressionNone, Compression(0)},
		{"CompressionZstd", CompressionZstd, Compression(1)},
		{"ChangeInsert", ChangeInsert, ChangeType(1)},
		{"ChangeUpdate", ChangeUpdate, ChangeType(2)},
		{"ChangeDelete", ChangeDelete, ChangeType(3)},
		{"RowEncodingNone", RowEncodingNone, RowEncoding(0)},
		{"RowEncodingTypedTuple", RowEncodingTypedTuple, RowEncoding(1)},
		{"OperationUpsert", OperationUpsert, Operation(1)},
		{"OperationDelete", OperationDelete, Operation(2)},
		{"WireBool", WireBool, WireType(1)},
		{"WireUint", WireUint, WireType(2)},
		{"WireSint", WireSint, WireType(3)},
		{"WireString", WireString, WireType(4)},
		{"WireBytes", WireBytes, WireType(5)},
		{"WireObjectRef", WireObjectRef, WireType(6)},
		{"WireStringList", WireStringList, WireType(7)},
		{"WireObjectRefList", WireObjectRefList, WireType(8)},
		{"WireExpression", WireExpression, WireType(9)},
		{"WireFieldSet", WireFieldSet, WireType(10)},
		{"TypeBool", TypeBool, ValueType(1)},
		{"TypeInt8", TypeInt8, ValueType(2)},
		{"TypeInt16", TypeInt16, ValueType(3)},
		{"TypeInt32", TypeInt32, ValueType(4)},
		{"TypeInt64", TypeInt64, ValueType(5)},
		{"TypeUint8", TypeUint8, ValueType(6)},
		{"TypeUint16", TypeUint16, ValueType(7)},
		{"TypeUint32", TypeUint32, ValueType(8)},
		{"TypeUint64", TypeUint64, ValueType(9)},
		{"TypeFloat32", TypeFloat32, ValueType(10)},
		{"TypeFloat64", TypeFloat64, ValueType(11)},
		{"TypeString", TypeString, ValueType(12)},
		{"TypeBytes", TypeBytes, ValueType(13)},
		{"TypeDate", TypeDate, ValueType(14)},
		{"TypeTime", TypeTime, ValueType(15)},
		{"TypeDateTime", TypeDateTime, ValueType(16)},
		{"TypeDecimal", TypeDecimal, ValueType(17)},
		{"RecordTable", RecordTable, RecordType(2)},
		{"RecordColumn", RecordColumn, RecordType(3)},
		{"FlagCritical", FlagCritical, 1},
		{"FieldFlagCritical", FieldFlagCritical, 1},
		{"FieldFlagRepeated", FieldFlagRepeated, 2},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.got, "%s = %v, want %v", tt.name, tt.got, tt.want)
	}
}

// TestFrozenMagicsAndSizes protects the fixed structure layout constants. A
// magic must be exactly 8 ASCII bytes and every structure size must match the
// v1 spec tables.
func TestFrozenMagicsAndSizes(t *testing.T) {
	for _, m := range []string{
		MagicDataFile, MagicSnapshotHdr, MagicSnapshotFtr,
		MagicBlockHdr, MagicRowsPayload, MagicMetaPayload,
		MagicIndexTxnHdr, MagicIndexTxnFtr,
	} {
		assert.Equal(t, 8, len(m), "magic %q has length %d, want 8", m, len(m))
	}

	sizes := []struct {
		name string
		got  int
		want int
	}{
		{"DataFileHeaderSize", DataFileHeaderSize, 128},
		{"SnapshotHeaderSize", SnapshotHeaderSize, 96},
		{"SnapshotFooterSize", SnapshotFooterSize, 144},
		{"BlockHeaderSize", BlockHeaderSize, 64},
		{"RowsPayloadHeaderSize", RowsPayloadHeaderSize, 32},
		{"RowDirectoryEntrySize", RowDirectoryEntrySize, 24},
		{"RowRecordHeaderSize", RowRecordHeaderSize, 24},
		{"MetaPayloadHeaderSize", MetaPayloadHeaderSize, 32},
		{"MetaDirectoryEntrySize", MetaDirectoryEntrySize, 32},
		{"IndexTxnHeaderSize", IndexTxnHeaderSize, 80},
		{"IndexTxnFooterSize", IndexTxnFooterSize, 80},
		{"SnapshotIndexEntrySize", SnapshotIndexEntrySize, 72},
		{"MetadataIndexEntrySize", MetadataIndexEntrySize, 48},
		{"BlockIndexEntrySize", BlockIndexEntrySize, 56},
		{"RowIndexEntrySize", RowIndexEntrySize, 40},
	}
	for _, s := range sizes {
		assert.Equal(t, s.want, s.got, "%s = %d, want %d", s.name, s.got, s.want)
		assert.Zero(t, s.got%8, "%s = %d is not 8-byte aligned", s.name, s.got)
	}

	assert.Less(t, DataFileHeaderCRC32COffset+8, DataFileHeaderSize+1, "DataFileHeader CRC field at %d plus trailing reserved does not fit in %d", DataFileHeaderCRC32COffset, DataFileHeaderSize)
}

// TestFeatureBits protects the feature bit numbering.
func TestFeatureBits(t *testing.T) {
	require.Equal(t, 1, int(FeatureTypedTupleV1))
	require.Equal(t, 2, int(FeatureZstd))
	require.Equal(t, 4, int(FeatureMetadataBlock))
	require.Equal(t, 8, int(FeatureDeltaSnapshot))
	require.Zero(t, RequiredFeaturesV1&^uint64(0xF), "RequiredFeaturesV1 contains unknown bits: %b", RequiredFeaturesV1)
}

// TestRecordTypeValues protects the engine's schema record type numbers.
func TestRecordTypeValues(t *testing.T) {
	assert.Equal(t, RecordType(2), RecordTable, "schema record types changed: Table=%d Column=%d", RecordTable, RecordColumn)
	assert.Equal(t, RecordType(3), RecordColumn, "schema record types changed: Table=%d Column=%d", RecordTable, RecordColumn)
}
