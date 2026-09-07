package metadata

import "github.com/rowpack/rowpack/internal/fileformat"

// Core field IDs and wire types for the engine's schema records
// (Table and Column). These are the records DefineSchema writes itself;
// every field listed here is written by DefineSchema and read back by
// schema derivation. Foreign/upper-layer metadata objects are not part
// of the engine. FieldID numbering is frozen once published.

// Table field IDs (RecordType 2).
const (
	TableTableName uint16 = 1
)

// Column field IDs (RecordType 3). ColumnID leads so that the record's
// first field identifies the column position; the rest follow in DefineSchema
// semantic order.
const (
	ColColumnID   uint16 = 1 // Sint，1-based 列序
	ColColumnName uint16 = 2
	ColColumnType uint16 = 3 // 规范类型字符串（uint64/string/decimal…）
	ColNullable   uint16 = 4 // 规范 YES/NO
	ColDataScale  uint16 = 5 // Sint，仅 Decimal 非零
)

// str is the String wire type; sint marks Sint.
func sint() fileformat.WireType { return fileformat.WireSint }
func str() fileformat.WireType  { return fileformat.WireString }

// CoreFieldSchemas maps the engine's schema record types to their canonical
// field set. Records decoded with a nil schema keep every field verbatim;
// with a schema, known fields are validated and unknown critical fields are
// rejected while unknown non-critical fields are preserved losslessly.
var CoreFieldSchemas = map[uint32]KnownFieldSchema{
	uint32(fileformat.RecordTable): {
		TableTableName: str(),
	},
	uint32(fileformat.RecordColumn): {
		ColColumnName: str(), ColColumnType: str(), ColNullable: str(),
		ColColumnID: sint(), ColDataScale: sint(),
	},
}
