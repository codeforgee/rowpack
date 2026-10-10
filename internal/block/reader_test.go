package block

import (
	"bytes"
	"io"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

type mockReaderAt struct {
	data []byte
}

func (m *mockReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n = copy(p, m.data[off:])
	if n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (m *mockReaderAt) View(offset, n int64) ([]byte, func(), error) {
	if offset < 0 || offset+n > int64(len(m.data)) {
		return nil, nil, io.EOF
	}
	return m.data[offset : offset+n], func() {}, nil
}

func TestNewReader(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)
	require.NotNil(t, r)
}

func TestReaderSetDecrypter(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	r.SetDecrypter(nil)
	require.Nil(t, r.decrypter)
}

func TestReaderStats(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	stats := r.Stats()
	require.Equal(t, uint64(0), stats.ReadBytes)
	require.Equal(t, uint64(0), stats.DecompressedBytes)
	require.Equal(t, uint64(0), stats.PageLoads)
	require.Equal(t, uint64(0), stats.PageRawBytes)
	require.Equal(t, uint64(0), stats.PageStoredBytes)
}

func TestReaderCount(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	h := &format.BlockHeader{
		StoredSize: 100,
		RawSize:    200,
	}
	r.count(h)

	stats := r.Stats()
	require.Equal(t, uint64(format.BlockHeaderSize+100), stats.ReadBytes)
	require.Equal(t, uint64(200), stats.DecompressedBytes)
}

func TestReaderCheckHeader(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 100, MaxRawBytes: 100}
	r := NewReader(ra, limits)

	t.Run("stored size exceeds limit", func(t *testing.T) {
		h := &format.BlockHeader{StoredSize: 200, RawSize: 50}
		err := r.checkHeader(h)
		require.Error(t, err)
		require.Contains(t, err.Error(), "stored size")
	})

	t.Run("raw size exceeds limit", func(t *testing.T) {
		h := &format.BlockHeader{StoredSize: 50, RawSize: 200}
		err := r.checkHeader(h)
		require.Error(t, err)
		require.Contains(t, err.Error(), "raw size")
	})

	t.Run("none compression size mismatch", func(t *testing.T) {
		h := &format.BlockHeader{StoredSize: 100, RawSize: 50, Compression: format.CompressionNone, Encrypted: false}
		err := r.checkHeader(h)
		require.Error(t, err)
		require.Contains(t, err.Error(), "none-compressed block stored")
	})

	t.Run("encrypted none compression ok", func(t *testing.T) {
		h := &format.BlockHeader{StoredSize: 100, RawSize: 50, Compression: format.CompressionNone, Encrypted: true}
		err := r.checkHeader(h)
		require.NoError(t, err)
	})

	t.Run("valid header", func(t *testing.T) {
		h := &format.BlockHeader{StoredSize: 50, RawSize: 50, Compression: format.CompressionNone}
		err := r.checkHeader(h)
		require.NoError(t, err)
	})
}

func TestReaderMaybeDecrypt(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	h := &format.BlockHeader{BlockID: 1, StoredSize: 50, Encrypted: false}

	t.Run("plain block", func(t *testing.T) {
		stored := bytes.Repeat([]byte{0x42}, 50)
		result, err := r.maybeDecrypt(stored, h)
		require.NoError(t, err)
		require.Equal(t, stored, result)
	})

	t.Run("encrypted without decrypter", func(t *testing.T) {
		hEncrypted := &format.BlockHeader{BlockID: 1, StoredSize: 50, Encrypted: true}
		stored := bytes.Repeat([]byte{0x42}, 50)
		_, err := r.maybeDecrypt(stored, hEncrypted)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no decrypter")
	})
}

func TestReaderDecompress(t *testing.T) {
	ra := &mockReaderAt{data: []byte{}}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	h := &format.BlockHeader{BlockID: 1, Compression: format.CompressionNone}

	t.Run("none compression", func(t *testing.T) {
		stored := []byte("hello world")
		result, err := r.decompress(h, stored)
		require.NoError(t, err)
		require.Equal(t, stored, result)
	})

	t.Run("zstd compression", func(t *testing.T) {
		hZstd := &format.BlockHeader{BlockID: 1, Compression: format.CompressionZstd}
		raw := bytes.Repeat([]byte{0x42}, 1000)
		compressed, err := Compress(format.CompressionZstd, 3, raw)
		require.NoError(t, err)
		result, err := r.decompress(hZstd, compressed)
		require.NoError(t, err)
		require.Equal(t, raw, result)
	})
}

