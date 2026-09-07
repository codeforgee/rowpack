package rowpack

import (
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// Identifier types. Zero values are invalid for ordinary objects except where
// documented (SnapshotID(0) denotes "no parent snapshot").
type (
	SnapshotID    = uint64
	TableID       = uint32
	RowID         = uint64
	SchemaVersion = uint32
)

// Type is the logical value type of a column and equals the disk Type ID used
// by the TypedTuple v1 row encoding. The enum values are frozen; they must not
// be renumbered after release.
type Type = fileformat.ValueType

const (
	TypeBool     Type = fileformat.TypeBool
	TypeInt8     Type = fileformat.TypeInt8
	TypeInt16    Type = fileformat.TypeInt16
	TypeInt32    Type = fileformat.TypeInt32
	TypeInt64    Type = fileformat.TypeInt64
	TypeUint8    Type = fileformat.TypeUint8
	TypeUint16   Type = fileformat.TypeUint16
	TypeUint32   Type = fileformat.TypeUint32
	TypeUint64   Type = fileformat.TypeUint64
	TypeFloat32  Type = fileformat.TypeFloat32
	TypeFloat64  Type = fileformat.TypeFloat64
	TypeString   Type = fileformat.TypeString
	TypeBytes    Type = fileformat.TypeBytes
	TypeDate     Type = fileformat.TypeDate
	TypeTime     Type = fileformat.TypeTime
	TypeDateTime Type = fileformat.TypeDateTime
	TypeDecimal  Type = fileformat.TypeDecimal
)

// Date is a calendar date expressed as days since the Unix epoch.
type Date = codec.Date

// TimeOfDay is a time of day expressed as nanoseconds since midnight in the
// range [0, 86400e9).
type TimeOfDay = codec.TimeOfDay

// Decimal is a decimal fixed-point value: Unscaled is the two's-complement
// unscaled integer and Scale is the number of fractional digits. Scale must
// be >= 0 and Unscaled non-nil.
type Decimal = codec.Decimal

// Schema describes the ordered column layout of a table version.
type Schema = codec.Schema

// Column describes one column of a Schema.
type Column = codec.Column

// TableInfo is a lightweight table identity returned by Table listing APIs.
type TableInfo struct {
	ID            TableID
	Name          string
	LatestVersion SchemaVersion
}
