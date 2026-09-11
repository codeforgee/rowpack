package rowpack

import (
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
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
type Type = format.ValueType

const (
	TypeBool     Type = format.TypeBool
	TypeInt8     Type = format.TypeInt8
	TypeInt16    Type = format.TypeInt16
	TypeInt32    Type = format.TypeInt32
	TypeInt64    Type = format.TypeInt64
	TypeUint8    Type = format.TypeUint8
	TypeUint16   Type = format.TypeUint16
	TypeUint32   Type = format.TypeUint32
	TypeUint64   Type = format.TypeUint64
	TypeFloat32  Type = format.TypeFloat32
	TypeFloat64  Type = format.TypeFloat64
	TypeString   Type = format.TypeString
	TypeBytes    Type = format.TypeBytes
	TypeDate     Type = format.TypeDate
	TypeTime     Type = format.TypeTime
	TypeDateTime Type = format.TypeDateTime
	TypeDecimal  Type = format.TypeDecimal
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

// Table ns. A table is identified by (ns, name); the default ns is NSUser and
// needs no metadata, so a table defined by DefineTable carries no ns field at
// all and stores written before ns existed keep their exact bytes.
//
// The engine attaches no semantics to an ns: it is a caller-chosen label. Two
// tables sharing a name may coexist as long as their ns differ.
//
// Every API that takes a table takes an address string:
//
//	"users"         -> ns "user",   name "users"
//	"public.users"  -> ns "public", name "users"
//	"public.a.b"    -> ns "public", name "a.b"   (names may contain the separator)
//
// The default ns is addressed by the bare name; any other ns is prefixed with
// "ns.". No character is forbidden in an ns or a name. Two pairs can still
// produce the same address -- a dotted name in NSUser ("a.b") versus a ns named
// after its prefix (ns "a", name "b") -- and that is a real conflict: the second
// DefineTable reports ErrSchemaConflict. Inside the engine an address is only
// ever used as a map key, never parsed, so lookup is unambiguous.
const NSUser = "user"

// NSSeparator separates the ns from the name in a table address.
const NSSeparator = "."

// Qualify returns the address that names (ns, name). Tables in NSUser keep
// their bare name.
func Qualify(ns, name string) string {
	if ns == "" || ns == NSUser {
		return name
	}
	return ns + NSSeparator + name
}

// SplitAddress splits a table address at its first separator: everything before
// it is the ns, everything after it is the name. A bare name belongs to NSUser.
//
// Names may contain the separator, so this is not a general inverse of Qualify
// when a ns itself contains one (an ns named "a.b" and a table "c" produce
// "a.b.c", which splits as ns "a", name "b.c"). Table.NS and Table.Name are the
// authoritative decomposition; resolution inside the engine never parses an
// address, it looks it up.
func SplitAddress(address string) (ns, name string) {
	for i := 0; i < len(address); i++ {
		if address[i] == NSSeparator[0] {
			return address[:i], address[i+1:]
		}
	}
	return NSUser, address
}

// Table is a lightweight table identity returned by Table listing APIs.
type Table struct {
	ID            TableID
	Name          string
	LatestVersion SchemaVersion
	NS            string
}

// Address returns the string that addresses this table in table-taking APIs.
func (t Table) Address() string { return Qualify(t.NS, t.Name) }

// Block describes one rows block written by a snapshot transaction, in
// physical write order. MinRowID/MaxRowID bound the primary keys held in the
// block and are derived from the in-memory row index (zero block I/O).
type Block struct {
	BlockID     uint64
	ItemCount   uint32
	MinRowID    RowID // inclusive
	MaxRowID    RowID // exclusive
	RawBytes    uint32
	StoredBytes uint32
}
