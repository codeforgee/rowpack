package block

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// smallWant builds n simple insert records for the container builders.
func smallWant(t *testing.T, n int) ([]expectedPageRow, [][]byte) {
	t.Helper()
	schema := pageTestSchema()
	var want []expectedPageRow
	var bodies [][]byte
	for i := uint64(1); i <= uint64(n); i++ {
		body := pageTestRow(t, schema, i)
		want = append(want, expectedPageRow{rowID: i, version: 1, ct: format.ChangeInsert, bodyLen: len(body)})
		bodies = append(bodies, body)
	}
	return want, bodies
}

func containerDirBytes(container []byte) uint32 {
	var rh format.RowsBlockHeader
	if err := rh.Unmarshal(container[:format.RowsBlockHeaderSize]); err != nil {
		panic(err)
	}
	return rh.DirectoryBytes
}

// TestParseContainerRejectsNonRowsKind: the container parser refuses blocks
// of any other kind.
func TestParseContainerRejectsNonRowsKind(t *testing.T) {
	want, bodies := smallWant(t, 10)
	fb, _ := buildContainer(t, 4<<10, 1<<20, format.CompressionNone, want, bodies)
	h := fb.Header
	h.BlockKind = format.BlockKindMetadata
	_, err := ParseContainer(fb.Stored, h, DefaultLimits())
	require.ErrorContains(t, err, "not rows")
}

// TestParseContainerEncryptedLengthContract: an encrypted block's StoredSize
// carries the AEAD tag; ParseContainer must accept the tagless plaintext
// length and the container stays fully readable.
func TestParseContainerEncryptedLengthContract(t *testing.T) {
	want, bodies := smallWant(t, 10)
	fb, _ := buildContainer(t, 4<<10, 1<<20, format.CompressionNone, want, bodies)
	h := fb.Header
	h.Encrypted = true
	h.StoredSize += format.AESGCMTagLen
	rc, err := ParseContainer(fb.Stored, h, DefaultLimits())
	require.NoError(t, err)
	p, release, err := rc.PageScratch(0)
	require.NoError(t, err)
	require.NotNil(t, p)
	release()
}

// TestParseContainerDirectoryOverrun: a forged directory size that escapes
// the container is rejected before the CRC check can even apply.
func TestParseContainerDirectoryOverrun(t *testing.T) {
	want, bodies := smallWant(t, 10)
	fb, _ := buildContainer(t, 4<<10, 1<<20, format.CompressionNone, want, bodies)
	payload := append([]byte(nil), fb.Stored...)
	var rh format.RowsBlockHeader
	require.NoError(t, rh.Unmarshal(payload[:format.RowsBlockHeaderSize]))
	// Unmarshal enforces DirectoryBytes == PageCount * entry size, so the only
	// way to escape the container is a huge-but-consistent page count.
	rh.PageCount = 1 << 20
	rh.DirectoryBytes = rh.PageCount * format.RowsPageDirEntrySize
	require.NoError(t, rh.MarshalTo(payload[:format.RowsBlockHeaderSize]))
	_, err := ParseContainer(payload, fb.Header, DefaultLimits())
	require.ErrorContains(t, err, "overruns")
}

// 容器头 magic 损坏由 container_geometry_test.go TestContainerHeaderCRCIsEnforced
// 末尾覆盖（同一 ParseContainer magic 臂，且断言诊断文本为 "magic"）。

// TestPageScratchOutOfRange: page indices outside the directory are rejected.
func TestPageScratchOutOfRange(t *testing.T) {
	want, bodies := smallWant(t, 10)
	_, rc := buildContainer(t, 4<<10, 1<<20, format.CompressionNone, want, bodies)
	_, _, err := rc.PageScratch(-1)
	require.ErrorContains(t, err, "out of range")
	_, _, err = rc.PageScratch(rc.PageCount())
	require.ErrorContains(t, err, "out of range")
}

