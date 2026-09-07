package fileformat

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// updateGolden regenerates golden files instead of comparing. Enable with
// `go test ./... -run TestGolden -args -update-golden` (see Makefile).
var updateGolden = flag.Bool("update-golden", false, "regenerate golden files")

// goldenPath returns the absolute path of a golden file under testdata/golden.
func goldenPath(name string) string {
	return filepath.Join("..", "..", "testdata", "golden", name)
}

// fixedStoreUUID is the deterministic UUID used by golden generators so byte
// comparison is stable across runs.
var fixedStoreUUID = [16]byte{0x52, 0x4f, 0x57, 0x50, 0x41, 0x43, 0x4b, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

// buildEmptyDataHeader renders the canonical empty-store .rpk header.
func buildEmptyDataHeader() []byte {
	var h DataFileHeader
	h.FileHeader = FileHeader{
		StoreUUID:          fixedStoreUUID,
		CreatedUnixNano:    1757400000000000000,
		RequiredFeatures:   RequiredFeaturesV1,
		OptionalFeatures:   0,
		DefaultBlockSize:   DefaultBlockSize,
		DefaultCompression: CompressionZstd,
		DefaultRowEncoding: RowEncodingTypedTuple,
		Flags:              0,
	}
	buf := make([]byte, DataFileHeaderSize)
	_ = h.MarshalTo(buf)
	return buf
}

// buildEmptyIndexHeader renders the canonical empty-store .rpi header.
func buildEmptyIndexHeader() []byte {
	var h IndexFileHeader
	h.FileHeader = FileHeader{
		StoreUUID:          fixedStoreUUID,
		CreatedUnixNano:    1757400000000000000,
		RequiredFeatures:   RequiredFeaturesV1,
		OptionalFeatures:   0,
		DefaultBlockSize:   DefaultBlockSize,
		DefaultCompression: CompressionZstd,
		DefaultRowEncoding: RowEncodingTypedTuple,
		Flags:              0,
	}
	buf := make([]byte, IndexFileHeaderSize)
	_ = h.MarshalTo(buf)
	return buf
}

// TestGoldenEmptyStore locks the two-header "empty Store" golden files.
func TestGoldenEmptyStore(t *testing.T) {
	want := map[string][]byte{
		"empty-store.rpk": buildEmptyDataHeader(),
		"empty-store.rpi": buildEmptyIndexHeader(),
	}
	for name, data := range want {
		path := goldenPath(name)
		if *updateGolden {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, data, 0o644))
			continue
		}
		got, err := os.ReadFile(path)
		require.NoError(t, err, "read golden %s: %v (regenerate with make golden)", name, err)
		if string(got) != string(data) {
			require.Fail(t, "golden %s differs from implementation (regenerate with make golden)", name)
		}
		// Golden must be exactly the two-header size.
		require.Equal(t, DataFileHeaderSize, len(got), "golden %s size = %d, want %d", name, len(got), DataFileHeaderSize)
	}
}
