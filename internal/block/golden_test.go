package block

import (
	"bytes"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// updateGolden regenerates golden files. Enable with `-args -update-golden`.
var updateGolden = flag.Bool("update-golden", false, "regenerate golden files")

func goldenPath(name string) string {
	return filepath.Join("..", "..", "testdata", "golden", name)
}

// TestGoldenRowsPayloadAllTypes locks the deterministic uncompressed Rows
// payload produced by the codec and block builder for a fixed all-types schema
// and a fixed row set. Any change to TypedTuple or the rows payload layout
// breaks this test.
func TestGoldenRowsPayloadAllTypes(t *testing.T) {
	schema := &codec.Schema{TableID: 1, Version: 1, Name: "golden", Columns: []codec.Column{
		{Name: "b", Type: codec.TypeBool},
		{Name: "i64", Type: codec.TypeInt64},
		{Name: "u32", Type: codec.TypeUint32},
		{Name: "f64", Type: codec.TypeFloat64},
		{Name: "s", Type: codec.TypeString},
		{Name: "by", Type: codec.TypeBytes},
		{Name: "d", Type: codec.TypeDate},
		{Name: "t", Type: codec.TypeTime},
		{Name: "dt", Type: codec.TypeDateTime},
		{Name: "dec", Type: codec.TypeDecimal, Scale: 4},
		{Name: "maybe", Type: codec.TypeString, Nullable: true},
	}}
	row, err := codec.Encode(schema, []codec.Value{
		codec.Bool(true),
		codec.Int64(-987654321012345),
		codec.Uint32(4294967295),
		codec.Float64(3.141592653589793),
		codec.String("黄金行 Δemo😀"),
		codec.Bytes([]byte{0x00, 0x01, 0xFE, 0xFF}),
		codec.DateValue(19723),
		codec.TimeValue(codec.TimeOfDay(43200000000001)),
		codec.DateTime(time.Unix(0, 1700000000123456789).UTC()),
		codec.DecimalValue(codec.Decimal{Unscaled: big.NewInt(-1234567890123), Scale: 4}),
		codec.Null(),
	}, codec.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}

	var sink captureSink
	b := NewRowsBlockBuilder(1, 1, 1<<20, fileformat.CompressionNone, 0, DefaultLimits(), sink.flush)
	for i := 0; i < 3; i++ {
		if err := b.Add(uint64(100+i), 1, fileformat.ChangeInsert, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(sink.blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(sink.blocks))
	}
	payload := sink.blocks[0].payload // None compression: stored == raw

	path := goldenPath("rows-payload-all-types.bin")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (regenerate with make golden)", path, err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("golden %s differs from implementation (regenerate with make golden)", path)
	}
	// The golden must parse back.
	p, err := ParseRowsPayload(payload, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 3 {
		t.Fatalf("golden payload has %d entries, want 3", len(p.Entries))
	}
}
