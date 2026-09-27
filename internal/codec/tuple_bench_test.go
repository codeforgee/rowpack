package codec

import "testing"

func benchmarkDecodeFixture(b *testing.B) (Codec, *Schema, []byte) {
	b.Helper()
	c := Codec{Limits: DefaultLimits()}
	schema := &Schema{Name: "fixed", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "i", Type: TypeInt64},
		{Name: "u", Type: TypeUint32},
		{Name: "f", Type: TypeFloat64},
		{Name: "date", Type: TypeDate},
		{Name: "time", Type: TypeTime},
		{Name: "last", Type: TypeInt64},
	}}
	row := []Value{Uint64(42), Int64(-7), Uint32(9), Float64(1.25),
		DateValue(Date(20_000)), TimeValue(TimeOfDay(123)), Int64(99)}
	body, err := c.EncodeInto(schema, row, nil)
	if err != nil {
		b.Fatal(err)
	}
	return c, schema, body
}

func BenchmarkPreparedDecodeBody(b *testing.B) {
	c, schema, body := benchmarkDecodeFixture(b)
	decoder, err := c.CompileDecoder(schema)
	if err != nil {
		b.Fatal(err)
	}
	dst := make([]Value, len(schema.Columns))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decoder.DecodeInto(dst, body, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPreparedDecodeBatch128(b *testing.B) {
	c, schema, body := benchmarkDecodeFixture(b)
	decoder, err := c.CompileDecoder(schema)
	if err != nil {
		b.Fatal(err)
	}
	bodies := make([][]byte, 128)
	for i := range bodies {
		bodies[i] = body
	}
	slab := make([]Value, 0, len(bodies)*len(schema.Columns))
	b.ReportAllocs()
	b.SetBytes(int64(len(body) * len(bodies)))
	for b.Loop() {
		if _, err := decoder.DecodeBatchInto(slab[:0], bodies, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// benchmarkMixedFixture is the mixed-type, fully non-nullable schema that
// mirrors the store's read benchmarks (fixed columns plus String/Bytes).
func benchmarkMixedFixture(b *testing.B, nullable bool) (Codec, *Schema, []byte) {
	b.Helper()
	c := Codec{Limits: DefaultLimits()}
	schema := &Schema{Name: "mixed", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "a", Type: TypeInt64, Nullable: nullable},
		{Name: "b", Type: TypeInt64},
		{Name: "c", Type: TypeFloat64},
		{Name: "s", Type: TypeString, Nullable: nullable},
		{Name: "t", Type: TypeDateTime},
		{Name: "by", Type: TypeBytes},
	}}
	row := []Value{Uint64(42), Int64(-7), Int64(9), Float64(1.25),
		String("row-00000123-payload"), Value{typ: TypeDateTime, i: 1_700_000_000_000_000_000}, Bytes([]byte{1, 2, 3, 4})}
	body, err := c.EncodeInto(schema, row, nil)
	if err != nil {
		b.Fatal(err)
	}
	return c, schema, body
}

func BenchmarkPreparedDecodeMixed(b *testing.B) {
	for _, nullable := range []bool{false, true} {
		name := "nonnull"
		if nullable {
			name = "nullable"
		}
		c, schema, body := benchmarkMixedFixture(b, nullable)
		decoder, err := c.CompileDecoder(schema)
		if err != nil {
			b.Fatal(err)
		}
		dst := make([]Value, len(schema.Columns))
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := decoder.DecodeInto(dst, body, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
