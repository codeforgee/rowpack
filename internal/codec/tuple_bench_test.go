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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
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
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decoder.DecodeBatchInto(slab[:0], bodies, nil); err != nil {
			b.Fatal(err)
		}
	}
}
