package codec

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEncodeIntoGuards covers the guard branches of EncodeInto: nil schema,
// column-limit, row-length mismatch, and the reuse-capacity fast path.
func TestEncodeIntoGuards(t *testing.T) {
	schema := &Schema{
		Name: "t",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "name", Type: TypeString},
		},
	}
	row := []Value{Uint64(1), String("x")}
	lim := DefaultLimits()

	if _, err := testCodec.EncodeInto(nil, row, nil); err == nil {
		t.Fatal("nil schema should error")
	}

	over := &Schema{Name: "big", Columns: make([]Column, lim.MaxColumns+1)}
	for i := range over.Columns {
		over.Columns[i] = Column{Name: fmt.Sprintf("c%d", i), Type: TypeUint64}
	}
	if _, err := testCodec.EncodeInto(over, make([]Value, len(over.Columns)), nil); err == nil {
		t.Fatal("column limit should error")
	}

	if _, err := testCodec.EncodeInto(schema, []Value{Uint64(1)}, nil); !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("row length mismatch: err %v, want ErrSchemaMismatch", err)
	}

	// reuse with sufficient capacity must be used (len(cap) path).
	reuse := make([]byte, 0, 4096)
	enc, err := testCodec.EncodeInto(schema, row, reuse)
	if err != nil {
		t.Fatalf("EncodeInto reuse: %v", err)
	}
	ref, err := testCodec.EncodeInto(schema, row, nil)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	require.Equal(t, ref, enc)
}

// TestDecodeBodyIntoGuards covers the nil-schema and column-limit guards.
func TestDecodeBodyIntoGuards(t *testing.T) {
	if _, err := testCodec.CompileDecoder(nil); err == nil {
		t.Fatal("nil schema should error")
	}

	over := &Schema{Name: "big", Columns: make([]Column, DefaultLimits().MaxColumns+1)}
	for i := range over.Columns {
		over.Columns[i] = Column{Name: fmt.Sprintf("c%d", i), Type: TypeUint64}
	}
	if _, err := testCodec.CompileDecoder(over); err == nil {
		t.Fatal("column limit should error")
	}
}

// TestCheckDuplicateColumnNamesMapPath drives the hash-map branch (schemas
// above validateMapThreshold columns) and its error outputs.
func TestCheckDuplicateColumnNamesMapPath(t *testing.T) {
	n := validateMapThreshold + 8

	dup := make([]Column, n)
	for i := range dup {
		dup[i] = Column{Name: fmt.Sprintf("c%d", i), Type: TypeUint64}
	}
	dup[n-1].Name = "c0" // duplicate with the first column
	if err := checkDuplicateColumnNames(&Schema{Name: "s", Columns: dup}, 1<<20); err == nil {
		t.Fatal("duplicate column name above threshold should error")
	}

	empty := make([]Column, n)
	for i := range empty {
		empty[i] = Column{Name: fmt.Sprintf("c%d", i), Type: TypeUint64}
	}
	empty[5].Name = ""
	if err := checkDuplicateColumnNames(&Schema{Name: "s", Columns: empty}, 1<<20); err == nil {
		t.Fatal("empty column name above threshold should error")
	}

	ok := make([]Column, n)
	for i := range ok {
		ok[i] = Column{Name: fmt.Sprintf("c%d", i), Type: TypeUint64}
	}
	if err := checkDuplicateColumnNames(&Schema{Name: "s", Columns: ok}, 1<<20); err != nil {
		t.Fatalf("unique names above threshold should pass: %v", err)
	}
}

// TestValueGettersWrongType confirms typed getters reject mismatched values.
func TestValueGettersWrongType(t *testing.T) {
	v := String("not a number")
	if got, ok := v.Uint64(); ok || got != 0 {
		t.Fatalf("Uint64 on string = %d, %v", got, ok)
	}
	if _, ok := v.Int64(); ok {
		t.Fatal("Int64 on string must fail")
	}
}

// TestGetVarLenShort covers the short-input branch of the length-prefix read.
func TestGetVarLenShort(t *testing.T) {
	if n, ok := getVarLen([]byte{1, 2}); ok || n != 0 {
		t.Fatalf("getVarLen short = %d, %v", n, ok)
	}
	n, ok := getVarLen([]byte{4, 0, 0, 0, 'a', 'b', 'c', 'd'})
	if !ok || n != 4 {
		t.Fatalf("getVarLen = %d, %v", n, ok)
	}
}

// TestDecodeTruncatedString covers a string value whose declared length runs
// past the end of the body.
func TestDecodeTruncatedString(t *testing.T) {
	schema := &Schema{
		Name: "t",
		Columns: []Column{
			{Name: "s", Type: TypeString},
		},
	}
	body, err := testCodec.EncodeInto(schema, []Value{String("abcdef")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Chop bytes off the end; the decoder must report a column error.
	_, err = decodeTestBody(t, testCodec, schema, body[:len(body)-2])
	if err == nil {
		t.Fatal("truncated string value should error")
	}
	if !strings.Contains(err.Error(), "decode column") {
		t.Fatalf("error should identify the failing column: %v", err)
	}
}
