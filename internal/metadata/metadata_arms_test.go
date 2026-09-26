package metadata

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
)

// metadata_arms_test.go 覆盖 metadata TLV 的几条拒绝臂:值编码不了的字段、重复而未标
// Repeated 的字段、以及三条「信封自洽、字段区不自洽」的解码输入(字段长度超出区域、字段
// 数与区域长度不符、字段 ID 乱序)。前两条在写入前就挡住,后三条是 CRC 正确但语义不成立
// ——只能靠重算 CRC 之后的手工篡改来构造。
//
// 到不了的六条:
//   - field.go 106:encodeField 里的 encodeFieldValue 失败——它与前面的 fieldValueLen 判定
//     完全等价,后者通过之后前者不可能失败。
//   - record.go 86:Encode 循环里的 encodeField 失败——fieldsBytes 已经对每个字段跑过同样的
//     fieldValueLen,成功之后再编码不会失败。
//   - payload.go 150、154:目录 truncated 与条目 Unmarshal 失败——Unmarshal 已把
//     DirectoryBytes 钉成 ItemCount*DirectoryEntrySize,而 base 又已被 137 行检查过,每个条目
//     恒有完整的一条目可读,Unmarshal 只查长度也因此不会失败。
//   - payload.go 196、210:Build 里两处 MarshalTo 失败——写入的目标是刚按精确长度分配的。
//   - record.go 164:ensureSingle 失败——字段 ID 必非递减(153 行先查),同 ID 必相邻;重复而
//     未标 Repeated 的在 153 行就被拦下,标了 Repeated 的根本不进 ensureSingle。

// fieldsStart is where the field region of the sample record begins: the
// envelope header plus its namespace and external key.
const fieldsStart = RecordEnvelopeHeaderSize + len("rowpack.meta.v1") + len("ext")

// tampered re-stamps the record CRC over a mutated envelope, so the failure
// can only come from the field region itself.
func tampered(t *testing.T, mutate func([]byte)) []byte {
	t.Helper()
	rec := sampleRecord()
	enc, err := rec.Encode(nil)
	require.NoError(t, err)
	bad := append([]byte(nil), enc...)
	mutate(bad)
	fixCRC(bad)
	return bad
}

// TestFieldValueRejectsUnsupportedWireType: a reserved wire type is not
// encodable, and the failure is the typed sentinel (125).
func TestFieldValueRejectsUnsupportedWireType(t *testing.T) {
	_, err := encodeFieldValue(&Field{ID: 7, WireType: format.WireObjectRef})
	require.ErrorIs(t, err, errUnsupportedWireType)
}

// TestRecordEncodeRejectsUnencodableField: an unencodable field is reported by
// Encode before any bytes are produced — no half-written record escapes
// (field.go 39, record.go 60).
func TestRecordEncodeRejectsUnencodableField(t *testing.T) {
	rec := sampleRecord()
	rec.Fields = []Field{{ID: 50, WireType: format.WireObjectRef, Value: "x"}}

	_, err := rec.Encode(nil)
	require.ErrorIs(t, err, errUnsupportedWireType)
}

// TestRecordEncodeRejectsRepeatedFieldWithoutFlag: two fields sharing an ID
// must say so, otherwise a reader cannot tell a repetition from corruption
// (45).
func TestRecordEncodeRejectsRepeatedFieldWithoutFlag(t *testing.T) {
	rec := sampleRecord()
	rec.Fields = []Field{
		{ID: 1, WireType: format.WireString, Value: "a"},
		{ID: 1, WireType: format.WireString, Value: "b"},
	}

	_, err := rec.Encode(nil)
	require.ErrorContains(t, err, "repeated without Repeated flag")
}

// TestRecordDecodeRejectsOversizedFieldValue: a field header claiming more
// value bytes than the field region holds is corruption, even though the
// record CRC was recomputed over it (144).
func TestRecordDecodeRejectsOversizedFieldValue(t *testing.T) {
	bad := tampered(t, func(b []byte) {
		binary.LittleEndian.PutUint32(b[fieldsStart+4:], 1<<20)
	})

	var got Record
	require.ErrorContains(t, got.Decode(bad, nil), "value length")
}

// TestRecordDecodeRejectsFieldCountMismatch: a record that claims no fields
// while the field region still holds two ends its walk short of the region
// (150).
func TestRecordDecodeRejectsFieldCountMismatch(t *testing.T) {
	bad := tampered(t, func(b []byte) {
		binary.LittleEndian.PutUint32(b[40:], 0) // field count
	})

	var got Record
	require.ErrorContains(t, got.Decode(bad, nil), "fields end at")
}

// TestRecordDecodeRejectsUnorderedFields: canonical order is part of the
// encoding, so a record whose first field jumps past the second is rejected
// after parsing, not silently reordered (153).
func TestRecordDecodeRejectsUnorderedFields(t *testing.T) {
	bad := tampered(t, func(b []byte) {
		binary.LittleEndian.PutUint16(b[fieldsStart:], 99)
	})

	var got Record
	require.ErrorContains(t, got.Decode(bad, nil), "not sorted by ID")
}

// TestPayloadHeaderRejectsShortDestination: a header is 32 bytes; a shorter
// destination is a programming error, reported not silently truncated (29).
func TestPayloadHeaderRejectsShortDestination(t *testing.T) {
	h := PayloadHeader{ItemCount: 1, DirectoryBytes: DirectoryEntrySize}

	require.Error(t, h.MarshalTo(make([]byte, PayloadHeaderSize-1)))
}

// TestPayloadRejectsRecordsLongerThanPayload: a payload whose records claim
// more bytes than it carries is corrupt, and it is rejected before the
// directory is walked (141).
func TestPayloadRejectsRecordsLongerThanPayload(t *testing.T) {
	h := PayloadHeader{RecordsBytes: 1 << 20}
	data := make([]byte, PayloadHeaderSize)
	require.NoError(t, h.MarshalTo(data))

	_, err := Parse(data)
	require.ErrorContains(t, err, "records exceed payload")
}
