package codec

import (
	"testing"
)

// Decoders must never panic on arbitrary input.

func seedDecode(f *testing.F, valid []byte) {
	f.Add([]byte(nil))
	f.Add(valid)
	f.Add(valid[:len(valid)/2])
	f.Add([]byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB, 0xFA})
	f.Add(make([]byte, 128))
}

func FuzzTupleDecode(f *testing.F) {
	s := allTypesSchema()
	row := fullRow()
	valid, err := Encode(s, row, DefaultLimits())
	if err != nil {
		panic(err)
	}
	seedDecode(f, valid)
	short := schemaOf(Column{Name: "i64", Type: TypeInt64}, Column{Name: "s", Type: TypeString})
	valid2, _ := Encode(short, []Value{Int64(7), String("x")}, DefaultLimits())
	seedDecode(f, valid2)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Decode(data, s, DefaultLimits())
		_, _ = Decode(data, short, DefaultLimits())
	})
}

func FuzzDecimalBytes(f *testing.F) {
	f.Add([]byte{0x00})
	f.Add([]byte{0x7f})
	f.Add([]byte{0x80})
	f.Add([]byte{0xff})
	f.Add([]byte{0x00, 0x80})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 || len(data) > 64 {
			return
		}
		_, _ = decodeDecimalBytes(data)
	})
}
