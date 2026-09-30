package block

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// reader_arms_test.go 覆盖 Reader 的错误传播臂:ReadAt / viewer 的 I/O 失败、头部
// 解析失败、限制校验失败,以及加密块的 OPEN/DECRYPT 失败与长度不符。这些臂的共同
// 契约是「坏输入必须报错,既不能 panic 也不能返回未验证的数据」,靠注入 ReaderAt
// 与伪 Decrypter 才能确定性地触发。

var errFault = errors.New("injected io fault")

// deadReaderAt fails every read, unlike the truncated plainReaderAt fixtures.
type deadReaderAt struct{}

func (deadReaderAt) ReadAt([]byte, int64) (int, error) { return 0, errFault }

// faultView is a viewer that fails either its header view (fail==1) or its
// payload view (fail==2); everything else is served from data.
type faultView struct {
	data  []byte
	fail  int
	calls int
}

func (v *faultView) View(offset, n int64) ([]byte, func(), error) {
	v.calls++
	if v.calls == v.fail {
		return nil, nil, errFault
	}
	if offset < 0 || offset+n > int64(len(v.data)) {
		return nil, nil, errFault
	}
	return v.data[offset : offset+n], func() {}, nil
}

func (v *faultView) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(v.data)) {
		return 0, errFault
	}
	n := copy(p, v.data[off:])
	if n < len(p) {
		return n, errFault
	}
	return n, nil
}

// fakeDecrypter is a scripted Decrypter whose hooks default to failing closed:
// a non-encrypted call site must never see a nil deref, only the arms the
// sub-test installed.
type fakeDecrypter struct {
	decrypt func(h format.BlockHeader, ciphertext []byte) ([]byte, error)
	open    func(h format.BlockHeader, page format.RowsPageDirEntry, ciphertext []byte) ([]byte, error)
	calls   int
}

func (f *fakeDecrypter) Decrypt(h format.BlockHeader, ciphertext []byte) ([]byte, error) {
	f.calls++
	if f.decrypt == nil {
		return nil, errors.New("unexpected Decrypt call")
	}
	return f.decrypt(h, ciphertext)
}

func (f *fakeDecrypter) OpenPage(h format.BlockHeader, page format.RowsPageDirEntry, ciphertext []byte) ([]byte, error) {
	f.calls++
	if f.open == nil {
		return nil, errors.New("unexpected OpenPage call")
	}
	return f.open(h, page, ciphertext)
}

// oneRecordPage builds a one-record Rows Page in its uncompressed form.
func oneRecordPage(tb testing.TB) []byte {
	tb.Helper()
	pb := NewPageBuilder(32 << 10)
	// Single int64 column, schema version 1, change INSERT, value 1.
	tuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	require.NoError(tb, pb.Add(1, 1, format.ChangeInsert, tuple))
	raw, err := pb.Finish()
	require.NoError(tb, err)
	return raw
}

// rowsBlockImage is one single-page Rows block file image built from scratch:
// image is the whole file, stored is the page as it lives on disk (sealed for
// encrypted blocks), plain is what OpenPage must return to strip the tag and
// raw is the decompressed page.
type rowsBlockImage struct {
	image   []byte
	pageOff int64
	stored  []byte
	plain   []byte
	raw     []byte
}

// buildRowsBlockImage assembles image for the given page compression; when
// encrypted is set every stored page carries an appended AEAD tag, so the
// directory sizes include it and a correct OpenPage returns stored - tag.
func buildRowsBlockImage(tb testing.TB, alg format.Compression, encrypted bool) *rowsBlockImage {
	tb.Helper()
	raw := oneRecordPage(tb)
	stored := raw
	if alg == format.CompressionZstd {
		var err error
		stored, err = Compress(alg, 3, raw)
		require.NoError(tb, err)
	}
	filePage, plain := stored, []byte(nil)
	if encrypted {
		plain = stored
		filePage = append(append([]byte(nil), stored...), make([]byte, format.AESGCMTagLen)...)
	}
	dir := format.RowsPageDirEntry{
		PageOrdinal:        0,
		FirstRecordOrdinal: 0,
		RecordCount:        1,
		StoredSize:         uint32(len(filePage)),
		RawSize:            uint32(len(raw)),
		MinRowID:           1,
		MaxRowID:           1,
		PageCRC32C:         format.CRC32C(raw[format.RowsPageHeaderSize:]),
		StoredOffset:       uint64(format.RowsBlockHeaderSize + format.RowsPageDirEntrySize),
	}
	chdr := format.RowsBlockHeader{PageCount: 1, DirectoryBytes: format.RowsPageDirEntrySize, TotalRecords: 1}

	var container bytes.Buffer
	cb := make([]byte, format.RowsBlockHeaderSize)
	require.NoError(tb, chdr.MarshalTo(cb))
	container.Write(cb)
	db := make([]byte, format.RowsPageDirEntrySize)
	require.NoError(tb, dir.MarshalTo(db))
	container.Write(db)
	hdrAndDir := container.Bytes()
	container.Write(filePage)

	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: alg,
		SnapshotID:  1,
		TableID:     1,
		ItemCount:   1,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(container.Len()),
		RawCRC32C:   format.CRC32C(hdrAndDir),
		Encrypted:   encrypted,
	}
	hb := make([]byte, format.BlockHeaderSize)
	require.NoError(tb, h.MarshalTo(hb))

	return &rowsBlockImage{
		image:   append(append([]byte(nil), hb...), container.Bytes()...),
		pageOff: format.BlockHeaderSize + int64(format.RowsBlockHeaderSize+format.RowsPageDirEntrySize),
		stored:  filePage,
		plain:   plain,
		raw:     raw,
	}
}

