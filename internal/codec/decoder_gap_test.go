package codec

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Decoder.Columns 本包覆盖为 0%（ReadBatch 在根包调用）。补一行语义测试。
func TestDecoderColumnsGetter(t *testing.T) {
	schema := &Schema{Name: "t", Columns: []Column{{Name: "a", Type: TypeInt64}, {Name: "b", Type: TypeString}}}
	d, err := (Codec{Limits: DefaultLimits()}.CompileDecoder(schema))
	if err != nil {
		t.Fatal(err)
	}
	require.EqualValues(t, 2, d.Columns(), "Columns = %d, want 2", d.Columns())
	// String 是长度前缀列 => 非 fully-fixed；fixedBytes 只累计定宽列。
	if d.fixed || d.fixedBytes != 8 {
		t.Fatalf("fixed=%v fixedBytes=%d", d.fixed, d.fixedBytes)
	}
	// 全定宽、非空 schema 才是 fully-fixed。
	schema2 := &Schema{Name: "t2", Columns: []Column{{Name: "a", Type: TypeInt64}, {Name: "b", Type: TypeInt32}}}
	d2, err := (Codec{Limits: DefaultLimits()}.CompileDecoder(schema2))
	if err != nil {
		t.Fatal(err)
	}
	if !d2.fixed || d2.fixedBytes != 12 {
		t.Fatalf("fixed=%v fixedBytes=%d", d2.fixed, d2.fixedBytes)
	}
}
