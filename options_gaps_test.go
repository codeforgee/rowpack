package rowpack

import (
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// TestOptionsApplyDefaults covers every default filled by applyDefaults.
func TestOptionsApplyDefaults(t *testing.T) {
	o := Options{}.applyDefaults()
	if o.BlockSize != fileformat.DefaultBlockSize {
		t.Errorf("BlockSize default = %d", o.BlockSize)
	}
	if o.PageSize != fileformat.DefaultPageSize {
		t.Errorf("PageSize default = %d", o.PageSize)
	}
	if o.Compression != CompressionZstd {
		t.Errorf("Compression default = %d", o.Compression)
	}
	if o.CacheBytes != fileformat.DefaultCacheBytes {
		t.Errorf("CacheBytes default = %d", o.CacheBytes)
	}
	l := o.Limits
	if l.MaxRowBytes != fileformat.DefaultMaxRowBytes ||
		l.MaxRawBlockBytes != fileformat.DefaultMaxRawBlockBytes ||
		l.MaxStoredBlockBytes != fileformat.DefaultMaxStoredBlockBytes ||
		l.MaxColumns != fileformat.DefaultMaxColumns ||
		l.MaxValueBytes != fileformat.DefaultMaxValueBytes ||
		l.MaxSnapshotDepth != fileformat.DefaultMaxSnapshotDepth {
		t.Errorf("Limits defaults not applied: %+v", l)
	}

	// A page larger than its block is clamped down to the block size.
	small := Options{BlockSize: 1024, PageSize: 1 << 20}.applyDefaults()
	if small.PageSize != 1024 {
		t.Errorf("oversized page not clamped: %d", small.PageSize)
	}
}

// TestOptionsValidateErrors exercises every rejection branch of validate.
func TestOptionsValidateErrors(t *testing.T) {
	valid := Options{}.applyDefaults()
	if err := valid.validate(); err != nil {
		t.Fatalf("default options must validate: %v", err)
	}

	cases := []struct {
		name  string
		mut   func(*Options)
		mutln func(*Limits)
	}{
		{"bad compression", func(o *Options) { o.Compression = Compression(9) }, nil},
		{"scan cache >= cache", func(o *Options) { o.ScanCacheBytes = o.CacheBytes }, nil},
		{"block too small", func(o *Options) { o.BlockSize = 63 }, nil},
		{"page too small", func(o *Options) { o.PageSize = 63 }, nil},
		{"block over raw limit", func(o *Options) { o.BlockSize = int(o.Limits.MaxRawBlockBytes) + 1 }, nil},
		{"bad durability", func(o *Options) { o.Durability = Durability(9) }, nil},
		{"bad validation", func(o *Options) { o.Validation = ValidationMode(9) }, nil},
		{"row over raw block", func(o *Options) {
			o.Limits.MaxRowBytes = o.Limits.MaxRawBlockBytes + 1
		}, nil},
	}
	for _, c := range cases {
		o := valid
		c.mut(&o)
		if err := o.validate(); err == nil {
			t.Errorf("%s: expected rejection", c.name)
		}
	}
}

// TestOptionsResolvedRoundtrip checks resolved() applies defaults and passes.
func TestOptionsResolvedRoundtrip(t *testing.T) {
	o, err := Options{}.resolved()
	if err != nil {
		t.Fatalf("resolved: %v", err)
	}
	if o.BlockSize == 0 || o.PageSize == 0 {
		t.Fatal("resolved options missing defaults")
	}
	if _, err := (Options{Compression: Compression(9)}).resolved(); err == nil {
		t.Fatal("invalid options must fail resolved()")
	}
}
