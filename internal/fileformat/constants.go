// Package fileformat implements the fixed on-disk structures, CRC and boundary
// validation of the RowPack v1 binary format. It is the single source of truth
// for every frozen disk constant, magic, enum value and structure size.
//
// The package must never reflect over Go structs to encode or decode: all
// multi-byte integers are hand-written Little Endian, all top-level structures
// are 8-byte aligned, and no field may depend on the host word size or struct
// layout.
package fileformat

// Format version. Version is frozen as Major=1, Minor=0 for the v1 line.
const (
	VersionMajor = 1
	VersionMinor = 0
)

// Required feature bits. A store's RequiredFeatures is written once at creation
// and never updated; it describes the capabilities the store is allowed to
// write. An unknown required bit must cause Open to fail.
const (
	FeatureTypedTupleV1  = 1 << 0 // v1 row encoding
	FeatureZstd          = 1 << 1 // Zstd blocks allowed
	FeatureMetadataBlock = 1 << 2 // generic metadata blocks
	FeatureDeltaSnapshot = 1 << 3 // DELTA snapshots allowed
)

// RequiredFeaturesV1 is the feature bit set of a v1 store.
const RequiredFeaturesV1 = FeatureTypedTupleV1 | FeatureZstd | FeatureMetadataBlock | FeatureDeltaSnapshot

// ASCII magics. All are exactly 8 bytes.
const (
	MagicDataFile    = "ROWPACKD"
	MagicIndexFile   = "ROWPACKI"
	MagicSnapshotHdr = "RPKSNAPH"
	MagicSnapshotFtr = "RPKSNAPF"
	MagicBlockHdr    = "RPKBLOCK"
	MagicRowsPayload = "RPKROWPL"
	MagicMetaPayload = "RPKMETAP"
	MagicIndexTxnHdr = "RPITXNBH"
	MagicIndexTxnFtr = "RPITXNEF"
)

// Fixed structure sizes, all multiples of 8 (except small entries that are
// multiples of their natural width and are packed inside larger aligned
// regions). These values are frozen by the v1 spec and protected by tests.
const (
	DataFileHeaderSize     = 128
	IndexFileHeaderSize    = 128
	SnapshotHeaderSize     = 96
	SnapshotFooterSize     = 96
	BlockHeaderSize        = 64
	RowsPayloadHeaderSize  = 32
	RowDirectoryEntrySize  = 24
	RowRecordHeaderSize    = 24
	MetaPayloadHeaderSize  = 32
	MetaDirectoryEntrySize = 32
	IndexTxnHeaderSize     = 80
	IndexTxnFooterSize     = 80
	SnapshotIndexEntrySize = 72
	MetadataIndexEntrySize = 48
	BlockIndexEntrySize    = 56
	RowIndexEntrySize      = 40
)

// Reserved field offsets that must stay fixed by the spec.
const (
	DataFileHeaderCRC32COffset  = 120
	IndexFileHeaderCRC32COffset = 120
)

// Alignment used for top-level structures and snapshot end offsets.
const Align = 8

// SnapshotType identifies FULL and DELTA snapshots.
type SnapshotType uint8

const (
	SnapshotFull  SnapshotType = 1
	SnapshotDelta SnapshotType = 2
)

// BlockKind distinguishes Rows and Metadata blocks.
type BlockKind uint8

const (
	BlockKindRows     BlockKind = 1
	BlockKindMetadata BlockKind = 2
)

// Compression is the on-disk block compression identifier.
type Compression uint8

const (
	CompressionNone Compression = 0
	CompressionZstd Compression = 1
)

// ChangeType is the per-record change kind.
type ChangeType uint8

const (
	ChangeInsert ChangeType = 1
	ChangeUpdate ChangeType = 2
	ChangeDelete ChangeType = 3
)

// RowEncoding identifies the row payload encoding. DELETE records carry 0.
type RowEncoding uint8

const (
	RowEncodingNone       RowEncoding = 0
	RowEncodingTypedTuple RowEncoding = 1
)

// Operation is the metadata record operation.
type Operation uint8

const (
	OperationUpsert Operation = 1
	OperationDelete Operation = 2
)

// Metadata flag bits (MetadataDirectoryEntry.Flags / MetadataRecord.Flags).
const (
	FlagCritical = 1 << 0
)

// Field flag bits.
const (
	FieldFlagCritical = 1 << 0
	FieldFlagRepeated = 1 << 1
)

// WireType is the metadata Field TLV value encoding.
type WireType uint8

const (
	WireBool          WireType = 1
	WireUint          WireType = 2
	WireSint          WireType = 3
	WireString        WireType = 4
	WireBytes         WireType = 5
	WireObjectRef     WireType = 6
	WireStringList    WireType = 7
	WireObjectRefList WireType = 8
	WireExpression    WireType = 9
	WireFieldSet      WireType = 10
)

// Metadata record namespace. Records written by the engine (DefineSchema
// schema records) use this namespace.
const NamespaceCore = "rowpack.meta.v1"

// Metadata RecordType identifiers used by the engine, frozen for v1.
type RecordType uint32

const (
	RecordTable  RecordType = 2 // Table schema record
	RecordColumn RecordType = 3 // Column schema record
)

// TypedTuple v1 value type identifiers. These equal the public API Type enum
// and are frozen for the v1 line.
type ValueType uint16

const (
	TypeBool     ValueType = 1
	TypeInt8     ValueType = 2
	TypeInt16    ValueType = 3
	TypeInt32    ValueType = 4
	TypeInt64    ValueType = 5
	TypeUint8    ValueType = 6
	TypeUint16   ValueType = 7
	TypeUint32   ValueType = 8
	TypeUint64   ValueType = 9
	TypeFloat32  ValueType = 10
	TypeFloat64  ValueType = 11
	TypeString   ValueType = 12
	TypeBytes    ValueType = 13
	TypeDate     ValueType = 14
	TypeTime     ValueType = 15
	TypeDateTime ValueType = 16
	TypeDecimal  ValueType = 17
)

// Payload versions for Rows and Metadata payloads.
const (
	RowsPayloadVersion = 1
	MetaPayloadVersion = 1
)

// Default safety limits from the binary format spec. Readable code must
// validate lengths, integer overflow, file bounds and these limits before
// allocating.
const (
	DefaultMaxRowBytes         uint32 = 64 << 20 // 64 MiB single row encoding
	DefaultMaxRawBlockBytes    uint32 = 128 << 20
	DefaultMaxStoredBlockBytes uint32 = 128 << 20
	DefaultMaxColumns          uint32 = 16384
	DefaultMaxValueBytes       uint32 = 64 << 20
	DefaultMaxSnapshotDepth    uint32 = 4096
)

// Default tuning values from the API design.
const (
	DefaultBlockSize      = 256 << 10 // 256 KiB target raw block size
	DefaultCacheBytes     = 64 << 20  // 64 MiB block cache
	DefaultCompressionLvl = 3         // klauspost/compress default level mapping
)