func TestReaderReadAtBlockCopy(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, 1000)
	compressed, err := Compress(format.CompressionZstd, 3, raw)
	require.NoError(t, err)

	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   10,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   format.CRC32C(raw),
	}

	var buf bytes.Buffer
	hdr := make([]byte, format.BlockHeaderSize)
	_ = h.MarshalTo(hdr)
	buf.Write(hdr)
	buf.Write(compressed)

	ra := &mockReaderAt{data: buf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	block, err := r.ReadAtBlock(0)
	require.NoError(t, err)
	require.Equal(t, h.BlockKind, block.Header.BlockKind)
	require.Equal(t, h.Compression, block.Header.Compression)
	require.Equal(t, h.SnapshotID, block.Header.SnapshotID)
	require.Equal(t, h.TableID, block.Header.TableID)
	require.Equal(t, h.ItemCount, block.Header.ItemCount)
	require.Equal(t, raw, block.Raw)
}

func TestReaderReadAtBlockCopyPlain(t *testing.T) {
	raw := []byte("plain block data")
	h := format.BlockHeader{
		BlockKind:   format.BlockKindMetadata,
		Compression: format.CompressionNone,
		SnapshotID:  1,
		TableID:     0,
		ItemCount:   1,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(raw)),
		RawCRC32C:   format.CRC32C(raw),
	}

	var buf bytes.Buffer
	hdr := make([]byte, format.BlockHeaderSize)
	_ = h.MarshalTo(hdr)
	buf.Write(hdr)
	buf.Write(raw)

	ra := &mockReaderAt{data: buf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	block, err := r.ReadAtBlock(0)
	require.NoError(t, err)
	require.Equal(t, raw, block.Raw)
}

func TestReaderReadAtBlockCRCError(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, 1000)
	compressed, err := Compress(format.CompressionZstd, 3, raw)
	require.NoError(t, err)

	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   10,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   0xDEADBEEF, // Wrong CRC
	}

	var buf bytes.Buffer
	hdr := make([]byte, format.BlockHeaderSize)
	_ = h.MarshalTo(hdr)
	buf.Write(hdr)
	buf.Write(compressed)

	ra := &mockReaderAt{data: buf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	_, err = r.ReadAtBlock(0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "CRC mismatch")
}

func TestReaderReadAtBlockView(t *testing.T) {
	raw := bytes.Repeat([]byte{0x42}, 1000)
	compressed, err := Compress(format.CompressionZstd, 3, raw)
	require.NoError(t, err)

	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   10,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   format.CRC32C(raw),
	}

	var buf bytes.Buffer
	hdr := make([]byte, format.BlockHeaderSize)
	_ = h.MarshalTo(hdr)
	buf.Write(hdr)
	buf.Write(compressed)

	ra := &mockReaderAt{data: buf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	block, err := r.ReadAtBlock(0)
	require.NoError(t, err)
	require.Equal(t, raw, block.Raw)
}

func TestReaderReadRowsDir(t *testing.T) {
	// Build a minimal rows block with container header and page directory
	// Create a page with one record using the page builder
	pageBuilder := NewPageBuilder(32 << 10)
	simpleTuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	require.NoError(t, pageBuilder.Add(1, 1, format.ChangeInsert, simpleTuple))
	pageRaw, err := pageBuilder.Finish()
	require.NoError(t, err)

	pageStored, err := Compress(format.CompressionZstd, 3, pageRaw)
	require.NoError(t, err)

	dirEntry := format.RowsPageDirEntry{
		PageOrdinal:        0,
		FirstRecordOrdinal: 0,
		RecordCount:        1,
		StoredSize:         uint32(len(pageStored)),
		RawSize:            uint32(len(pageRaw)),
		MinRowID:           1,
		MaxRowID:           1,
	}
	dirBuf := encodeDir(dirEntry)
	containerHeader := format.RowsBlockHeader{
		PageCount:      1,
		DirectoryBytes: uint32(len(dirBuf)),
		TotalRecords:   1,
	}
	dirEntry.StoredOffset = uint64(format.RowsBlockHeaderSize + len(dirBuf))

	var containerBuf bytes.Buffer
	hdr := make([]byte, format.RowsBlockHeaderSize)
	_ = containerHeader.MarshalTo(hdr)
	containerBuf.Write(hdr)
	containerBuf.Write(dirBuf)

	// RawCRC32C is CRC of header + directory only (not including stored pages)
	headerAndDir := containerBuf.Bytes()
	containerBuf.Write(pageStored)

	blockHeader := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   1,
		RawSize:     uint32(len(pageRaw)),
		StoredSize:  uint32(containerBuf.Len()),
		RawCRC32C:   format.CRC32C(headerAndDir),
	}

	hdrBytes := make([]byte, format.BlockHeaderSize)
	_ = blockHeader.MarshalTo(hdrBytes)

	fullBuf := bytes.Buffer{}
	fullBuf.Write(hdrBytes)
	fullBuf.Write(containerBuf.Bytes())

	ra := &mockReaderAt{data: fullBuf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	container, err := r.ReadRowsDir(0)
	require.NoError(t, err)
	require.NotNil(t, container)
	require.EqualValues(t, 1, len(container.Dir))
}

