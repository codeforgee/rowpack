package metadata

import "github.com/rowpack/rowpack/internal/format"

// Table field IDs (RecordType 1 = format.RecordTable). FieldIDs are allocated densely from 1 per
// record type: a number is only frozen once some file has been written with it,
// so an unused reservation is reclaimed rather than left as a gap.
const (
	TableName uint16 = 1
	TableNS   uint16 = 2
)

// Column field IDs (RecordType 2 = format.RecordColumn). ColumnID leads so that the record's
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
func sint() format.WireType { return format.WireSint }
func str() format.WireType  { return format.WireString }

// CoreFieldSchemas maps the engine's schema record types to their canonical
// field set. Records decoded with a nil schema keep every field verbatim;
// with a schema, known fields are validated and unknown critical fields are
// rejected while unknown non-critical fields are preserved losslessly.
var CoreFieldSchemas = map[uint32]KnownFieldSchema{
	uint32(format.RecordTable): {
		TableName: str(), TableNS: str(),
	},
	uint32(format.RecordColumn): {
		ColColumnName: str(), ColColumnType: str(), ColNullable: str(),
		ColColumnID: sint(), ColDataScale: sint(),
	},
}
