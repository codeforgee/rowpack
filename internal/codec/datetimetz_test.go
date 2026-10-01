package codec

import (
	"testing"
	"time"
)

func TestDateTimeTZRoundsTrip(t *testing.T) {
	orig := time.Date(2026, 8, 6, 15, 4, 5, 123456789, time.FixedZone("+08", 8*3600))
	v := DateTimeTZ(orig)
	if v.Type() != TypeDateTimeTZ {
		t.Fatalf("type = %d, want %d", v.Type(), TypeDateTimeTZ)
	}
	schema := &Schema{Name: "t", Columns: []Column{{Name: "ts", Type: TypeDateTimeTZ, Nullable: true}}}
	c := Codec{Limits: DefaultLimits()}
	if err := schema.Validate(c.Limits); err != nil {
		t.Fatal(err)
	}
	row := []Value{v}
	encoded, err := c.EncodeInto(schema, row, nil)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := c.CompileDecoder(schema)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := dec.DecodeInto(nil, encoded, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := decoded[0].DateTimeTZValue()
	if !ok {
		t.Fatal("accessor invalid")
	}
	if !got.Equal(orig) {
		t.Fatalf("instant changed: %v -> %v", orig, got)
	}
	_, off := got.Zone()
	if off != 8*3600 {
		t.Fatalf("offset = %d, want 28800", off)
	}
	if got.Nanosecond() != orig.Nanosecond() {
		t.Fatalf("sub-second precision lost: %d != %d", got.Nanosecond(), orig.Nanosecond())
	}
}

// MySQL DATETIME spans years 1000..9999; UnixNano overflows outside
// 1678..2262, so the stored form must be a seconds/nanoseconds split.
func TestDateTimeExtremeYears(t *testing.T) {
	c := Codec{Limits: DefaultLimits()}
	for _, when := range []time.Time{
		time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(1816, 3, 29, 5, 56, 8, 66276000, time.UTC),
		time.Date(2262, 4, 11, 23, 47, 16, 854775807, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
	} {
		schema := &Schema{Name: "t", Columns: []Column{{Name: "ts", Type: TypeDateTime, Nullable: true}}}
		if err := schema.Validate(c.Limits); err != nil {
			t.Fatal(err)
		}
		encoded, err := c.EncodeInto(schema, []Value{DateTime(when)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		dec, err := c.CompileDecoder(schema)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := dec.DecodeInto(nil, encoded, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := decoded[0].DateTimeValue()
		if !ok {
			t.Fatal("accessor invalid")
		}
		if !got.Equal(when) {
			t.Fatalf("%v round-tripped as %v", when, got)
		}
	}
}
