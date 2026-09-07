package rowpack

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// Compression selects the block compression at Create time. The API enum is
// deliberately not equal to the disk enum: the zero value means "default".
type Compression uint8

const (
	CompressionDefault Compression = iota // resolved to Zstd at Create
	CompressionNone
	CompressionZstd
)

// Durability selects the commit durability mode.
type Durability uint8

const (
	SyncCommit  Durability = iota // data + index files fsync before returning
	AsyncCommit                   // no explicit sync; process crash may lose the last commit
)

// ValidationMode controls parent-view row existence checks.
type ValidationMode uint8

const (
	ValidationStrict ValidationMode = iota
	ValidationNone                  // skips only the parent-view row existence checks
)

// Limits bounds resource usage; zero values are replaced by the defaults.
type Limits struct {
	MaxRowBytes         uint32
	MaxRawBlockBytes    uint32
	MaxStoredBlockBytes uint32
	MaxColumns          uint32
	MaxValueBytes       uint32
	MaxSnapshotDepth    uint32
}

// Options configure a Store at Create or Open. Zero values are replaced by
// defaults in Create/Open; new fields must remain zero-value safe.
type Options struct {
	ReadOnly         bool
	BlockSize        int
	Compression      Compression
	CompressionLevel int
	CacheBytes       int64
	Durability       Durability
	Validation       ValidationMode
	Limits           Limits
}

// resolved returns an Options copy with defaults applied.
func (o Options) resolved() (Options, error) {
	o = o.applyDefaults()
	if err := o.validate(); err != nil {
		return o, err
	}
	return o, nil
}

func (o Options) applyDefaults() Options {
	if o.BlockSize <= 0 {
		o.BlockSize = fileformat.DefaultBlockSize
	}
	if o.Compression == CompressionDefault {
		o.Compression = CompressionZstd
	}
	if o.CacheBytes == 0 {
		o.CacheBytes = fileformat.DefaultCacheBytes
	}
	l := &o.Limits
	if l.MaxRowBytes == 0 {
		l.MaxRowBytes = fileformat.DefaultMaxRowBytes
	}
	if l.MaxRawBlockBytes == 0 {
		l.MaxRawBlockBytes = fileformat.DefaultMaxRawBlockBytes
	}
	if l.MaxStoredBlockBytes == 0 {
		l.MaxStoredBlockBytes = fileformat.DefaultMaxStoredBlockBytes
	}
	if l.MaxColumns == 0 {
		l.MaxColumns = fileformat.DefaultMaxColumns
	}
	if l.MaxValueBytes == 0 {
		l.MaxValueBytes = fileformat.DefaultMaxValueBytes
	}
	if l.MaxSnapshotDepth == 0 {
		l.MaxSnapshotDepth = fileformat.DefaultMaxSnapshotDepth
	}
	return o
}

func (o Options) validate() error {
	if o.Compression != CompressionNone && o.Compression != CompressionZstd {
		return fmt.Errorf("%w: compression %d", ErrInvalidArgument, o.Compression)
	}
	if o.BlockSize < 64 {
		return fmt.Errorf("%w: block size %d too small", ErrInvalidArgument, o.BlockSize)
	}
	if o.BlockSize > int(o.Limits.MaxRawBlockBytes) {
		return fmt.Errorf("%w: block size %d exceeds raw block limit %d", ErrInvalidArgument, o.BlockSize, o.Limits.MaxRawBlockBytes)
	}
	if o.Durability != SyncCommit && o.Durability != AsyncCommit {
		return fmt.Errorf("%w: durability %d", ErrInvalidArgument, o.Durability)
	}
	if o.Validation != ValidationStrict && o.Validation != ValidationNone {
		return fmt.Errorf("%w: validation %d", ErrInvalidArgument, o.Validation)
	}
	if o.Limits.MaxRowBytes > o.Limits.MaxRawBlockBytes {
		return fmt.Errorf("%w: max row %d exceeds max raw block %d", ErrInvalidArgument, o.Limits.MaxRowBytes, o.Limits.MaxRawBlockBytes)
	}
	return nil
}

// diskCompression maps the API enum to the disk enum.
func (o Options) diskCompression() fileformat.Compression {
	switch o.Compression {
	case CompressionNone:
		return fileformat.CompressionNone
	case CompressionZstd:
		return fileformat.CompressionZstd
	}
	return fileformat.CompressionNone
}

// codecLimits builds the codec limits from Options.
func (o Options) codecLimits() codec.Limits {
	return codec.Limits{
		MaxColumns:    o.Limits.MaxColumns,
		MaxValueBytes: o.Limits.MaxValueBytes,
		MaxRowBytes:   o.Limits.MaxRowBytes,
	}
}