// TestDecompressStoredExceedsLimit: for a None+encrypted container the stored
// page is raw+tag; a MaxRawBytes between the two must be enforced at page
// load (checkBounds passes on RawSize, the loader rejects the stored length).
func TestDecompressStoredExceedsLimit(t *testing.T) {
	want, bodies := smallWant(t, 4)
	fb, _ := buildContainer(t, 4<<10, 1<<20, format.CompressionNone, want, bodies)

	// Forge the encrypted-block length contract: pretend the single stored
	// page carries a 16-byte tag by growing dir.StoredSize and appending tag
	// bytes to the container.
	payload := append([]byte(nil), fb.Stored...)
	dirEnd := format.RowsBlockHeaderSize + int(containerDirBytes(payload))
	var dir format.RowsPageDirEntry
	require.NoError(t, dir.Unmarshal(payload[format.RowsBlockHeaderSize:dirEnd]))
	pageLen := dir.StoredSize
	dir.StoredSize += format.AESGCMTagLen
	require.NoError(t, dir.MarshalTo(payload[format.RowsBlockHeaderSize:dirEnd]))
	payload = append(payload, make([]byte, format.AESGCMTagLen)...)

	h := fb.Header
	h.Encrypted = true
	h.StoredSize = uint32(len(payload)) + format.AESGCMTagLen
	h.RawCRC32C = format.CRC32C(payload[:dirEnd])

	limits := DefaultLimits()
	limits.MaxRawBytes = pageLen + 8 // between raw and stored+tag
	rc, err := ParseContainer(payload, h, limits)
	require.NoError(t, err)
	_, _, err = rc.PageScratch(0)
	require.ErrorContains(t, err, "exceeds limit")
}

// TestDecompressZstdGarbage: corrupt page payload bytes (outside the CRC'd
// header/dir region) fail at zstd decode with the page index attached.
func TestDecompressZstdGarbage(t *testing.T) {
	want, bodies := smallWant(t, 50)
	fb, rc := buildContainer(t, 64, 1<<20, format.CompressionZstd, want, bodies)
	_ = fb
	payload := append([]byte(nil), rc.stored...)
	start := format.RowsBlockHeaderSize + len(rc.Dir)*format.RowsPageDirEntrySize
	payload[start+4] ^= 0xFF
	payload[start+5] ^= 0xFF
	rc2, err := ParseContainer(payload, fb.Header, DefaultLimits())
	require.NoError(t, err)
	_, _, err = rc2.PageScratch(0)
	require.ErrorContains(t, err, "page 0:")
}

// TestForgedDirRawSizeMismatch: a directory entry claiming a different raw
// size than the page decompresses to (with the container CRC restamped) is
// rejected after decode.
func TestForgedDirRawSizeMismatch(t *testing.T) {
	want, bodies := smallWant(t, 50)
	fb, rc := buildContainer(t, 64, 1<<20, format.CompressionZstd, want, bodies)
	payload := append([]byte(nil), rc.stored...)
	dirEnd := format.RowsBlockHeaderSize + int(containerDirBytes(payload))
	var dir format.RowsPageDirEntry
	require.NoError(t, dir.Unmarshal(payload[format.RowsBlockHeaderSize:dirEnd]))
	dir.RawSize++
	require.NoError(t, dir.MarshalTo(payload[format.RowsBlockHeaderSize:dirEnd]))
	h := fb.Header
	h.RawCRC32C = format.CRC32C(payload[:dirEnd])

	rc2, err := ParseContainer(payload, h, DefaultLimits())
	require.NoError(t, err)
	_, _, err = rc2.PageScratch(0)
	require.ErrorContains(t, err, "decompressed")
}

// TestForgedPageCRCMismatch: a directory entry whose per-page CRC disagrees
// with the actual page header (container CRC restamped) is rejected.
func TestForgedPageCRCMismatch(t *testing.T) {
	want, bodies := smallWant(t, 50)
	fb, rc := buildContainer(t, 64, 1<<20, format.CompressionZstd, want, bodies)
	payload := append([]byte(nil), rc.stored...)
	dirEnd := format.RowsBlockHeaderSize + int(containerDirBytes(payload))
	var dir format.RowsPageDirEntry
	require.NoError(t, dir.Unmarshal(payload[format.RowsBlockHeaderSize:dirEnd]))
	dir.PageCRC32C ^= 0x40
	require.NoError(t, dir.MarshalTo(payload[format.RowsBlockHeaderSize:dirEnd]))
	h := fb.Header
	h.RawCRC32C = format.CRC32C(payload[:dirEnd])

	rc2, err := ParseContainer(payload, h, DefaultLimits())
	require.NoError(t, err)
	_, _, err = rc2.PageScratch(0)
	require.ErrorContains(t, err, "header CRC")
}

// ---- disk path (ParseRowsDir via Reader) ----

