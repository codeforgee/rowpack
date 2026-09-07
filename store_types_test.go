package rowpack

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"
)

// TestStoreRoundTripAllTypes covers the complete persistence path for every
// public value type: write a FULL snapshot, close/reopen the Store, then read
// the row through both Get and Scan.
func TestStoreRoundTripAllTypes(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir() + "/all-types"

	schema := Schema{TableID: 1, Version: 1, Name: "all_types", Columns: []Column{
		{Name: "bool", Type: TypeBool},
		{Name: "int8", Type: TypeInt8},
		{Name: "int16", Type: TypeInt16},
		{Name: "int32", Type: TypeInt32},
		{Name: "int64", Type: TypeInt64},
		{Name: "uint8", Type: TypeUint8},
		{Name: "uint16", Type: TypeUint16},
		{Name: "uint32", Type: TypeUint32},
		{Name: "uint64", Type: TypeUint64},
		{Name: "float32", Type: TypeFloat32},
		{Name: "float64", Type: TypeFloat64},
		{Name: "string", Type: TypeString},
		{Name: "bytes", Type: TypeBytes},
		{Name: "date", Type: TypeDate},
		{Name: "time", Type: TypeTime},
		{Name: "datetime", Type: TypeDateTime},
		{Name: "decimal", Type: TypeDecimal, Scale: 6},
	}}
	want := Row{
		Bool(true),
		Int8(-128),
		Int16(-32768),
		Int32(math.MinInt32),
		Int64(math.MinInt64),
		Uint8(math.MaxUint8),
		Uint16(math.MaxUint16),
		Uint32(math.MaxUint32),
		Uint64(math.MaxUint64),
		Float32(float32(math.Pi)),
		Float64(math.E),
		String("张三\n\tquoted 😀"),
		Bytes([]byte{0, 1, 2, 0xff}),
		DateValue(Date(-2147483648)),
		TimeValue(TimeOfDay(23*60*60*1_000_000_000 + 59*60*1_000_000_000 + 59*1_000_000_000 + 999_999_999)),
		DateTime(time.Unix(0, math.MaxInt64).UTC()),
		DecimalValue(Decimal{Unscaled: big.NewInt(-1234567890123456789), Scale: 6}),
	}

	db, err := Create(base, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.BeginSnapshot(ctx, SnapshotFull, SnapshotOptions{})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := w.DefineSchema(schema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := w.Insert(ctx, schema.TableID, 1, schema.Version, want); err != nil {
		db.Close()
		t.Fatal(err)
	}
	full, err := w.Commit(ctx)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(base, Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	got, err := db.Get(ctx, full.ID, schema.TableID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertRowsEqual(t, want, got)

	it, err := db.Scan(ctx, full.ID, schema.TableID, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	row, ok := it.Next()
	if !ok {
		t.Fatalf("Scan returned no row: %v", it.Err())
	}
	assertRowsEqual(t, want, row)
	if _, ok := it.Next(); ok {
		t.Fatal("Scan returned more than one row")
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
}

func assertRowsEqual(t *testing.T, want, got Row) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("row length: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if want[i].Type() != got[i].Type() || want[i].IsNull() != got[i].IsNull() {
			t.Fatalf("column %d type/null mismatch: got %v/%v want %v/%v", i, got[i].Type(), got[i].IsNull(), want[i].Type(), want[i].IsNull())
		}
		switch want[i].Type() {
		case TypeBool:
			a, _ := want[i].Bool()
			b, _ := got[i].Bool()
			if a != b {
				t.Fatalf("column %d: bool mismatch", i)
			}
		case TypeInt8:
			a, _ := want[i].Int8()
			b, _ := got[i].Int8()
			if a != b {
				t.Fatalf("column %d: int8 mismatch", i)
			}
		case TypeInt16:
			a, _ := want[i].Int16()
			b, _ := got[i].Int16()
			if a != b {
				t.Fatalf("column %d: int16 mismatch", i)
			}
		case TypeInt32:
			a, _ := want[i].Int32()
			b, _ := got[i].Int32()
			if a != b {
				t.Fatalf("column %d: int32 mismatch", i)
			}
		case TypeInt64:
			a, _ := want[i].Int64()
			b, _ := got[i].Int64()
			if a != b {
				t.Fatalf("column %d: int64 mismatch", i)
			}
		case TypeUint8:
			a, _ := want[i].Uint8()
			b, _ := got[i].Uint8()
			if a != b {
				t.Fatalf("column %d: uint8 mismatch", i)
			}
		case TypeUint16:
			a, _ := want[i].Uint16()
			b, _ := got[i].Uint16()
			if a != b {
				t.Fatalf("column %d: uint16 mismatch", i)
			}
		case TypeUint32:
			a, _ := want[i].Uint32()
			b, _ := got[i].Uint32()
			if a != b {
				t.Fatalf("column %d: uint32 mismatch", i)
			}
		case TypeUint64:
			a, _ := want[i].Uint64()
			b, _ := got[i].Uint64()
			if a != b {
				t.Fatalf("column %d: uint64 mismatch", i)
			}
		case TypeFloat32:
			a, _ := want[i].Float32()
			b, _ := got[i].Float32()
			if math.Float32bits(a) != math.Float32bits(b) {
				t.Fatalf("column %d: float32 mismatch", i)
			}
		case TypeFloat64:
			a, _ := want[i].Float64()
			b, _ := got[i].Float64()
			if math.Float64bits(a) != math.Float64bits(b) {
				t.Fatalf("column %d: float64 mismatch", i)
			}
		case TypeString:
			a, _ := want[i].String()
			b, _ := got[i].String()
			if a != b {
				t.Fatalf("column %d: string mismatch", i)
			}
		case TypeBytes:
			a, _ := want[i].Bytes()
			b, _ := got[i].Bytes()
			if string(a) != string(b) {
				t.Fatalf("column %d: bytes mismatch", i)
			}
		case TypeDate:
			a, _ := want[i].Date()
			b, _ := got[i].Date()
			if a != b {
				t.Fatalf("column %d: date mismatch", i)
			}
		case TypeTime:
			a, _ := want[i].Time()
			b, _ := got[i].Time()
			if a != b {
				t.Fatalf("column %d: time mismatch", i)
			}
		case TypeDateTime:
			a, _ := want[i].DateTimeValue()
			b, _ := got[i].DateTimeValue()
			if !a.Equal(b) {
				t.Fatalf("column %d: datetime mismatch", i)
			}
		case TypeDecimal:
			a, _ := want[i].Decimal()
			b, _ := got[i].Decimal()
			if a.Scale != b.Scale || a.Unscaled.Cmp(b.Unscaled) != 0 {
				t.Fatalf("column %d: decimal mismatch", i)
			}
		default:
			t.Fatalf("column %d: unsupported type %v", i, want[i].Type())
		}
	}
}
