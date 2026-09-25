package block

import (
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// TestDecompressBombRejected verifies the per-call maxOut policy limit: a
// valid zstd frame whose decoded size exceeds maxOut is rejected (the
// decoder-level zstd.WithDecoderMaxMemory ceiling is the second layer).
func TestDecompressBombRejected(t *testing.T) {
	src := make([]byte, 4096)
	stored, err := Compress(format.CompressionZstd, 3, src)
	require.NoError(t, err)

	_, err = Decompress(format.CompressionZstd, nil, stored, 1024)
	require.ErrorContains(t, err, "exceeds limit")

	// At the exact limit it decodes fine.
	out, err := Decompress(format.CompressionZstd, nil, stored, uint32(len(src)))
	require.NoError(t, err)
	require.Equal(t, src, out)
}

// TestDecompressNoneOverLimit and the unsupported-algorithm arms keep the
// CompressionNone / unknown-alg branches of Decompress and Compress covered.
func TestDecompressNoneOverLimit(t *testing.T) {
	src := make([]byte, 100)
	_, err := Decompress(format.CompressionNone, nil, src, 50)
	require.ErrorContains(t, err, "exceeds limit")

	out, err := Decompress(format.CompressionNone, nil, src, 100)
	require.NoError(t, err)
	require.Equal(t, src, out)

	_, err = Compress(format.Compression(99), 0, src)
	require.ErrorContains(t, err, "unsupported compression")

	_, err = Decompress(format.Compression(99), nil, src, 100)
	require.ErrorContains(t, err, "unsupported compression")
}
