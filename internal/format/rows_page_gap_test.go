package format

import (
	"testing"
)

// RowsPageHeader 三件套在 format 包内是 0% 覆盖（实际经 block/codec 包间接
// 执行，但跨包调用不计入本包覆盖）。这些是短输入/几何越界的第一道校验，
// 补直接单测锁住 roundtrip 与错误路径。

func TestRowsPageHeaderRoundtrip(t *testing.T) {
	h := RowsPageHeader{
		EntryCount:      7,
		RowIDsBytes:     10,
		OffsetsBytes:    9,
		SchemaRLEBytes:  4,
		ChangeBitsBytes: 2, // (7+3)/4
		TuplesBytes:     123,
		FirstRowID:      42,
		MinRowID:        1,
		MaxRowID:        99,
		CRC32C:          0xDEADBEEF,
	}
	// 完整页 = 64B 头 + 各流字节区；几何校验要求 totalLen == 64+Σ流长。
	page := make([]byte, 0, RowsPageHeaderSize+int(h.StreamsBytes()))
	var hb [RowsPageHeaderSize]byte
	if err := h.MarshalTo(hb[:]); err != nil {
		t.Fatal(err)
	}
	page = append(page, hb[:]...)
	page = append(page, make([]byte, h.RowIDsBytes)...)
	page = append(page, make([]byte, h.OffsetsBytes)...)
	page = append(page, make([]byte, h.SchemaRLEBytes)...)
	page = append(page, make([]byte, h.ChangeBitsBytes)...)
	page = append(page, make([]byte, h.TuplesBytes)...)
	if got := h.StreamsBytes(); got != 10+9+4+2+123 {
		t.Fatalf("StreamsBytes = %d", got)
	}
	var out RowsPageHeader
	if err := out.Unmarshal(page, len(page)); err != nil {
		t.Fatal(err)
	}
	if out != h {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", out, h)
	}
	// totalLen 与几何不符必须被拒绝。
	if err := out.Unmarshal(page, len(page)+1); err == nil {
		t.Fatal("geometry mismatch accepted")
	}
	if err := out.Unmarshal(page, 0); err != nil { // 跳过几何检查的调用形式
		t.Fatalf("totalLen=0 should skip geometry: %v", err)
	}
}

func TestRowsPageHeaderUnmarshalErrors(t *testing.T) {
	var h RowsPageHeader
	h.EntryCount = 1
	h.ChangeBitsBytes = 1
	var buf [RowsPageHeaderSize]byte
	if err := h.MarshalTo(buf[:]); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mut  func(b []byte)
	}{
		{"short input", func(b []byte) {}},
		{"bad magic", func(b []byte) { copy(b, "XXXXXXXX") }},
		{"bad version", func(b []byte) { b[8] = 9 }},
		{"reserved nonzero", func(b []byte) { b[9] = 1 }},
		{"bits stream mismatch", func(b []byte) { putU32(b[28:], 5) }}, // 1 entry needs 1 byte
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp := append([]byte(nil), buf[:]...)
			if c.name == "short input" {
				cp = cp[:10]
			} else {
				c.mut(cp)
			}
			var out RowsPageHeader
			if err := out.Unmarshal(cp, len(cp)); err == nil {
				t.Fatalf("%s: nil error", c.name)
			}
		})
	}
}

func TestPackUnpackChangeType(t *testing.T) {
	for ct, packed := range map[ChangeType]uint8{
		ChangeInsert: 0,
		ChangeUpdate: 1,
		ChangeDelete: 2,
	} {
		p, err := PackChangeType(ct)
		if err != nil {
			t.Fatalf("pack %d: %v", ct, err)
		}
		if p != packed {
			t.Fatalf("pack %d = %d, want %d", ct, p, packed)
		}
		got, err := UnpackChangeType(p)
		if err != nil {
			t.Fatalf("unpack %d: %v", p, err)
		}
		if got != ct {
			t.Fatalf("unpack %d = %d, want %d", p, got, ct)
		}
	}
	if _, err := PackChangeType(0); err == nil {
		t.Fatal("PackChangeType(0) accepted")
	}
	if _, err := PackChangeType(99); err == nil {
		t.Fatal("PackChangeType(99) accepted")
	}
	if _, err := UnpackChangeType(3); err == nil {
		t.Fatal("reserved packed value 3 accepted")
	}
}