func TestReadRowsDirErrorArms(t *testing.T) {
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	t.Run("header io failure", func(t *testing.T) {
		r := NewReader(deadReaderAt{}, limits)
		_, err := r.ReadRowsDir(0)
		require.ErrorIs(t, err, errFault, "the handle failure must be wrapped, not swallowed")
		require.Contains(t, err.Error(), "read block header at 0")
	})

	t.Run("header not decodable", func(t *testing.T) {
		garbage := bytes.Repeat([]byte{0xFF}, format.BlockHeaderSize)
		r := NewReader(plainReaderAt(garbage), limits)
		_, err := r.ReadRowsDir(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bad magic")
	})

	t.Run("header sizes over limits", func(t *testing.T) {
		h := format.BlockHeader{BlockKind: format.BlockKindRows, StoredSize: 1 << 21, ItemCount: 1}
		hb := make([]byte, format.BlockHeaderSize)
		require.NoError(t, h.MarshalTo(hb))
		r := NewReader(plainReaderAt(hb), limits)
		_, err := r.ReadRowsDir(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})
}

func TestReadRowsPagePlainErrorArms(t *testing.T) {
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	t.Run("page io failure", func(t *testing.T) {
		img := buildRowsBlockImage(t, format.CompressionZstd, false)
		// Everything up to (but not including) the page payload survives.
		truncated := img.image[:img.pageOff+1]
		r := NewReader(plainReaderAt(truncated), limits)
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		_, err = r.ReadRowsPage(0, c, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "read page 0 at")
	})

	t.Run("page does not decompress", func(t *testing.T) {
		img := buildRowsBlockImage(t, format.CompressionZstd, false)
		bad := append([]byte(nil), img.image...)
		bad[img.pageOff] ^= 0xFF // flip inside the stored page
		r := NewReader(plainReaderAt(bad), limits)
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		_, err = r.ReadRowsPage(0, c, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "rowpack: page 0:")
	})

	t.Run("uncompressed page round trip", func(t *testing.T) {
		// CompressionNone takes the raw = stored shortcut that every other
		// fixture bypasses; it must still validate sizes and parse.
		img := buildRowsBlockImage(t, format.CompressionNone, false)
		r := NewReader(plainReaderAt(img.image), limits)
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		page, err := r.ReadRowsPage(0, c, 0)
		require.NoError(t, err)
		require.EqualValues(t, 1, page.Header().EntryCount)
		require.EqualValues(t, 1, r.Stats().PageLoads)
	})

	t.Run("decompressed size disagrees with the directory", func(t *testing.T) {
		// The directory is authenticated by the block CRC, so a forging writer
		// is the only way to reach this arm: RawSize must be rechecked against
		// what decompression actually produced.
		img := buildRowsBlockImage(t, format.CompressionNone, false)
		r := NewReader(plainReaderAt(img.image), limits)
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		c.Dir[0].RawSize = uint32(len(img.raw) + 1)
		_, err = r.ReadRowsPage(0, c, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "page 0 decompressed")
	})
}

func TestReadRowsPageEncryptedArms(t *testing.T) {
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	img := buildRowsBlockImage(t, format.CompressionZstd, true)

	t.Run("open fails", func(t *testing.T) {
		r := NewReader(plainReaderAt(img.image), limits)
		r.SetDecrypter(&fakeDecrypter{open: func(format.BlockHeader, format.RowsPageDirEntry, []byte) ([]byte, error) {
			return nil, errors.New("tag mismatch")
		}})
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		_, err = r.ReadRowsPage(0, c, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "tag mismatch")
	})

	t.Run("opened plaintext is the wrong size", func(t *testing.T) {
		r := NewReader(plainReaderAt(img.image), limits)
		r.SetDecrypter(&fakeDecrypter{open: func(format.BlockHeader, format.RowsPageDirEntry, []byte) ([]byte, error) {
			return img.plain[:len(img.plain)-1], nil // dropped a byte
		}})
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		_, err = r.ReadRowsPage(0, c, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "opened")
		require.Contains(t, err.Error(), "- tag")
	})

	t.Run("opened page decodes", func(t *testing.T) {
		r := NewReader(plainReaderAt(img.image), limits)
		d := &fakeDecrypter{open: func(_ format.BlockHeader, page format.RowsPageDirEntry, ct []byte) ([]byte, error) {
			require.Equal(t, img.stored, ct, "the reader must hand the sealed bytes to OpenPage")
			return img.plain, nil
		}}
		r.SetDecrypter(d)
		c, err := r.ReadRowsDir(0)
		require.NoError(t, err)
		page, err := r.ReadRowsPage(0, c, 0)
		require.NoError(t, err)
		require.EqualValues(t, 1, page.Header().EntryCount)
		require.EqualValues(t, 1, d.calls, "the page is OPENed exactly once")
		require.EqualValues(t, len(img.stored), r.Stats().PageStoredBytes, "the sealed bytes are charged, not the plaintext")
	})
}

// TestReadAtBlockViewErrorArms walks the zero-copy (mmap) path.
func TestReadAtBlockViewErrorArms(t *testing.T) {
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	raw := []byte("view-path block payload")
	compressed, err := Compress(format.CompressionZstd, 3, raw)
	require.NoError(t, err)
	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		ItemCount:   1,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   format.CRC32C(raw),
	}
	image := func(h format.BlockHeader, payload []byte) []byte {
		hb := make([]byte, format.BlockHeaderSize)
		require.NoError(t, h.MarshalTo(hb))
		return append(append([]byte(nil), hb...), payload...)
	}

	t.Run("header view fails", func(t *testing.T) {
		r := NewReader(&faultView{data: image(h, compressed), fail: 1}, limits)
		_, err := r.ReadAtBlock(0)
		require.ErrorIs(t, err, errFault)
		require.Contains(t, err.Error(), "read block header at 0")
	})

	t.Run("viewed header does not decode", func(t *testing.T) {
		r := NewReader(&faultView{data: bytes.Repeat([]byte{0x00}, format.BlockHeaderSize)}, limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bad magic")
	})

	t.Run("viewed header over limits", func(t *testing.T) {
		overLimit := h
		overLimit.StoredSize = 1 << 21
		r := NewReader(&faultView{data: image(overLimit, compressed)}, limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})

	t.Run("payload view fails", func(t *testing.T) {
		// The header view succeeds, so the payload view is the second call.
		r := NewReader(&faultView{data: image(h, compressed), fail: 2}, limits)
		_, err := r.ReadAtBlock(0)
		require.ErrorIs(t, err, errFault)
		require.Contains(t, err.Error(), "read block payload at")
	})

	t.Run("encrypted without a decrypter", func(t *testing.T) {
		enc := h
		enc.Encrypted = true
		r := NewReader(&faultView{data: image(enc, compressed)}, limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no decrypter")
	})

	t.Run("payload does not decompress", func(t *testing.T) {
		// Same stored length as the real payload, but not a zstd frame: the
		// view succeeds and decompression rejects the bytes.
		r := NewReader(&faultView{data: image(h, bytes.Repeat([]byte{0xFF}, len(compressed)))}, limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "rowpack: block")
	})

	t.Run("decompressed size disagrees with the header", func(t *testing.T) {
		// None-compression at Rows kind is exempt from stored == raw, which
		// leaves RawSize as the only witness of an oversized payload.
		mismatch := h
		mismatch.Compression = format.CompressionNone
		mismatch.StoredSize = uint32(len(raw))
		mismatch.RawSize = uint32(len(raw) + 1)
		mismatch.RawCRC32C = format.CRC32C(raw)
		r := NewReader(&faultView{data: image(mismatch, raw)}, limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "decompressed")
	})
}

// TestReadAtBlockCopyErrorArms walks the io.ReaderAt path (no viewer).
func TestReadAtBlockCopyErrorArms(t *testing.T) {
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	raw := []byte("copy-path block payload")
	compressed, err := Compress(format.CompressionZstd, 3, raw)
	require.NoError(t, err)
	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		ItemCount:   1,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   format.CRC32C(raw),
	}
	image := func(h format.BlockHeader, payload []byte) []byte {
		hb := make([]byte, format.BlockHeaderSize)
		require.NoError(t, h.MarshalTo(hb))
		return append(append([]byte(nil), hb...), payload...)
	}

	t.Run("header does not decode", func(t *testing.T) {
		r := NewReader(plainReaderAt(bytes.Repeat([]byte{0x00}, format.BlockHeaderSize)), limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bad magic")
	})

	t.Run("header over limits", func(t *testing.T) {
		overLimit := h
		overLimit.StoredSize = 1 << 21
		hb := make([]byte, format.BlockHeaderSize)
		require.NoError(t, overLimit.MarshalTo(hb))
		r := NewReader(plainReaderAt(hb), limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "exceeds limit")
	})

	t.Run("payload short read", func(t *testing.T) {
		// Header only: the stored payload read hits EOF.
		hb := make([]byte, format.BlockHeaderSize)
		require.NoError(t, h.MarshalTo(hb))
		r := NewReader(plainReaderAt(hb), limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "read block payload at")
	})

	t.Run("encrypted without a decrypter", func(t *testing.T) {
		enc := h
		enc.Encrypted = true
		r := NewReader(plainReaderAt(image(enc, compressed)), limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no decrypter")
	})

	t.Run("decompressed size disagrees with the header", func(t *testing.T) {
		mismatch := h
		mismatch.Compression = format.CompressionNone
		mismatch.StoredSize = uint32(len(raw))
		mismatch.RawSize = uint32(len(raw) + 1)
		mismatch.RawCRC32C = format.CRC32C(raw)
		r := NewReader(plainReaderAt(image(mismatch, raw)), limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "decompressed")
	})

	t.Run("raw CRC mismatch", func(t *testing.T) {
		badCRC := h
		badCRC.RawCRC32C = ^h.RawCRC32C
		r := NewReader(plainReaderAt(image(badCRC, compressed)), limits)
		_, err := r.ReadAtBlock(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "raw CRC mismatch")
	})
}

func TestMaybeDecryptEncryptedArms(t *testing.T) {
	r := NewReader(plainReaderAt(nil), Limits{MaxStoredBytes: 1 << 16, MaxRawBytes: 1 << 16})
	h := format.BlockHeader{BlockID: 7, StoredSize: 32, Encrypted: true}
	ct := bytes.Repeat([]byte{0xA5}, int(h.StoredSize))
	// A correct OPEN strips exactly the AEAD tag.
	plaintext := bytes.Repeat([]byte{0x5A}, int(h.StoredSize)-format.AESGCMTagLen)

	t.Run("decrypt fails", func(t *testing.T) {
		sentinel := errors.New("ciphertext below assigned keyring")
		r.SetDecrypter(&fakeDecrypter{decrypt: func(format.BlockHeader, []byte) ([]byte, error) {
			return nil, sentinel
		}})
		defer r.SetDecrypter(nil)
		_, err := r.maybeDecrypt(ct, &h)
		require.ErrorIs(t, err, sentinel, "the OPEN failure must reach the caller unchanged")
	})

	t.Run("decrypted plaintext is the wrong size", func(t *testing.T) {
		r.SetDecrypter(&fakeDecrypter{decrypt: func(format.BlockHeader, []byte) ([]byte, error) {
			return plaintext[:len(plaintext)-1], nil
		}})
		defer r.SetDecrypter(nil)
		_, err := r.maybeDecrypt(ct, &h)
		require.Error(t, err)
		require.Contains(t, err.Error(), "block 7 decrypted")
		require.Contains(t, err.Error(), "- tag")
	})

	t.Run("decrypted plaintext is returned", func(t *testing.T) {
		r.SetDecrypter(&fakeDecrypter{decrypt: func(_ format.BlockHeader, got []byte) ([]byte, error) {
			require.Equal(t, ct, got, "the stored ciphertext is handed over verbatim")
			return plaintext, nil
		}})
		defer r.SetDecrypter(nil)
		got, err := r.maybeDecrypt(ct, &h)
		require.NoError(t, err)
		require.Equal(t, plaintext, got)
	})
}
