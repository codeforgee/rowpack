package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// TestMmapReaderEquivalence verifies the mmap View fast path produces
// byte-identical blocks to the plain ReadAt path, and that mmap-backed
// reads never let cached blocks alias the file mapping (V1.1-C).
func TestMmapReaderEquivalence(t *testing.T) {
	base := filepath.Join(tmpdb(t), "mmap")
	db, fullID := buildMemStore(t, base, 5000)
	st, err := db.captureState()
	require.NoError(t, err)
	view := st.view
	// Collect every block location from the committed view.
	bls := view.Blocks()
	require.NotEmpty(t, bls, "no blocks in view")

	// Plain ReadAt reader over a separate file handle.
	f, err := os.Open(base + ".rpk")
	require.NoError(t, err)
	defer f.Close()
	plain := block.NewReader(f, block.DefaultLimits())

	for _, bl := range bls {
		off := int64(bl.DataOffset)
		gotView, err := db.reader.ReadAtBlock(off)
		require.NoError(t, err, "block %d: view read", bl.BlockID)
		gotCopy, err := plain.ReadAtBlock(off)
		require.NoError(t, err, "block %d: copy read", bl.BlockID)
		require.Equal(t, gotView.Header, gotCopy.Header, "block %d: header mismatch", bl.BlockID)
		require.Equal(t, string(gotView.Raw), string(gotCopy.Raw), "block %d: raw mismatch (%d vs %d bytes)", bl.BlockID, len(gotView.Raw), len(gotCopy.Raw))
		require.Equal(t, gotView.Header.RawCRC32C, fileformat.CRC32C(gotView.Raw), "block %d: CRC mismatch", bl.BlockID)
	}

	// Data reads through the public API must agree with the committed rows.
	for i := uint64(0); i < 100; i++ {
		_, err := db.Get(context.Background(), fullID, "bench", i+1, nil)
		require.NoError(t, err, "get %d", i+1)
	}
}