func TestReaderReadRowsPage(t *testing.T) {
	// Build a minimal rows block with one page
	// Create a page with one record using the page builder
	pageBuilder := NewPageBuilder(32 << 10)
	simpleTuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	require.NoError(t, pageBuilder.Add(1, 1, format.ChangeInsert, simpleTuple))
	pageRaw, err := pageBuilder.Finish()
	require.NoError(t, err)

	pageStored, err := Compress(format.CompressionZstd, 3, pageRaw)
	require.NoError(t, err)

	dirEntry := format.RowsPageDirEntry{
		PageOrdinal:        0,
		FirstRecordOrdinal: 0,
		RecordCount:        1,
		StoredSize:         uint32(len(pageStored)),
		RawSize:            uint32(len(pageRaw)),
		MinRowID:           1,
		MaxRowID:           1,
	}
	dirBuf := encodeDir(dirEntry)
	containerHeader := format.RowsBlockHeader{
		PageCount:      1,
		DirectoryBytes: uint32(len(dirBuf)),
		TotalRecords:   1,
	}
	dirEntry.StoredOffset = uint64(format.RowsBlockHeaderSize + len(dirBuf))

	var containerBuf bytes.Buffer
	hdr := make([]byte, format.RowsBlockHeaderSize)
	_ = containerHeader.MarshalTo(hdr)
	containerBuf.Write(hdr)
	containerBuf.Write(dirBuf)

	// RawCRC32C is CRC of header + directory only (not including stored pages)
	headerAndDir := containerBuf.Bytes()
	containerBuf.Write(pageStored)

	blockHeader := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   1,
		RawSize:     uint32(len(pageRaw)),
		StoredSize:  uint32(containerBuf.Len()),
		RawCRC32C:   format.CRC32C(headerAndDir),
	}

	hdrBytes := make([]byte, format.BlockHeaderSize)
	_ = blockHeader.MarshalTo(hdrBytes)

	fullBuf := bytes.Buffer{}
	fullBuf.Write(hdrBytes)
	fullBuf.Write(containerBuf.Bytes())

	ra := &mockReaderAt{data: fullBuf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	container, err := r.ReadRowsDir(0)
	require.NoError(t, err)

	page, err := r.ReadRowsPage(0, container, 0)
	require.NoError(t, err)
	require.NotNil(t, page)
	require.Equal(t, uint32(1), page.Header().EntryCount)
}

func TestReaderReadRowsPageEncryptedNoDecrypter(t *testing.T) {
	// Build a minimal rows block with one page (with one record)
	pageBuilder := NewPageBuilder(32 << 10)
	simpleTuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	require.NoError(t, pageBuilder.Add(1, 1, format.ChangeInsert, simpleTuple))
	pageRaw, err := pageBuilder.Finish()
	require.NoError(t, err)

	pageStored, err := Compress(format.CompressionZstd, 3, pageRaw)
	require.NoError(t, err)

	dirEntry := format.RowsPageDirEntry{
		PageOrdinal:        0,
		FirstRecordOrdinal: 0,
		RecordCount:        1,
		StoredSize:         uint32(len(pageStored)),
		RawSize:            uint32(len(pageRaw)),
		MinRowID:           1,
		MaxRowID:           1,
	}
	dirBuf := encodeDir(dirEntry)
	containerHeader := format.RowsBlockHeader{
		PageCount:      1,
		DirectoryBytes: uint32(len(dirBuf)),
		TotalRecords:   1,
	}
	dirEntry.StoredOffset = uint64(format.RowsBlockHeaderSize + len(dirBuf))

	var containerBuf bytes.Buffer
	hdr := make([]byte, format.RowsBlockHeaderSize)
	_ = containerHeader.MarshalTo(hdr)
	containerBuf.Write(hdr)
	containerBuf.Write(dirBuf)

	// RawCRC32C is CRC of header + directory only (not including stored pages)
	headerAndDir := containerBuf.Bytes()
	containerBuf.Write(pageStored)

	blockHeader := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   1,
		RawSize:     uint32(len(pageRaw)),
		StoredSize:  uint32(containerBuf.Len()),
		RawCRC32C:   format.CRC32C(headerAndDir),
		Encrypted:   true,
	}

	hdrBytes := make([]byte, format.BlockHeaderSize)
	_ = blockHeader.MarshalTo(hdrBytes)

	fullBuf := bytes.Buffer{}
	fullBuf.Write(hdrBytes)
	fullBuf.Write(containerBuf.Bytes())

	ra := &mockReaderAt{data: fullBuf.Bytes()}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(ra, limits)

	container, err := r.ReadRowsDir(0)
	require.NoError(t, err)

	_, err = r.ReadRowsPage(0, container, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "encrypted but no decrypter")
}
