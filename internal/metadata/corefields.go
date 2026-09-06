package metadata

import "github.com/rowpack/rowpack/internal/fileformat"

// Core field IDs and wire types for the 13 core record types
// (METADATA_FORMAT_V1.md §7). FieldID numbering follows the existing msgpack
// tag order and is frozen for v1.

const (
	// Shared numeric field IDs (String wire type unless noted).
	FTableName  uint16 = 1
	FColumnName uint16 = 2
	FSchema     uint16 = 5
	FConsName   uint16 = 1
)

// Header field IDs (RecordType 1).
const (
	HeaderDBType        uint16 = 1
	HeaderUserID        uint16 = 2
	HeaderDBName        uint16 = 3
	HeaderIPAddr        uint16 = 4
	HeaderPort          uint16 = 5
	HeaderDBVersion     uint16 = 6
	HeaderCharset       uint16 = 7
	HeaderCollation     uint16 = 8
	HeaderVersion       uint16 = 9
	HeaderProperties    uint16 = 10 // FieldSet
	HeaderExcludes      uint16 = 11 // StringList
	HeaderIncludes      uint16 = 12 // StringList
	HeaderSnapshotType  uint16 = 13 // Sint
	HeaderSnapshotToken uint16 = 14
	HeaderSnapshotAt    uint16 = 15 // Sint
)

// Table field IDs (RecordType 2).
const (
	TableTableName uint16 = 1
	TableTotalRows uint16 = 2 // Sint
	TableBatchSize uint16 = 3 // Sint
	TableBytes     uint16 = 4 // Sint
	TableSchema    uint16 = 5
	TableProps     uint16 = 6 // FieldSet
)

// Column / VirtualColumn field IDs (RecordType 3 / 13).
const (
	ColTableName   uint16 = 1
	ColColumnName  uint16 = 2
	ColDataType    uint16 = 3
	ColDataLength  uint16 = 4  // Sint
	ColCharLength  uint16 = 5  // Sint
	ColDataPrec    uint16 = 6  // Sint
	ColDataScale   uint16 = 7  // Sint
	ColNullable    uint16 = 8  // 原始字符串
	ColDataDefault uint16 = 9  // 原始字符串
	ColColumnID    uint16 = 10 // Sint
	ColCharUsed    uint16 = 11
	ColSchema      uint16 = 12
	ColColumnType  uint16 = 13
)

// PrimaryKey field IDs (RecordType 4).
const (
	PKConsName   uint16 = 1
	PKTableName  uint16 = 2
	PKColumnName uint16 = 3
	PKKeySeq     uint16 = 4 // Sint
	PKSchema     uint16 = 5
)

// Index field IDs (RecordType 5).
const (
	IdxIndexName uint16 = 1
	IdxTableName uint16 = 2
	IdxComment   uint16 = 3 // 原始字符串
	IdxColumns   uint16 = 4
	IdxSchema    uint16 = 5
)

// UniqueKey field IDs (RecordType 6).
const (
	UKTableName uint16 = 1
	UKConsName  uint16 = 2
	UKColumns   uint16 = 3 // 原始字符串
	UKSchema    uint16 = 4
)

// ForeignKey field IDs (RecordType 7).
const (
	FKTableName     uint16 = 1
	FKConsName      uint16 = 2
	FKColumnName    uint16 = 3
	FKRefTableName  uint16 = 4
	FKRefColumnName uint16 = 5
	FKUpdateRule    uint16 = 6
	FKDeleteRule    uint16 = 7
	FKSchema        uint16 = 8
	FKRefSchema     uint16 = 9
)

// AutoInc field IDs (RecordType 8).
const (
	AINext       uint16 = 3 // Sint
	AIColumnType uint16 = 4
	AISchema     uint16 = 5
	AISeed       uint16 = 6 // Sint
	AIIncrement  uint16 = 7 // Sint
	AIKind       uint16 = 8
	AIGenerated  uint16 = 9
	AICache      uint16 = 10 // Sint
)

// TableComment field IDs (RecordType 9).
const (
	TCComment uint16 = 3 // 原始字符串
)

// ColComment field IDs (RecordType 10).
const (
	CCComment uint16 = 3 // 原始字符串
)

