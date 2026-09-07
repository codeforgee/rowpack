package codec

import (
	"math"
	"math/big"
	"testing"
)

func allTypesSchema() *Schema {
	return schemaOf(
		Column{Name: "b", Type: TypeBool, Nullable: true},
		Column{Name: "i8", Type: TypeInt8, Nullable: true},
		Column{Name: "i16", Type: TypeInt16, Nullable: true},
		Column{Name: "i32", Type: TypeInt32, Nullable: true},
		Column{Name: "i64", Type: TypeInt64, Nullable: true},
		Column{Name: "u8", Type: TypeUint8, Nullable: true},
		Column{Name: "u16", Type: TypeUint16, Nullable: true},
		Column{Name: "u32", Type: TypeUint32, Nullable: true},
		Column{Name: "u64", Type: TypeUint64, Nullable: true},
		Column{Name: "f32", Type: TypeFloat32, Nullable: true},
		Column{Name: "f64", Type: TypeFloat64, Nullable: true},
		Column{Name: "s", Type: TypeString, Nullable: true},
		Column{Name: "by", Type: TypeBytes, Nullable: true},
		Column{Name: "d", Type: TypeDate, Nullable: true},
		Column{Name: "t", Type: TypeTime, Nullable: true},
		Column{Name: "dt", Type: TypeDateTime, Nullable: true},
		Column{Name: "dec", Type: TypeDecimal, Nullable: true, Scale: 4},
	)
}

func fullRow() []Value {
	return []Value{
		Bool(true),
		Int8(-128),
		Int16(-32768),
		Int32(math.MinInt32),
		Int64(math.MinInt64),
		Uint8(255),
		Uint16(65535),
		Uint32(math.MaxUint32),
		Uint64(math.MaxUint64),
		Float32(float32(math.Pi)),
		Float64(math.E),
		String("张三\n\t\"quoted\" 😀"),
		Bytes([]byte{0, 1, 2, 0xFF}),
		DateValue(19723),
		TimeValue(TimeOfDay(12345678901)),
		DateTimeValueOf(1700000000123456789),
		DecimalValue(Decimal{Unscaled: big.NewInt(-123456789012345), Scale: 4}),
	}
}

// DateTimeValueOf builds a DateTime Value from a unix nano literal.
func DateTimeValueOf(ns int64) Value { return Value{typ: TypeDateTime, i: ns} }

func TestRoundTripAllTypes(t *testing.T) {
	s := allTypesSchema()
	row := fullRow()
	enc, err := Encode(s, row, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc, s, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Compare value-by-value.
	for i, want := range row {
		if !valuesEqual(want, got[i]) {
			t.Errorf("column %d: got %#v, want %#v", i, got[i], want)
		}
	}
	// Encoded form is deterministic.
	enc2, _ := Encode(s, row, DefaultLimits())
	if string(enc) != string(enc2) {
		t.Fatal("non-deterministic encoding")
	}
}

func valuesEqual(a, b Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() == b.IsNull()
	}
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case TypeBool:
		x, _ := a.Bool()
		y, _ := b.Bool()
		return x == y
	case TypeInt8, TypeInt16, TypeInt32, TypeInt64:
		x, _ := a.Int64()
		y, _ := b.Int64()
		return x == y
	case TypeUint8, TypeUint16, TypeUint32, TypeUint64:
		x, _ := a.Uint64()
		y, _ := b.Uint64()
		return x == y
	case TypeFloat32:
		x, _ := a.Float32()
		y, _ := b.Float32()
		return math.Float32bits(x) == math.Float32bits(y)
	case TypeFloat64:
		x, _ := a.Float64()
		y, _ := b.Float64()
		return math.Float64bits(x) == math.Float64bits(y)
	case TypeString:
		x, _ := a.String()
		y, _ := b.String()
		return x == y
	case TypeBytes:
		x, _ := a.Bytes()
		y, _ := b.Bytes()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case TypeDate:
		x, _ := a.Date()
		y, _ := b.Date()
		return x == y
	case TypeTime:
		x, _ := a.Time()
		y, _ := b.Time()
		return x == y
	case TypeDateTime:
		x, _ := a.DateTimeValue()
		y, _ := b.DateTimeValue()
		return x.Equal(y)
	case TypeDecimal:
		x, _ := a.Decimal()
		y, _ := b.Decimal()
		return x.Scale == y.Scale && x.Unscaled.Cmp(y.Unscaled) == 0
	}
	return false
}

