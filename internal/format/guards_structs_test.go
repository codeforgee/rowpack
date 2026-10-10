package format

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Rows Block 容器头 / 页目录项 / 块头的输入边界。这两个结构是「先按定长解析、再按
// 交叉校验拒绝」的入口：容器头的 PageCount×56 == DirectoryBytes 交叉校验如果放水，
// 下游 parsePageDir 就会按被夸大的目录长度去读字节。这里把每条拒绝路径与每条
// 序列化往返钉死。

func TestRowsBlockHeaderGuards(t *testing.T) {
	h := RowsBlockHeader{PageCount: 3, DirectoryBytes: 3 * MinRowsPageDirEntrySize, TotalRecords: 17}
	var buf [RowsBlockHeaderSize]byte
	err := h.MarshalTo(buf[:])
	require.NoError(t, err, "marshal")
	var got RowsBlockHeader
	err = got.Unmarshal(buf[:])
	require.NoError(t, err, "unmarshal")
	require.Equal(t, h, got, "round trip = %+v, want %+v", got, h)
	if got.Size() != RowsBlockHeaderSize {
		t.Fatalf("Size() = %d, want %d", got.Size(), RowsBlockHeaderSize)
	}
	// Reserved 必须回写为零。
	for i := 10; i < 12; i++ {
		require.EqualValues(t, 0, buf[i], "reserved byte %d = %d, want 0", i, buf[i])
	}
	// MarshalTo 先清零整个目标：脏缓冲区不得留下残字节。
	dirty := make([]byte, RowsBlockHeaderSize)
	for i := range dirty {
		dirty[i] = 0xFF
	}
	err = h.MarshalTo(dirty)
	require.NoError(t, err, "marshal into dirty buffer")
	if dirty[9] != 0 || dirty[10] != 0 || dirty[11] != 0 {
		t.Fatalf("padding bytes not zeroed: %v", dirty[:12])
	}

	cases := []struct {
		name   string
		mutate func(*RowsBlockHeader, *[RowsBlockHeaderSize]byte)
		want   string
	}{
		{"bad-magic", func(_ *RowsBlockHeader, b *[RowsBlockHeaderSize]byte) { b[0] = 'X' }, "magic"},
		{"bad-version", func(_ *RowsBlockHeader, b *[RowsBlockHeaderSize]byte) { b[8] = 2 }, "version"},
		{"reserved-nonzero", func(_ *RowsBlockHeader, b *[RowsBlockHeaderSize]byte) { b[10] = 1 }, "reserved"},
		{"reserved-nonzero-high", func(_ *RowsBlockHeader, b *[RowsBlockHeaderSize]byte) { b[11] = 9 }, "reserved"},
		{"dir-bytes-mismatch", func(_ *RowsBlockHeader, b *[RowsBlockHeaderSize]byte) {
			// 3 页的合法区间是 [3×8, 3×80]；0 落在下界之下。
			binary.LittleEndian.PutUint32(b[16:], 0)
		}, "directory bytes"},
		{"pagecount-overflow", func(_ *RowsBlockHeader, b *[RowsBlockHeaderSize]byte) {
			binary.LittleEndian.PutUint32(b[12:], 0xFFFFFFFF)
			binary.LittleEndian.PutUint32(b[16:], 0xFFFFFFFF) // 与溢出后的乘积都不符
		}, "directory bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var src [RowsBlockHeaderSize]byte
			err := h.MarshalTo(src[:])
			require.NoError(t, err, "marshal fixture")
			var hh RowsBlockHeader
			tc.mutate(&hh, &src)
			var out RowsBlockHeader
			err = out.Unmarshal(src[:])
			require.NotNil(t, err, "accepted forged header %v", src[:])
			require.Contains(t, err.Error(), tc.want, "err = %v, want diagnosis %q", err, tc.want)
		})
	}

	// 短输入：不得越界读。
	for n := range RowsBlockHeaderSize {
		var out RowsBlockHeader
		if err := out.Unmarshal(make([]byte, n)); err == nil {
			t.Fatalf("Unmarshal of %d bytes must fail", n)
		}
	}
	// MarshalTo 的 dst 太短同样拒绝（写侧也不能越界写）。
	if err := h.MarshalTo(make([]byte, RowsBlockHeaderSize-1)); err == nil ||
		!strings.Contains(err.Error(), "destination too short") {
		t.Fatalf("short destination = %v", err)
	}
	// MarshalTo 自己拒绝越界的目录长度：4 页至少要有 4×MinRowsPageDirEntrySize
	// 字节，条目是变长的，所以校验的是区间而非精确值。
	bad := RowsBlockHeader{PageCount: 4, DirectoryBytes: 4*MinRowsPageDirEntrySize - 1}
	if err := bad.MarshalTo(make([]byte, RowsBlockHeaderSize)); err == nil ||
		!strings.Contains(err.Error(), "directory bytes") {
		t.Fatalf("inconsistent DirectoryBytes = %v", err)
	}
	require.Error(t, (&RowsBlockHeader{PageCount: 1 << 24, DirectoryBytes: 0}).MarshalTo(make([]byte, RowsBlockHeaderSize)), "PageCount whose directory bytes overflow must be rejected on the write side")
}

