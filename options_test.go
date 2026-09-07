package rowpack

import (
	"errors"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func TestOptionsResolvedDefaults(t *testing.T) {
	o, err := Options{}.resolved()
	if err != nil {
		t.Fatal(err)
	}
	if o.BlockSize != fileformat.DefaultBlockSize {
		t.Fatalf("BlockSize default = %d", o.BlockSize)
	}
	if o.Compression != CompressionZstd {
		t.Fatalf("Compression default = %d", o.Compression)
	}
	if o.CacheBytes != fileformat.DefaultCacheBytes {
		t.Fatalf("CacheBytes default = %d", o.CacheBytes)
	}
	if o.Limits.MaxRowBytes != fileformat.DefaultMaxRowBytes ||
		o.Limits.MaxRawBlockBytes != fileformat.DefaultMaxRawBlockBytes ||
		o.Limits.MaxStoredBlockBytes != fileformat.DefaultMaxStoredBlockBytes ||
		o.Limits.MaxColumns != fileformat.DefaultMaxColumns ||
		o.Limits.MaxValueBytes != fileformat.DefaultMaxValueBytes ||
		o.Limits.MaxSnapshotDepth != fileformat.DefaultMaxSnapshotDepth {
		t.Fatalf("Limits defaults wrong: %+v", o.Limits)
	}
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
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: err = %v, want ErrInvalidArgument", tc.name, err)
		}
	}
}

func TestOptionsValidConfigs(t *testing.T) {
	for _, o := range []Options{
		{Compression: CompressionNone},
		{Compression: CompressionZstd, Durability: AsyncCommit, Validation: ValidationNone},
	} {
		if _, err := o.resolved(); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
	}
}

func TestOptionsDiskCompression(t *testing.T) {
	if got := (Options{Compression: CompressionNone}).diskCompression(); got != fileformat.CompressionNone {
		t.Fatalf("None -> %d", got)
	}
	if got := (Options{Compression: CompressionZstd}).diskCompression(); got != fileformat.CompressionZstd {
		t.Fatalf("Zstd -> %d", got)
	}
	// Anything else (unreachable via resolved) maps to None.
	if got := (Options{Compression: CompressionDefault}).diskCompression(); got != fileformat.CompressionNone {
		t.Fatalf("Default -> %d", got)
	}
}
