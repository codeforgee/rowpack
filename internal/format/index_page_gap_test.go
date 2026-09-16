package format

import "testing"

// RowsPageDirEntry.Unmarshal / RowIndexPageHeader.Unmarshal 的错误分支覆盖
// 偏低，补短输入、magic、版本、保留位、位流几何等校验测试。

func TestRowsPageDirEntryRoundtripAndShort(t *testing.T) {
	e := RowsPageDirEntry{
		PageOrdinal:        2,
		FirstRecordOrdinal: 5,
		RecordCount:        9,
		StoredOffset:       1234,
		StoredSize:         77,
		RawSize:            321,
		MinRowID:           1,
		MaxRowID:           88,
		PageCRC32C:         0xABCDEF,
		Flags:              1,
	}
	var buf [RowsPageDirEntrySize]byte
	if err := e.MarshalTo(buf[:]); err != nil {
		t.Fatal(err)
	}
	var out RowsPageDirEntry
	if err := out.Unmarshal(buf[:]); err != nil {
		t.Fatal(err)
	}
	if out != e {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", out, e)
	}
	for _, n := range []int{0, 1, RowsPageDirEntrySize - 1} {
		if err := out.Unmarshal(buf[:n]); err == nil {
			t.Fatalf("short input %d accepted", n)
		}
	}
}

func TestRowIndexPageHeaderErrors(t *testing.T) {
	h := RowIndexPageHeader{EntryCount: 4, ChangeBitsBytes: 1, TableRunBytes: 2, RowIDBytes: 8, BlockRunBytes: 2, OrdinalBytes: 4, FirstRowID: 3, MinRowID: 1, MaxRowID: 7}
	page := make([]byte, IndexPageHeaderSize)
	if err := h.MarshalTo(page); err != nil {
		t.Fatal(err)
	}
	validLen := IndexPageHeaderSize + int(h.StreamsBytes())
	full := append(append([]byte{}, page...), make([]byte, h.StreamsBytes())...)

	var out RowIndexPageHeader
	if err := out.Unmarshal(full, validLen); err != nil {
		t.Fatalf("valid page rejected: %v", err)
	}
	if out != h {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
	// 几何不符。
	if err := out.Unmarshal(full, validLen+1); err == nil {
		t.Fatal("geometry mismatch accepted")
	}
	cases := []struct {
		name string
		mut  func(b []byte)
	}{
		{"bad magic", func(b []byte) { copy(b, "XXXXXXXX") }},
		{"bad version", func(b []byte) { b[8] = 0xFF }},
		{"reserved nonzero", func(b []byte) { b[9] = 1 }},
		{"entry count zero", func(b []byte) { putU32(b[12:], 0) }},
		{"bits mismatch", func(b []byte) { putU32(b[32:], 9) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cp := append([]byte(nil), full...)
			c.mut(cp)
			if err := out.Unmarshal(cp, validLen); err == nil {
				t.Fatalf("%s accepted", c.name)
			}
		})
	}
	for _, n := range []int{0, 8, IndexPageHeaderSize - 1} {
		if err := out.Unmarshal(full[:n], n); err == nil {
			t.Fatalf("short input %d accepted", n)
		}
	}
}
