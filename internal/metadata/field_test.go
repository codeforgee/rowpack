package metadata

import (
	"encoding/binary"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func TestSintWidth(t *testing.T) {
	cases := []struct {
		v    int64
		want int
	}{
		{0, 1}, {127, 1}, {-128, 1},
		{128, 2}, {32767, 2}, {-32768, 2},
		{32768, 4}, {-2147483648, 4},
		{2147483648, 8}, {-2147483649, 8},
	}
	for _, c := range cases {
		if got := sintWidth(c.v); got != c.want {
			t.Errorf("sintWidth(%d) = %d, want %d", c.v, got, c.want)
		}
	}
}

func TestFieldEncodeDecodeSint(t *testing.T) {
	for _, v := range []int64{0, 127, 128, -128, -32768, 32768, 2147483648, -2147483649, -882575889879} {
		f := Field{ID: 7, WireType: fileformat.WireSint, Value: v}
		enc, err := encodeField(nil, &f)
		if err != nil {
			t.Fatalf("encodeField: %v", err)
		}
		got, n, err := decodeField(enc)
		if err != nil {
			t.Fatalf("decodeField: %v", err)
		}
		if n != len(enc) {
			t.Fatalf("consumed %d, want %d", n, len(enc))
		}
		if got.ID != 7 || got.Value != v {
			t.Fatalf("roundtrip mismatch: got %+v, want id=7 value=%d", got, v)
		}
	}
}

func TestFieldEncodeDecodeString(t *testing.T) {
	f := Field{ID: 3, WireType: fileformat.WireString, Value: "hello"}
	enc, err := encodeField(nil, &f)
	if err != nil {
		t.Fatalf("encodeField: %v", err)
	}
	got, _, err := decodeField(enc)
	if err != nil {
		t.Fatalf("decodeField: %v", err)
	}
	if got.Value != "hello" {
		t.Fatalf("value %v, want hello", got.Value)
	}
}

func TestFieldFlagsRoundtrip(t *testing.T) {
	f := Field{ID: 9, WireType: fileformat.WireString, Value: "x", Critical: true, Repeated: true}
	enc, err := encodeField(nil, &f)
	if err != nil {
		t.Fatalf("encodeField: %v", err)
	}
	if flags := enc[3]; flags&fileformat.FieldFlagCritical == 0 || flags&fileformat.FieldFlagRepeated == 0 {
		t.Fatalf("flags byte %x did not carry critical+repeated", flags)
	}
	got, _, err := decodeField(enc)
	if err != nil {
		t.Fatalf("decodeField: %v", err)
	}
	if !got.Critical || !got.Repeated {
		t.Fatalf("flags lost on decode: %+v", got)
	}
}

func TestFieldErrorPaths(t *testing.T) {
	if _, err := fieldValueLen(&Field{ID: 1}); err == nil {
		t.Fatal("zero WireType should error")
	}
	if _, err := fieldValueLen(&Field{ID: 1, WireType: fileformat.WireSint, Value: "not-int"}); err == nil {
		t.Fatal("WireSint with string value should error")
	}
	if _, err := fieldValueLen(&Field{ID: 2, WireType: fileformat.WireString, Value: 42}); err == nil {
		t.Fatal("WireString with int value should error")
	}
	if _, err := fieldEncodedLen(&Field{WireType: fileformat.WireObjectRef, Value: nil}); err == nil {
		t.Fatal("unsupported wire type should error")
	}
	// encodeFieldValue is only reached after fieldValueLen validation in
	// encodeField; the safe path must reject wrong-typed values.
	if _, err := encodeField(nil, &Field{ID: 1, WireType: fileformat.WireSint, Value: nil}); err == nil {
		t.Fatal("encodeField nil WireSint value should error")
	}

	if _, _, err := decodeField(nil); err == nil {
		t.Fatal("decodeField nil should error")
	}
	short := make([]byte, 4)
	if _, _, err := decodeField(short); err == nil {
		t.Fatal("decodeField short header should error")
	}

	// value length exceeds input
	f := Field{ID: 1, WireType: fileformat.WireString, Value: "abc"}
	enc, err := encodeField(nil, &f)
	if err != nil {
		t.Fatalf("encodeField: %v", err)
	}
	binary.LittleEndian.PutUint32(enc[4:], 999)
	if _, _, err := decodeField(enc); err == nil {
		t.Fatal("oversized value length should error")
	}

	// unsupported wire type on critical field: craft a header directly, since
	// encodeField refuses to encode an unsupported wire type.
	benc := make([]byte, 8)
	binary.LittleEndian.PutUint16(benc[0:], 1)
	benc[2] = byte(fileformat.WireObjectRef)
	benc[3] = fileformat.FieldFlagCritical
	binary.LittleEndian.PutUint32(benc[4:], 0)
	if _, _, err := decodeField(benc); err == nil {
		t.Fatal("unsupported wire type on critical field should error")
	}

	// unsupported wire type on non-critical field keeps raw and nil value
	noncrit := Field{ID: 1, WireType: fileformat.WireObjectRef, Critical: false, raw: []byte{1, 2, 3}}
	nenc, _ := encodeField(nil, &noncrit)
	got, _, err := decodeField(nenc)
	if err != nil {
		t.Fatalf("decodeField non-critical passthrough: %v", err)
	}
	if got.Value != nil {
		t.Fatalf("non-critical unsupported field value %v, want nil", got.Value)
	}
	if string(got.raw) != "\x01\x02\x03" {
		t.Fatalf("raw not preserved: %q", got.raw)
	}

	// fieldValueLen uses raw when present
	if n, err := fieldValueLen(&Field{raw: []byte{0, 0, 0, 7}}); err != nil || n != 4 {
		t.Fatalf("fieldValueLen raw = %d, %v", n, err)
	}
}

func TestSintWrongLength(t *testing.T) {
	// A 6-byte WireSint value length is not a valid sint width.
	enc := make([]byte, 8+6)
	binary.LittleEndian.PutUint16(enc[0:], 1)
	enc[2] = byte(fileformat.WireSint)
	binary.LittleEndian.PutUint32(enc[4:], 6)
	if _, _, err := decodeField(enc); err == nil {
		t.Fatal("6-byte WireSint should error")
	}
}

func TestFieldsBytesAndCanonical(t *testing.T) {
	fs := []Field{
		{ID: 1, WireType: fileformat.WireSint, Value: int64(5)},
		{ID: 2, WireType: fileformat.WireString, Value: "abc"},
	}
	n, err := fieldsBytes(fs)
	if err != nil {
		t.Fatalf("fieldsBytes: %v", err)
	}
	if n != 8+1+8+3 {
		t.Fatalf("fieldsBytes = %d, want %d", n, 8+1+8+3)
	}

	if err := checkCanonical([]Field{{ID: 2}, {ID: 1}}); err == nil {
		t.Fatal("unsorted fields should error")
	}
	if err := checkCanonical([]Field{{ID: 1}, {ID: 1}}); err == nil {
		t.Fatal("repeated non-flagged field should error")
	}
	if err := checkCanonical([]Field{{ID: 1, Repeated: true}, {ID: 1, Repeated: true}}); err != nil {
		t.Fatalf("flagged repeated field should pass: %v", err)
	}
	if err := checkCanonical(nil); err != nil {
		t.Fatalf("empty fields should pass: %v", err)
	}
}

func TestAppendHelpers(t *testing.T) {
	if got := appendU16(nil, 0x1234); len(got) != 2 || got[0] != 0x34 || got[1] != 0x12 {
		t.Fatalf("appendU16 wrong: %x", got)
	}
	if got := appendU32(nil, 0x12345678); len(got) != 4 || got[3] != 0x12 {
		t.Fatalf("appendU32 wrong: %x", got)
	}
	if got := appendU64(nil, 0x0102030405060708); len(got) != 8 || got[7] != 0x01 {
		t.Fatalf("appendU64 wrong: %x", got)
	}
}
