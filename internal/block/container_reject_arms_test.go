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
// 到不了的四条:parsePageDir 的 truncated(229)与条目 Unmarshal 失败(232)——目录长度在
// 容器头 Unmarshal 时就已被钉成 PageCount*RowsPageDirEntrySize,容器长度又已被 dirEnd
// 检查过,所以每个条目都恒有完整的一条目可读,Unmarshal 只查长度也因此不会失败;
// ParseContainer/ParseRowsDir 里传播 parsePageDir 错误的那两行(150、201)随之没有输入。

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
		PageCount:      1000,
		DirectoryBytes: 1000 * format.RowsPageDirEntrySize,
		TotalRecords:   1,
	}

	data := make([]byte, format.BlockHeaderSize+format.RowsBlockHeaderSize)
	require.NoError(t, h.MarshalTo(data))
	require.NoError(t, rh.MarshalTo(data[format.BlockHeaderSize:]))

	_, err := ParseRowsDir(0, NewReader(&mockReaderAt{data: data}, DefaultLimits()), h, DefaultLimits())
	require.ErrorContains(t, err, "!=", "the directory cannot fit the block that carries it")
}

// TestParseRowsDirRejectsUnreadableDirectory: the directory is read in one
// ReadAt; a file that ends inside it is an I/O failure, not a corrupt block
// (193).
func TestParseRowsDirRejectsUnreadableDirectory(t *testing.T) {
	h := format.BlockHeader{BlockID: 1, BlockKind: format.BlockKindRows, ItemCount: 1, StoredSize: 1 << 20}
	// One page, so the directory is 32 bytes past the container header — and
	// the file stops right after that header.
	rh := format.RowsBlockHeader{PageCount: 2, DirectoryBytes: 2 * format.RowsPageDirEntrySize, TotalRecords: 1}

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
	rh := format.RowsBlockHeader{PageCount: 1, DirectoryBytes: format.RowsPageDirEntrySize, TotalRecords: 1}
	// None-compressed pages are stored == raw; this one claims otherwise.
	dir := format.RowsPageDirEntry{
		RecordCount:  1,
		StoredOffset: format.RowsBlockHeaderSize + format.RowsPageDirEntrySize,
		StoredSize:   8,
		RawSize:      16,
	}

	data := make([]byte, format.BlockHeaderSize+format.RowsBlockHeaderSize+format.RowsPageDirEntrySize)
	require.NoError(t, rh.MarshalTo(data[format.BlockHeaderSize:]))
	require.NoError(t, dir.MarshalTo(data[format.BlockHeaderSize+format.RowsBlockHeaderSize:]))
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
	rh := format.RowsBlockHeader{PageCount: 1, DirectoryBytes: format.RowsPageDirEntrySize, TotalRecords: 5}
	dir := format.RowsPageDirEntry{
		RecordCount:  5, // claims five, the page holds two
		StoredOffset: format.RowsBlockHeaderSize + format.RowsPageDirEntrySize,
		StoredSize:   uint32(len(page)),
		RawSize:      uint32(len(page)),
		PageCRC32C:   format.CRC32C(page[format.RowsPageHeaderSize:]),
	}

	container := make([]byte, format.RowsBlockHeaderSize+format.RowsPageDirEntrySize+len(page))
	require.NoError(t, rh.MarshalTo(container))
	require.NoError(t, dir.MarshalTo(container[format.RowsBlockHeaderSize:]))
	copy(container[format.RowsBlockHeaderSize+format.RowsPageDirEntrySize:], page)
	h.StoredSize = uint32(len(container))
	h.RawCRC32C = format.CRC32C(container[:format.RowsBlockHeaderSize+format.RowsPageDirEntrySize])

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
	stored[format.RowsBlockHeaderSize+len(rc.Dir)*format.RowsPageDirEntrySize+2] ^= 0xFF // inside the page, past the directory
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
	require.Equal(t, format.RowsBlockHeaderSize+len(lazy.Dir)*format.RowsPageDirEntrySize, lazy.StoredLen())
	require.Less(t, lazy.StoredLen(), len(fb.Stored), "the pages are not resident")

	// A container without cache accounting reports its stored length as the
	// retained length (94).
	require.Equal(t, int64(rc.StoredLen()), rc.RetainedLen())
	require.Greater(t, rc.StoredLen(), 0, "the whole container is resident")
}