// diskRowsBlock builds a minimal single-page rows block laid out as it would
// live in the data file: [BlockHeader][container header][dir][page bytes].
func diskRowsBlock(t *testing.T) ([]byte, format.BlockHeader) {
	t.Helper()
	pb := NewPageBuilder(32 << 10)
	body := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 8)
	require.NoError(t, pb.Add(1, 1, format.ChangeInsert, body))
	pageRaw, err := pb.Finish()
	require.NoError(t, err)
	pageStored, err := Compress(format.CompressionZstd, 3, pageRaw)
	require.NoError(t, err)

	var rh format.RowsBlockHeader
	rh.PageCount = 1
	rh.DirectoryBytes = format.RowsPageDirEntrySize
	rh.TotalRecords = 1
	hdr := make([]byte, format.RowsBlockHeaderSize)
	require.NoError(t, rh.MarshalTo(hdr))

	dir := format.RowsPageDirEntry{
		PageOrdinal:        0,
		FirstRecordOrdinal: 0,
		RecordCount:        1,
		StoredSize:         uint32(len(pageStored)),
		RawSize:            uint32(len(pageRaw)),
		MinRowID:           1,
		MaxRowID:           1,
		PageCRC32C:         format.CRC32C(pageRaw[format.RowsPageHeaderSize:]),
		StoredOffset:       uint64(format.RowsBlockHeaderSize + format.RowsPageDirEntrySize),
	}
	dirBuf := make([]byte, format.RowsPageDirEntrySize)
	require.NoError(t, dir.MarshalTo(dirBuf))

	container := bytes.Join([][]byte{hdr, dirBuf, pageStored}, nil)
	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   1,
		RawSize:     uint32(len(pageRaw)),
		StoredSize:  uint32(len(container)),
		RawCRC32C:   format.CRC32C(container[:format.RowsBlockHeaderSize+format.RowsPageDirEntrySize]),
	}
	blk := bytes.Join([][]byte{make([]byte, format.BlockHeaderSize), container}, nil)
	require.NoError(t, h.MarshalTo(blk[:format.BlockHeaderSize]))
	return blk, h
}

func TestReadRowsDirCorruptionMatrix(t *testing.T) {
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	const at = int64(0) // the fixture IS the block: header at offset 0

	// Happy path sanity: the fixture parses.
	blk, _ := diskRowsBlock(t)
	r := NewReader(bytes.NewReader(blk), limits)
	rc, err := r.ReadRowsDir(at)
	require.NoError(t, err)
	require.EqualValues(t, 1, rc.PageCount())
	// The lazy container forwards page counters to the reader's stats.
	_, _, err = rc.PageScratch(0)
	require.NoError(t, err)
	require.Equal(t, uint64(1), r.Stats().PageLoads)

	// Wrong block kind.
	blk, h := diskRowsBlock(t)
	h.BlockKind = format.BlockKindMetadata
	bh := make([]byte, format.BlockHeaderSize)
	require.NoError(t, h.MarshalTo(bh))
	copy(blk, bh)
	r = NewReader(bytes.NewReader(blk), limits)
	_, err = r.ReadRowsDir(at)
	require.ErrorContains(t, err, "expected rows", "reader.go words it differently from ParseContainer")

	// Unreadable container header: the file ends inside the block header.
	blk, _ = diskRowsBlock(t)
	r = NewReader(bytes.NewReader(blk[:format.BlockHeaderSize]), limits)
	_, err = r.ReadRowsDir(at)
	require.ErrorContains(t, err, "read container header")

	// Forged item count.
	blk, h = diskRowsBlock(t)
	h.ItemCount++
	bh = make([]byte, format.BlockHeaderSize)
	require.NoError(t, h.MarshalTo(bh))
	copy(blk, bh)
	r = NewReader(bytes.NewReader(blk), limits)
	_, err = r.ReadRowsDir(at)
	require.ErrorContains(t, err, "total records")

	// Directory region extending past the stored block: validateRowCounts
	// passes (wantDir <= StoredSize) but the resolved dirEnd overruns it.
	blk, h = diskRowsBlock(t)
	h.StoredSize = format.RowsBlockHeaderSize + 36
	bh = make([]byte, format.BlockHeaderSize)
	require.NoError(t, h.MarshalTo(bh))
	copy(blk, bh)
	r = NewReader(bytes.NewReader(blk), limits)
	_, err = r.ReadRowsDir(at)
	require.ErrorContains(t, err, "overruns stored")

	// Header/dir CRC mismatch: flip one directory byte.
	blk, _ = diskRowsBlock(t)
	blk[format.BlockHeaderSize+format.RowsBlockHeaderSize+4] ^= 0x01
	r = NewReader(bytes.NewReader(blk), limits)
	_, err = r.ReadRowsDir(at)
	require.ErrorContains(t, err, "CRC mismatch")

	// Container header that fails to unmarshal: wreck the DirectoryBytes
	// field so the header's own geometry check rejects it.
	blk, _ = diskRowsBlock(t)
	blk[format.BlockHeaderSize+format.RowsBlockHeaderSize-8] ^= 0xFF
	r = NewReader(bytes.NewReader(blk), limits)
	_, err = r.ReadRowsDir(at)
	require.Error(t, err)
}

// silence unused warnings for helpers only used on some builds
var _ = binary.LittleEndian
var _ codec.PageRecord
