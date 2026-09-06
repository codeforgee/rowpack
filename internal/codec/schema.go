package codec

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Schema describes the ordered column layout of one table version. It is the
// type-derivation input for TypedTuple encoding: each column's Type decides
// the disk encoding, no per-value type tag is written.
type Schema struct {
	TableID uint32
	Version uint32
	Name    string
	Columns []Column
}

// Clone returns a deep copy of the schema.
func (s *Schema) Clone() *Schema {
	out := &Schema{TableID: s.TableID, Version: s.Version, Name: s.Name, Columns: make([]Column, len(s.Columns))}
	copy(out.Columns, s.Columns)
	return out
}

// Column describes one column of a Schema.
type Column struct {
	Name     string
	Type     Type
	Nullable bool
	Scale    int32 // only used when Type == TypeDecimal
}

// Validate checks the schema against the given limits and the v1 type rules.
// It returns an error for unknown types, too many columns, negative decimal
// scale, empty names, or an illegal fixed-width combination.
func (s *Schema) Validate(limits Limits) error {
	if s.Name == "" {
		return errors.New("rowpack: schema name is empty")
	}
	if uint32(len(s.Columns)) > limits.MaxColumns {
		return fmt.Errorf("rowpack: schema %q has %d columns, limit %d", s.Name, len(s.Columns), limits.MaxColumns)
	}
	seen := make(map[string]struct{}, len(s.Columns))
	for i, c := range s.Columns {
		if c.Name == "" {
			return fmt.Errorf("rowpack: schema %q column %d has empty name", s.Name, i)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("rowpack: schema %q duplicate column name %q", s.Name, c.Name)
		}
		seen[c.Name] = struct{}{}
		if c.Type == 0 {
			return fmt.Errorf("rowpack: schema %q column %q has zero type", s.Name, c.Name)
		}
		if !validValueType(c.Type) {
			return fmt.Errorf("rowpack: schema %q column %q has unknown type %d", s.Name, c.Name, c.Type)
		}
		if c.Type == TypeDecimal {
			if c.Scale < 0 {
				return fmt.Errorf("rowpack: schema %q column %q has negative scale %d", s.Name, c.Name, c.Scale)
			}
			if c.Scale > 1_000_000_000 {
				return fmt.Errorf("rowpack: schema %q column %q has excessive scale %d", s.Name, c.Name, c.Scale)
			}
		}
	}
	return nil
}

func validValueType(t Type) bool {
	switch t {
	case TypeBool, TypeInt8, TypeInt16, TypeInt32, TypeInt64,
		TypeUint8, TypeUint16, TypeUint32, TypeUint64,
		TypeFloat32, TypeFloat64, TypeString, TypeBytes,
		TypeDate, TypeTime, TypeDateTime, TypeDecimal:
		return true
	}
	return false
}

// Limits bounds allocations during encode/decode of untrusted input.
type Limits struct {
	MaxColumns    uint32
	MaxValueBytes uint32
	MaxRowBytes   uint32
}

// DefaultLimits returns the v1 default safety limits.
func DefaultLimits() Limits {
	return Limits{
		MaxColumns:    fileformat.DefaultMaxColumns,
		MaxValueBytes: fileformat.DefaultMaxValueBytes,
		MaxRowBytes:   fileformat.DefaultMaxRowBytes,
	}
}

// CheckString validates that s is valid UTF-8 and within the value byte limit.
func CheckString(s string, limits Limits) error {
	if uint32(len(s)) > limits.MaxValueBytes {
		return fmt.Errorf("rowpack: string value of %d bytes exceeds limit %d", len(s), limits.MaxValueBytes)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("rowpack: string value is not valid UTF-8")
	}
	return nil
}