// Expression sub-field IDs.
const (
	ExprLanguage       uint16 = 1
	ExprText           uint16 = 2
	ExprNormalizedText uint16 = 3
	ExprBinaryAST      uint16 = 4 // Bytes
	ExprASTFormat      uint16 = 5
)

// sint marks Sint wire type; str is String; fs is FieldSet; sl is StringList.
func sint() fileformat.WireType { return fileformat.WireSint }
func str() fileformat.WireType  { return fileformat.WireString }
func fs() fileformat.WireType   { return fileformat.WireFieldSet }
func sl() fileformat.WireType   { return fileformat.WireStringList }

// CoreFieldSchemas maps every core RecordType to its canonical field set.
var CoreFieldSchemas = map[uint32]KnownFieldSchema{
	uint32(fileformat.RecordHeader): {
		HeaderDBType: str(), HeaderUserID: str(), HeaderDBName: str(),
		HeaderIPAddr: str(), HeaderPort: str(), HeaderDBVersion: str(),
		HeaderCharset: str(), HeaderCollation: str(), HeaderVersion: str(),
		HeaderProperties: fs(), HeaderExcludes: sl(), HeaderIncludes: sl(),
		HeaderSnapshotType: sint(), HeaderSnapshotToken: str(), HeaderSnapshotAt: sint(),
	},
	uint32(fileformat.RecordTable): {
		TableTableName: str(), TableTotalRows: sint(), TableBatchSize: sint(),
		TableBytes: sint(), TableSchema: str(), TableProps: fs(),
	},
	uint32(fileformat.RecordColumn): {
		ColTableName: str(), ColColumnName: str(), ColDataType: str(),
		ColDataLength: sint(), ColCharLength: sint(), ColDataPrec: sint(),
		ColDataScale: sint(), ColNullable: str(), ColDataDefault: str(),
		ColColumnID: sint(), ColCharUsed: str(), ColSchema: str(), ColColumnType: str(),
	},
	uint32(fileformat.RecordPrimaryKey): {
		PKConsName: str(), PKTableName: str(), PKColumnName: str(),
		PKKeySeq: sint(), PKSchema: str(),
	},
	uint32(fileformat.RecordIndex): {
		IdxIndexName: str(), IdxTableName: str(), IdxComment: str(),
		IdxColumns: str(), IdxSchema: str(),
	},
	uint32(fileformat.RecordUniqueKey): {
		UKTableName: str(), UKConsName: str(), UKColumns: str(), UKSchema: str(),
	},
	uint32(fileformat.RecordForeignKey): {
		FKTableName: str(), FKConsName: str(), FKColumnName: str(),
		FKRefTableName: str(), FKRefColumnName: str(), FKUpdateRule: str(),
		FKDeleteRule: str(), FKSchema: str(), FKRefSchema: str(),
	},
	uint32(fileformat.RecordAutoInc): {
		FTableName: str(), FColumnName: str(), AINext: sint(), AIColumnType: str(),
		AISchema: str(), AISeed: sint(), AIIncrement: sint(), AIKind: str(),
		AIGenerated: str(), AICache: sint(),
	},
	uint32(fileformat.RecordTableComment): {
		FTableName: str(), FSyntaxTableType: str(), TCComment: str(), FSchema: str(),
	},
	uint32(fileformat.RecordColComment): {
		FTableName: str(), FColumnName: str(), CCComment: str(), FSchema: str(),
	},
	uint32(fileformat.RecordView): {
		FViewName: str(), FViewText: str(), FSchema: str(),
	},
	uint32(fileformat.RecordFunction): {
		FFuncName: str(), FFuncText: str(), FSchema: str(),
	},
	uint32(fileformat.RecordVirtualColumn): {
		ColTableName: str(), ColColumnName: str(), ColDataType: str(),
		ColDataLength: sint(), ColCharLength: sint(), ColDataPrec: sint(),
		ColDataScale: sint(), ColNullable: str(), ColDataDefault: str(),
		ColColumnID: sint(), ColCharUsed: str(), ColSchema: str(), ColColumnType: str(),
	},
}

// Additional shared field IDs used above.
const (
	FSyntaxTableType uint16 = 2 // TableComment.TableType
	FViewName        uint16 = 1
	FViewText        uint16 = 2
	FFuncName        uint16 = 1
	FFuncText        uint16 = 2
)
