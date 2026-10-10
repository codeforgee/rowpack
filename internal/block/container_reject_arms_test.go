package block

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
)

// container_reject_arms_test.go 覆盖 Rows 容器解析的拒绝臂:块头和目录是明文、也只被
// 块头 CRC 覆盖,所以「几何自洽、页已损坏」与「目录长度对不上」是两类不同的输入,各自
// 有不同的出口——前者要等到真的去读那一页才报错,后者在进入时就该被挡住。
//
// 目录条目是变长的,所以「目录长度正好等于 N 个条目」不再由容器头钉死:parsePageDir
// 的 truncated 与条目 Unmarshal 失败两条分支现在都可达,下面各有一条用例覆盖。

// encodeDir serializes page directory entries the way the container stores
// them: back to back, varint-encoded, with no padding.
func encodeDir(entries ...format.RowsPageDirEntry) []byte {
	var b []byte
	for i := range entries {
		b = entries[i].AppendTo(b)
	}
	return b
}

// TestParseContainerRejectsShortContainer: a block whose stored size matches
// but that cannot even hold a container header is rejected before any offset
// arithmetic happens (131).
func TestParseContainerRejectsShortContainer(t *testing.T) {
	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindRows, StoredSize: 8}

	_, err := ParseContainer(make([]byte, 8), h, DefaultLimits())
	require.ErrorContains(t, err, "too short for header")
}

// TestParseRowsDirRejectsNonRowsBlock: the lazy entry point checks the kind
// first — a metadata block has no page directory to read (166).
func TestParseRowsDirRejectsNonRowsBlock(t *testing.T) {
	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindMetadata, StoredSize: 64}

	_, err := ParseRowsDir(0, NewReader(&mockReaderAt{}, DefaultLimits()), h, DefaultLimits())
	require.ErrorContains(t, err, "not rows")
}

// TestParseRowsDirRejectsDirectoryGeometry: the container header must agree
// with the block it sits in — a directory claiming a thousand pages cannot
// live in a 1 KiB block (217).
func TestParseRowsDirRejectsDirectoryGeometry(t *testing.T) {
	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindRows, ItemCount: 1, StoredSize: 1 << 10}
	rh := format.RowsBlockHeader{
		PageCount: 1000,
		// Entries are varint-encoded, but even the smallest possible one is
		// MinRowsPageDirEntrySize: 1000 of them cannot fit a 1 KiB block.
		DirectoryBytes: 1000 * format.MinRowsPageDirEntrySize,
		TotalRecords:   1,
	}

	data := make([]byte, format.BlockHeaderSize+format.RowsBlockHeaderSize)
	require.NoError(t, h.MarshalTo(data))
	require.NoError(t, rh.MarshalTo(data[format.BlockHeaderSize:]))

	_, err := ParseRowsDir(0, NewReader(&mockReaderAt{data: data}, DefaultLimits()), h, DefaultLimits())
	require.ErrorContains(t, err, "overruns stored", "the directory cannot fit the block that carries it")
}

// TestParseRowsDirRejectsUnreadableDirectory: the directory is read in one
// ReadAt; a file that ends inside it is an I/O failure, not a corrupt block
// (193).
func TestParseRowsDirRejectsUnreadableDirectory(t *testing.T) {
	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindRows, ItemCount: 1, StoredSize: 1 << 20}
	// Two minimal entries, so the directory ends 16 bytes past the container
	// header — and the file stops right after that header.
	rh := format.RowsBlockHeader{PageCount: 2, DirectoryBytes: 2 * format.MinRowsPageDirEntrySize, TotalRecords: 1}

	data := make([]byte, format.BlockHeaderSize+format.RowsBlockHeaderSize)
	require.NoError(t, h.MarshalTo(data))
	require.NoError(t, rh.MarshalTo(data[format.BlockHeaderSize:]))

	_, err := ParseRowsDir(0, NewReader(&mockReaderAt{data: data}, DefaultLimits()), h, DefaultLimits())
	require.ErrorContains(t, err, "read container directory")
}

// TestParseRowsDirRejectsPageBounds: the directory is validated before any
// page is read — a page whose stored and raw sizes disagree under none
// compression is a corrupt directory, not a slow page (205).
func TestParseRowsDirRejectsPageBounds(t *testing.T) {
	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindRows, ItemCount: 1, StoredSize: 1 << 10}
	// None-compressed pages are stored == raw; this one claims otherwise.
	dir := format.RowsPageDirEntry{RecordCount: 1, StoredSize: 8, RawSize: 16}
	encoded := encodeDir(dir)
	rh := format.RowsBlockHeader{PageCount: 1, DirectoryBytes: uint32(len(encoded)), TotalRecords: 1}

	data := make([]byte, format.BlockHeaderSize+format.RowsBlockHeaderSize+len(encoded))
	require.NoError(t, rh.MarshalTo(data[format.BlockHeaderSize:]))
	copy(data[format.BlockHeaderSize+format.RowsBlockHeaderSize:], encoded)
	// MarshalTo clears its destination, so the block header is stamped last
	// and copied in.
	h.RawCRC32C = format.CRC32C(data[format.BlockHeaderSize:])
	hdr := make([]byte, format.BlockHeaderSize)
	require.NoError(t, h.MarshalTo(hdr))
	copy(data, hdr)

	_, err := ParseRowsDir(0, NewReader(&mockReaderAt{data: data}, DefaultLimits()), h, DefaultLimits())
	require.ErrorContains(t, err, "stored 8 != raw 16")
}

