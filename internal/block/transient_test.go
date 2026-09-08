package block

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// stubViewer is a minimal viewer + ReaderAt over an in-memory buffer,
// exercising the transient view (mmap-like) path without a real mapping.
type stubViewer struct {
	data []byte
}

func (s *stubViewer) View(offset, n int64) ([]byte, func(), error) {
	if offset < 0 || n < 0 || offset+n > int64(len(s.data)) {
		return nil, nil, errShortBlock
	}
	return s.data[offset : offset+n], func() {}, nil
}

func (s *stubViewer) ReadAt(b []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(b)) > int64(len(s.data)) {
		return 0, errShortBlock
	}
	return copy(b, s.data[off:]), nil
}

func buildSingleBlock(t *testing.T, compress fileformat.Compression, rows int) capturedBlock {
	t.Helper()
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 4096, compress, 3, DefaultLimits(), s.flush)
	for i := 0; i < rows; i++ {
		require.NoError(t, b.Add(uint64(i), 1, fileformat.ChangeInsert, mkRow(64)))
	}
	require.NoError(t, b.Flush())
	require.NotEmpty(t, s.blocks)
	return s.blocks[0]
}

func TestTransientMatchesRegular(t *testing.T) {
	for _, compress := range []fileformat.Compression{fileformat.CompressionNone, fileformat.CompressionZstd} {
		cb := buildSingleBlock(t, compress, 50)
		raw := appendBlock(nil, &cb)

		// ReadAt (copy) path.
		reg, err := NewReader(bytes.NewReader(raw), DefaultLimits()).ReadAtBlock(0)
		require.NoError(t, err, "%v: regular read failed", compress)
		tr, err := NewReader(bytes.NewReader(raw), DefaultLimits()).ReadAtBlockTransient(0)
		require.NoError(t, err, "%v: transient read failed", compress)
		require.Equal(t, reg.Header, tr.Header, "%v: header mismatch", compress)
		require.True(t, bytes.Equal(reg.Raw, tr.Raw), "%v: raw mismatch", compress)
		require.NotSame(t, &reg.Raw[0], &tr.Raw[0], "%v: transient must own its buffer", compress)
		tr.Release()
		tr.Release() // idempotent

		// mmap-like view path.
		trv, err := NewReader(&stubViewer{data: raw}, DefaultLimits()).ReadAtBlockTransient(0)
		require.NoError(t, err, "%v: transient view read failed", compress)
		require.True(t, bytes.Equal(reg.Raw, trv.Raw), "%v: view raw mismatch", compress)
		trv.Release()
	}
}

func TestTransientRejectsCorruption(t *testing.T) {
	cb := buildSingleBlock(t, fileformat.CompressionZstd, 50)
	raw := appendBlock(nil, &cb)
	read := func(b []byte) error {
		sc, err := NewReader(bytes.NewReader(b), DefaultLimits()).ReadAtBlockTransient(0)
		if err == nil {
			sc.Release()
		}
		return err
	}

	require.NoError(t, read(raw), "valid block rejected")

	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 0xFF
	require.Error(t, read(bad), "corrupt payload accepted by transient")

	bad = append([]byte(nil), raw...)
	bad[53] ^= 0xFF
	require.Error(t, read(bad), "corrupt header accepted by transient")

	require.Error(t, read(raw[:len(raw)-10]), "truncated block accepted by transient")
	require.Error(t, read(raw[:5]), "tiny input accepted by transient")
	require.Error(t, read(nil), "empty input accepted by transient")
}

// TestTransientPoolIsolation verifies that sequential transient reads through
// the same pool never observe stale bytes from a previous block.
func TestTransientPoolIsolation(t *testing.T) {
	full := []byte(nil)
	offsets := make([]int64, 0, 4)
	for i := 0; i < 4; i++ {
		cb := buildSingleBlock(t, fileformat.CompressionZstd, 3+i)
		offsets = append(offsets, int64(len(full)))
		full = append(full, appendBlock(nil, &cb)...)
	}
	rd := NewReader(bytes.NewReader(full), DefaultLimits())
	for i := 0; i < 8; i++ { // more rounds than blocks: pool recycles
		for _, off := range offsets {
			sc, err := rd.ReadAtBlockTransient(off)
			require.NoError(t, err)
			if uint32(len(sc.Raw)) != sc.Header.RawSize {
				t.Fatalf("raw length %d != header %d", len(sc.Raw), sc.Header.RawSize)
			}
			if fileformat.CRC32C(sc.Raw) != sc.Header.RawCRC32C {
				t.Fatal("stale pooled buffer observed: CRC mismatch")
			}
			sc.Release()
		}
	}
}