func TestNullBitmap(t *testing.T) {
	s := allTypesSchema()
	row := fullRow()
	row[0] = Null()  // b
	row[2] = Null()  // i16
	row[16] = Null() // dec
	enc, err := Encode(s, row, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc, s, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range row {
		if want.IsNull() != got[i].IsNull() {
			t.Errorf("column %d null mismatch: want %v got %v", i, want.IsNull(), got[i].IsNull())
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	s := schemaOf(Column{Name: "i64", Type: TypeInt64}, Column{Name: "s", Type: TypeString})
	row := []Value{Int64(1), String("x")}
	enc, _ := Encode(s, row, DefaultLimits())
	lim := DefaultLimits()

	// Truncated at every prefix.
	for n := 0; n < len(enc); n++ {
		if _, err := Decode(enc[:n], s, lim); err == nil {
			t.Fatalf("accepted truncated tuple of %d/%d bytes", n, len(enc))
		}
	}
	// Trailing bytes.
	if _, err := Decode(append(enc, 0), s, lim); err == nil {
		t.Fatal("accepted trailing byte")
	}
	// Wrong column count.
	bad := append([]byte(nil), enc...)
	bad[0] = 3
	if _, err := Decode(bad, s, lim); err == nil {
		t.Fatal("accepted wrong column count")
	}
	// Wrong bitmap size field.
	bad = append([]byte(nil), enc...)
	bad[4] = 9
	if _, err := Decode(bad, s, lim); err == nil {
		t.Fatal("accepted wrong bitmap size")
	}
	// Non-zero unused bitmap high bits: 2 columns => 1 bitmap byte, high 6 bits must be zero.
	bad = append([]byte(nil), enc...)
	bad[8] |= 0x80
	if _, err := Decode(bad, s, lim); err == nil {
		t.Fatal("accepted non-zero unused bitmap bits")
	}
	// Invalid bool byte.
	bs := schemaOf(Column{Name: "b", Type: TypeBool})
	be, _ := Encode(bs, []Value{Bool(true)}, lim)
	be[8] = 2
	if _, err := Decode(be, bs, lim); err == nil {
		t.Fatal("accepted invalid bool byte")
	}
	// Non-UTF8 string.
	ss := schemaOf(Column{Name: "s", Type: TypeString})
	se, _ := Encode(ss, []Value{String("ok")}, lim)
	// patch: string length 1, byte 0xFF
	se = se[:12]
	se[8] = 1
	se[9] = 0xFF
	// rebuild u32 lengths: header 8 + bitmap 1 + len4 + 1 byte = 14; fix nothing else needed
	if _, err := Decode(se, ss, lim); err == nil {
		t.Fatal("accepted non-UTF8 string")
	}
	// Out-of-range time of day.
	ts := schemaOf(Column{Name: "t", Type: TypeTime})
	te, _ := Encode(ts, []Value{TimeValue(1)}, lim)
	copy(te[9:], u64bytes(uint64(MaxTimeOfDay))) // 86400e9 out of range
	if _, err := Decode(te, ts, lim); err == nil {
		t.Fatal("accepted out-of-range time of day")
	}
}

func u64bytes(v uint64) []byte {
	return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24), byte(v >> 32), byte(v >> 40), byte(v >> 48), byte(v >> 56)}
}

func TestEncodeRejects(t *testing.T) {
	lim := DefaultLimits()

	// []Value length mismatch.
	s := schemaOf(Column{Name: "i", Type: TypeInt64})
	if _, err := Encode(s, []Value{}, lim); err == nil {
		t.Fatal("accepted empty row for 1-column schema")
	}
	// Wrong value type.
	if _, err := Encode(s, []Value{String("x")}, lim); err == nil {
		t.Fatal("accepted string as int64")
	}
	// NULL in non-nullable column.
	s2 := schemaOf(Column{Name: "i", Type: TypeInt64, Nullable: false})
	if _, err := Encode(s2, []Value{Null()}, lim); err == nil {
		t.Fatal("accepted NULL in non-nullable column")
	}
	// Decimal scale mismatch with schema column.
	s3 := schemaOf(Column{Name: "d", Type: TypeDecimal, Scale: 2})
	if _, err := Encode(s3, []Value{DecimalValue(Decimal{Unscaled: big.NewInt(1), Scale: 3})}, lim); err == nil {
		t.Fatal("accepted decimal scale mismatch")
	}
	// Unknown type.
	s4 := schemaOf(Column{Name: "x", Type: Type(99)})
	if _, err := Encode(s4, []Value{Int64(1)}, lim); err == nil {
		t.Fatal("accepted unknown type")
	}
	// Invalid UTF-8 string.
	s5 := schemaOf(Column{Name: "s", Type: TypeString})
	if _, err := Encode(s5, []Value{Value{typ: TypeString, s: string([]byte{0xFF, 0xFE})}}, lim); err == nil {
		t.Fatal("accepted invalid UTF-8 string")
	}
	// Out-of-range time of day.
	s6 := schemaOf(Column{Name: "t", Type: TypeTime})
	if _, err := Encode(s6, []Value{Value{typ: TypeTime, i: MaxTimeOfDay}}, lim); err == nil {
		t.Fatal("accepted out-of-range time")
	}
	// Too many columns.
	cols := make([]Column, 0, lim.MaxColumns+1)
	for i := uint32(0); i <= lim.MaxColumns; i++ {
		cols = append(cols, Column{Name: "c", Type: TypeInt64})
	}
	s7 := schemaOf(cols...)
	if _, err := Encode(s7, make([]Value, len(cols)), lim); err == nil {
		t.Fatal("accepted schema over column limit")
	}
}

func TestOversizeValueLimit(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxValueBytes = 8
	s := schemaOf(Column{Name: "s", Type: TypeString})
	if _, err := Encode(s, []Value{String("123456789")}, lim); err == nil {
		t.Fatal("accepted string over value limit")
	}
	big := make([]byte, 9)
	be, err := Encode(schemaOf(Column{Name: "by", Type: TypeBytes}), []Value{Bytes(big)}, lim)
	if err == nil {
		t.Fatal("accepted bytes over value limit")
	}
	_ = be
	// Decode side with malicious length prefix must fail before allocation.
	payload := []byte{1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := Decode(payload, schemaOf(Column{Name: "s", Type: TypeString}), lim); err == nil {
		t.Fatal("accepted huge string length")
	}
}

func TestBoundaryValues(t *testing.T) {
	s := allTypesSchema()
	row := fullRow()
	// extremes for each fixed-width type
	row[0] = Bool(false)
	row[1] = Int8(127)
	row[2] = Int16(32767)
	row[3] = Int32(math.MaxInt32)
	row[4] = Int64(math.MaxInt64)
	row[5] = Uint8(0)
	row[6] = Uint16(0)
	row[7] = Uint32(0)
	row[8] = Uint64(0)
	row[9] = Float32(0)
	row[10] = Float64(-0.0)
	row[11] = String("")
	row[12] = Bytes(nil)
	row[13] = DateValue(0)
	row[14] = TimeValue(0)
	row[15] = DateTimeValueOf(math.MinInt64)
	row[16] = DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: 4})
	enc, err := Encode(s, row, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(enc, s, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	for i := range row {
		if !valuesEqual(row[i], got[i]) {
			t.Errorf("boundary column %d mismatch", i)
		}
	}
	if v, _ := got[11].String(); v != "" {
		t.Fatal("empty string not preserved")
	}
	if v, _ := got[12].Bytes(); len(v) != 0 {
		t.Fatal("empty bytes not preserved")
	}
}

func TestDecimalRoundTrip(t *testing.T) {
	vals := []int64{0, 1, -1, 127, 128, 255, -128, -129, 65535, -65536, 1 << 40, -(1 << 40), math.MaxInt64, math.MinInt64}
	s := schemaOf(Column{Name: "d", Type: TypeDecimal, Scale: 9})
	for _, v := range vals {
		row := []Value{DecimalValue(Decimal{Unscaled: big.NewInt(v), Scale: 9})}
		enc, err := Encode(s, row, DefaultLimits())
		if err != nil {
			t.Fatalf("%d: encode: %v", v, err)
		}
		got, err := Decode(enc, s, DefaultLimits())
		if err != nil {
			t.Fatalf("%d: decode: %v", v, err)
		}
		d, _ := got[0].Decimal()
		if d.Scale != 9 || d.Unscaled.Int64() != v {
			t.Fatalf("%d: got %v", v, d)
		}
	}
}

// TestDecodeIntoReuse verifies DecodeInto is behavior-identical to Decode and
// reuses the caller's slice and Decimal big.Int across calls (V1.1-A).
func TestDecodeIntoReuse(t *testing.T) {
	s := allTypesSchema()
	// Build a few distinct rows covering nulls, strings, decimals.
	rows := [][]Value{
		fullRow(),
		[]Value{
			Bool(false), Int8(0), Int16(0), Int32(0), Int64(0),
			Uint8(0), Uint16(0), Uint32(0), Uint64(0),
			Float32(1.5), Float64(2.5), String(""), Bytes(nil),
			DateValue(0), TimeValue(0), DateTimeValueOf(0),
			DecimalValue(Decimal{Unscaled: big.NewInt(-987654321), Scale: 4}),
		},
		[]Value{
			Null(), Null(), Null(), Null(), Null(), Null(), Null(), Null(), Null(),
			Null(), Null(), Null(), Null(), Null(), Null(), Null(),
			DecimalValue(Decimal{Unscaled: big.NewInt(1 << 60), Scale: 4}),
		},
	}
	var encs [][]byte
	for _, r := range rows {
		enc, err := Encode(s, r, DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		encs = append(encs, enc)
	}

	var dst []Value
	prevDecPtr := map[int]*big.Int{}
	for round := 0; round < 3; round++ {
		for i, enc := range encs {
			out, err := DecodeInto(dst, enc, s, DefaultLimits(), nil)
			if err != nil {
				t.Fatalf("round %d row %d: %v", round, i, err)
			}
			want, err := Decode(enc, s, DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != len(want) {
				t.Fatalf("round %d row %d: len %d", round, i, len(out))
			}
			for c := range want {
				if !valuesEqual(want[c], out[c]) {
					t.Fatalf("round %d row %d col %d: got %#v want %#v", round, i, c, out[c], want[c])
				}
			}
			// Decimal columns must reuse the same big.Int after the first round.
			// (Decimal() returns a copy, so compare the internal pointer.)
			cell := out[len(s.Columns)-1]
			if cell.typ != TypeDecimal || cell.d.Unscaled == nil {
				t.Fatalf("round %d row %d: last col is not decimal", round, i)
			}
			if round == 0 {
				prevDecPtr[i] = cell.d.Unscaled
			} else if cell.d.Unscaled != prevDecPtr[i] {
				t.Fatalf("round %d row %d: decimal big.Int not reused", round, i)
			}
			dst = out[:0]
		}
	}
}