// TestRecordAtRejectsOrdinalBeyondPageRecords: a directory that claims more
// records than its page carries still adds up geometrically, so the container
// parses; the ordinal only fails inside the page (435).
func TestRecordAtRejectsOrdinalBeyondPageRecords(t *testing.T) {
	page := insertPage(2).encode(t) // two records

	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindRows, ItemCount: 5}
	dir := format.RowsPageDirEntry{
		RecordCount: 5, // claims five, the page holds two
		StoredSize:  uint32(len(page)),
		RawSize:     uint32(len(page)),
	}
	encoded := encodeDir(dir)
	rh := format.RowsBlockHeader{PageCount: 1, DirectoryBytes: uint32(len(encoded)), TotalRecords: 5}

	container := make([]byte, format.RowsBlockHeaderSize+len(encoded)+len(page))
	require.NoError(t, rh.MarshalTo(container))
	copy(container[format.RowsBlockHeaderSize:], encoded)
	copy(container[format.RowsBlockHeaderSize+len(encoded):], page)
	h.StoredSize = uint32(len(container))
	h.RawCRC32C = format.CRC32C(container[:format.RowsBlockHeaderSize+len(encoded)])

	rc, err := ParseContainer(container, h, DefaultLimits())
	require.NoError(t, err, "the geometry adds up even though the record count does not")

	_, _, err = rc.RecordAt(3)
	require.Error(t, err, "the page refuses an ordinal it does not carry")
}

// TestContainerRejectsDamagedPage: the container header and directory still
// check out, so the block parses and is cached; the damage only surfaces when
// that page is decompressed and re-validated (341). Every accessor that walks
// the pages must report it — a single record read (430) and a full scan (448)
// alike, never a partial result.
func TestContainerRejectsDamagedPage(t *testing.T) {
	fb, rc := buildContainer(t, 1<<10, 1<<20, format.CompressionNone,
		[]expectedPageRow{{rowID: 1, version: 1, ct: format.ChangeInsert}},
		[][]byte{[]byte("abcd")})

	stored := append([]byte(nil), fb.Stored...)
	stored[rc.RecordsRegionStart()+2] ^= 0xFF // inside the page, past the directory
	bad, err := ParseContainer(stored, fb.Header, DefaultLimits())
	require.NoError(t, err, "the container geometry and header CRC are still intact")

	_, _, err = bad.PageScratch(0)
	require.Error(t, err, "the page is re-validated when it is decoded")

	_, _, err = bad.RecordAt(0)
	require.Error(t, err, "a single-record read fails, it does not return an empty row")

	require.Error(t, bad.ForEach(func(codec.PageRecord) error { return nil }),
		"a scan reports the damage instead of ending early")
}

// TestRecordAtRejectsOrdinalOutsidePages: an ordinal past the last page is a
// caller error, reported before any page is decompressed (426).
func TestRecordAtRejectsOrdinalOutsidePages(t *testing.T) {
	_, rc := buildContainer(t, 1<<10, 1<<20, format.CompressionNone,
		[]expectedPageRow{{rowID: 1, version: 1, ct: format.ChangeInsert}},
		[][]byte{[]byte("abcd")})

	_, _, err := rc.RecordAt(1 << 20)
	require.Error(t, err)
}

// TestLazyContainerStoredLen: a lazy container owns only the header and the
// directory — its cache footprint is that, not the whole block's bytes (76).
func TestLazyContainerStoredLen(t *testing.T) {
	fb, rc := buildContainer(t, 1<<10, 1<<20, format.CompressionNone,
		[]expectedPageRow{{rowID: 1, version: 1, ct: format.ChangeInsert}},
		[][]byte{[]byte("abcd")})

	data := make([]byte, format.BlockHeaderSize+len(fb.Stored))
	require.NoError(t, fb.Header.MarshalTo(data))
	copy(data[format.BlockHeaderSize:], fb.Stored)

	lazy, err := ParseRowsDir(0, NewReader(&mockReaderAt{data: data}, DefaultLimits()), fb.Header, DefaultLimits())
	require.NoError(t, err)
	require.Equal(t, lazy.RecordsRegionStart(), lazy.StoredLen(),
		"a lazy container owns the header and the directory, no more")
	require.Less(t, lazy.StoredLen(), len(fb.Stored), "the pages are not resident")

	// A container without cache accounting reports its stored length as the
	// retained length (94).
	require.Equal(t, int64(rc.StoredLen()), rc.RetainedLen())
	require.Greater(t, rc.StoredLen(), 0, "the whole container is resident")
}
