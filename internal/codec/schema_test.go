package codec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchemaValidate(t *testing.T) {
	lim := DefaultLimits()

	t.Run("valid schema", func(t *testing.T) {
		s := &Schema{
			Name: "t",
			Columns: []Column{
				{Name: "id", Type: TypeUint64},
				{Name: "name", Type: TypeString},
			},
		}
		require.NoError(t, s.Validate(lim))
	})

	t.Run("empty name", func(t *testing.T) {
		s := &Schema{Name: "", Columns: []Column{{Name: "id", Type: TypeUint64}}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "schema name is empty")
	})

	t.Run("too many columns", func(t *testing.T) {
		cols := make([]Column, lim.MaxColumns+1)
		for i := range cols {
			cols[i] = Column{Name: "c", Type: TypeInt64}
		}
		s := &Schema{Name: "t", Columns: cols}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "columns, limit")
	})

	t.Run("duplicate column names", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "id", Type: TypeString},
		}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "duplicate column name")
	})

	t.Run("empty column name", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{{Name: "", Type: TypeInt64}}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "empty name")
	})

	t.Run("zero type", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{{Name: "id", Type: 0}}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "zero type")
	})

	t.Run("unknown type", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{{Name: "id", Type: 999}}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "unknown type")
	})

	t.Run("decimal negative scale", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{{Name: "d", Type: TypeDecimal, Scale: -1}}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "negative scale")
	})

	t.Run("decimal excessive scale", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{{Name: "d", Type: TypeDecimal, Scale: 1_000_000_001}}}
		err := s.Validate(lim)
		require.Error(t, err)
		require.Contains(t, err.Error(), "excessive scale")
	})

	t.Run("valid decimal scale", func(t *testing.T) {
		s := &Schema{Name: "t", Columns: []Column{{Name: "d", Type: TypeDecimal, Scale: 2}}}
		require.NoError(t, s.Validate(lim))
	})
}

func TestSchemaClone(t *testing.T) {
	s := &Schema{
		TableID: 1,
		Version: 2,
		Name:    "test",
		Columns: []Column{
			{Name: "a", Type: TypeInt64},
			{Name: "b", Type: TypeString, Nullable: true},
		},
	}
	cloned := s.Clone()
	require.Equal(t, s.TableID, cloned.TableID)
	require.Equal(t, s.Version, cloned.Version)
	require.Equal(t, s.Name, cloned.Name)
	require.Equal(t, len(s.Columns), len(cloned.Columns))
	require.Equal(t, s.Columns[0].Name, cloned.Columns[0].Name)
	require.Equal(t, s.Columns[1].Nullable, cloned.Columns[1].Nullable)

	cloned.Columns[0].Name = "modified"
	require.NotEqual(t, s.Columns[0].Name, cloned.Columns[0].Name)
}

func TestValidValueType(t *testing.T) {
	validTypes := []Type{
		TypeBool, TypeInt8, TypeInt16, TypeInt32, TypeInt64,
		TypeUint8, TypeUint16, TypeUint32, TypeUint64,
		TypeFloat32, TypeFloat64, TypeString, TypeBytes,
		TypeDate, TypeTime, TypeDateTime, TypeDecimal,
	}
	for _, tt := range validTypes {
		require.True(t, validValueType(tt), "type %d should be valid", tt)
	}
	require.False(t, validValueType(999))
}

func TestDefaultLimits(t *testing.T) {
	lim := DefaultLimits()
	require.Equal(t, uint32(16384), lim.MaxColumns)
	require.Equal(t, uint32(64<<20), lim.MaxValueBytes)
	require.Equal(t, uint32(64<<20), lim.MaxRowBytes)
}

func TestCheckString(t *testing.T) {
	lim := DefaultLimits()

	require.NoError(t, CheckString("hello", lim))
	require.NoError(t, CheckString("", lim))

	long := string(make([]byte, lim.MaxValueBytes+1))
	err := CheckString(long, lim)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds limit")

	invalidUTF8 := string([]byte{0xff, 0xfe})
	err = CheckString(invalidUTF8, lim)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not valid UTF-8")
}