// TestRowsPageDirEntryRoundtripAndRejects pins the varint directory entry:
// it round-trips, reports exactly the bytes it consumed, rejects every
// truncation, and refuses a varint that would silently truncate into a uint32
// field.
func TestRowsPageDirEntryRoundtripAndRejects(t *testing.T) {
	e := RowsPageDirEntry{
		PageOrdinal: 2, FirstRecordOrdinal: 40, RecordCount: 20,
		StoredOffset: 1 << 40, StoredSize: 4096, RawSize: 8192,
		MinRowID: 7, MaxRowID: ^uint64(0), Flags: 1,
	}
	encoded := e.AppendTo(nil)
	require.Len(t, encoded, e.EncodedLen(), "EncodedLen must match what AppendTo writes")

	// StoredOffset is recomputed by the directory parser from the preceding
	// sizes, so it is deliberately not part of the encoding.
	want := e
	want.StoredOffset = 0
	var got RowsPageDirEntry
	n, err := got.Unmarshal(encoded)
	require.NoError(t, err, "unmarshal")
	require.Equal(t, want, got, "round trip = %+v, want %+v", got, want)
	require.Equal(t, len(encoded), n, "Unmarshal must report every byte it consumed")

	// Every proper prefix is a truncation and must fail.
	for i := range len(encoded) {
		var out RowsPageDirEntry
		_, err := out.Unmarshal(encoded[:i])
		require.Error(t, err, "Unmarshal of %d/%d bytes must fail", i, len(encoded))
	}
	// A varint wider than 10 bytes cannot be decoded at all.
	var empty RowsPageDirEntry
	_, err = empty.Unmarshal([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	require.Error(t, err, "an over-wide varint must fail")
	// A value that does not fit its uint32 field is rejected rather than
	// silently truncated into a different page geometry.
	over := binary.AppendUvarint(nil, math.MaxUint32+1)
	for range dirEntryFields - 1 {
		over = binary.AppendUvarint(over, 0)
	}
	var wide RowsPageDirEntry
	_, err = wide.Unmarshal(over)
	require.ErrorContains(t, err, "does not fit uint32", "an oversized field must be rejected")
}

func TestBlockHeaderGuards(t *testing.T) {
	h := BlockHeader{
		BlockKind: BlockKindRows, Compression: CompressionZstd, BlockID: 9, SnapshotID: 3,
		TableID: 2, ItemCount: 5, RawSize: 100, StoredSize: 80, RawCRC32C: 12345, KeyEpoch: 7,
	}
	var buf [BlockHeaderSize]byte
	err := h.MarshalTo(buf[:])
	require.NoError(t, err, "marshal")
	var got BlockHeader
	err = got.Unmarshal(buf[:])
	require.NoError(t, err, "unmarshal")
	if got.Encrypted || got.KeyEpoch != 7 {
		t.Fatalf("flags/keyepoch lost: %+v", got)
	}
	// Encrypted 位与 KeyEpoch 各自独立。
	h.Encrypted = true
	err = h.MarshalTo(buf[:])
	require.NoError(t, err, "marshal encrypted")
	if err := got.Unmarshal(buf[:]); err != nil || !got.Encrypted || got.KeyEpoch != 7 {
		t.Fatalf("encrypted round trip = %+v %v", got, err)
	}
	// 块头 CRC 覆盖整块头：任何一字节漂移都要被拒。
	for _, off := range []int{12, 13, 16, 32, 40, 48, 50} {
		bad := buf
		bad[off] ^= 0x01
		var out BlockHeader
		if err := out.Unmarshal(bad[:]); err == nil {
			t.Fatalf("byte %d escaped the header CRC", off)
		}
	}
	require.Error(t, new(BlockHeader).Unmarshal(buf[:BlockHeaderSize-1]), "short input must be rejected")
	badMagic := buf
	badMagic[0] = 'Q'
	if err := new(BlockHeader).Unmarshal(badMagic[:]); err == nil ||
		!strings.Contains(err.Error(), "magic") {
		t.Fatalf("bad magic = %v", err)
	}
	badSize := buf
	binary.LittleEndian.PutUint32(badSize[8:], BlockHeaderSize+8)
	if err := new(BlockHeader).Unmarshal(badSize[:]); err == nil ||
		!strings.Contains(err.Error(), "size") {
		t.Fatalf("bad declared size = %v", err)
	}
	require.Error(t, new(BlockHeader).MarshalTo(nil), "MarshalTo(nil) must fail")
}

func TestIsVersionErrorClassification(t *testing.T) {
	if IsVersionError(nil) {
		t.Fatal("nil must not classify as a version error")
	}
	if IsVersionError(errors.New("plain")) {
		t.Fatal("a plain error must not classify as a version error")
	}
	if IsVersionError(formatError("X", 1, "bad magic")) {
		t.Fatal("a non-version format error must not classify as a version error")
	}
	ver := formatError("FileHeader", 4, "%s: major %d", errBadVersion, 9)
	if !IsVersionError(ver) {
		t.Fatalf("version error misclassified: %v", ver)
	}
	// 包装后仍可判定（上层普遍用 %w 再包一层）。
	if !IsVersionError(fmt.Errorf("read header: %w", ver)) {
		t.Fatal("wrapped version error must still classify")
	}
}
