package rowpack

import (
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

func TestOptionsResolvedDefaults(t *testing.T) {
	o, err := Options{}.resolved()
	require.NoError(t, err)
	require.Equal(t, fileformat.DefaultBlockSize, o.BlockSize)
	require.Equal(t, CompressionZstd, o.Compression)
	require.Equal(t, int64(fileformat.DefaultCacheBytes), o.CacheBytes)
	require.Equal(t, fileformat.DefaultMaxRowBytes, o.Limits.MaxRowBytes)
	require.Equal(t, fileformat.DefaultMaxRawBlockBytes, o.Limits.MaxRawBlockBytes)
	require.Equal(t, fileformat.DefaultMaxStoredBlockBytes, o.Limits.MaxStoredBlockBytes)
	require.Equal(t, fileformat.DefaultMaxColumns, o.Limits.MaxColumns)
	require.Equal(t, fileformat.DefaultMaxValueBytes, o.Limits.MaxValueBytes)
	require.Equal(t, fileformat.DefaultMaxSnapshotDepth, o.Limits.MaxSnapshotDepth)
}

func TestOptionsValidateRejects(t *testing.T) {
	base := Options{}.applyDefaults()
	cases := []struct {
		name string
		mut  func(*Options)
	}{
		{"bad compression", func(o *Options) { o.Compression = Compression(9) }},
		{"block too small", func(o *Options) { o.BlockSize = 8 }},
		{"block over raw limit", func(o *Options) { o.Limits.MaxRawBlockBytes = 64; o.BlockSize = 128 }},
		{"bad durability", func(o *Options) { o.Durability = Durability(9) }},
		{"bad validation", func(o *Options) { o.Validation = ValidationMode(9) }},
		{"row over raw block", func(o *Options) { o.Limits.MaxRowBytes = o.Limits.MaxRawBlockBytes + 1 }},
	}
	for _, tc := range cases {
		o := base
		tc.mut(&o)
		_, err := o.resolved()
		require.ErrorIs(t, err, ErrInvalidArgument, "%s: err = %v", tc.name, err)
	}
}

func TestOptionsValidConfigs(t *testing.T) {
	for _, o := range []Options{
		{Compression: CompressionNone},
		{Compression: CompressionZstd, Durability: AsyncCommit, Validation: ValidationNone},
	} {
		_, err := o.resolved()
		require.NoError(t, err, "%+v", o)
	}
}

func TestOptionsDiskCompression(t *testing.T) {
	require.Equal(t, fileformat.CompressionNone, (Options{Compression: CompressionNone}).diskCompression(), "None")
	require.Equal(t, fileformat.CompressionZstd, (Options{Compression: CompressionZstd}).diskCompression(), "Zstd")
	// Anything else (unreachable via resolved) maps to None.
	require.Equal(t, fileformat.CompressionNone, (Options{Compression: CompressionDefault}).diskCompression(), "Default")
}
