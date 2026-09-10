package block

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/metadata"
)

// MetadataBuilder accumulates metadata records of one snapshot and emits
// Metadata Blocks. A Metadata Block belongs to one snapshot; its TableID is
// the single-table object or 0.
type MetadataBuilder struct {
	snapshotID uint64
	tableID    uint32
	blockSize  int
	compress   fileformat.Compression
	level      int
	limits     Limits

	entries []metadata.DirectoryEntry
	records [][]byte
	count   uint32

	// enc is the caller-owned zstd encoder (store-level, outliving GC pool
	// churn); nil selects the pooled encoder.
	enc *ZstdEncoder

	onFlush func(*FlushedBlock) error
}

// NewMetadataBuilder creates a builder for the given snapshot, using the
// shared Config.
func NewMetadataBuilder(snapshotID uint64, tableID uint32, cfg Config) *MetadataBuilder {
	return &MetadataBuilder{
		snapshotID: snapshotID,
		tableID:    tableID,
		blockSize:  cfg.BlockSize,
		compress:   cfg.Compression,
		level:      cfg.Level,
		limits:     cfg.Limits,
		onFlush:    cfg.OnFlush,
	}
}

// SetZstdEncoder attaches a caller-owned zstd encoder used at Flush time
// instead of the pooled one.
func (b *MetadataBuilder) SetZstdEncoder(e *ZstdEncoder) { b.enc = e }

// Add appends one metadata record body with its directory entry, flushing
// when the pending payload reaches the target size.
func (b *MetadataBuilder) Add(e metadata.DirectoryEntry, rec []byte) error {
	if uint32(len(rec)) > b.limits.MaxRawBytes {
		return fmt.Errorf("rowpack: metadata record of %d bytes exceeds limit %d", len(rec), b.limits.MaxRawBytes)
	}
	b.entries = append(b.entries, e)
	b.records = append(b.records, rec)
	b.count++
	// Estimate raw payload size; flush when at or above target.
	if b.rawSize() >= b.blockSize {
		return b.Flush()
	}
	return nil
}

func (b *MetadataBuilder) rawSize() int {
	n := metadata.PayloadHeaderSize + len(b.entries)*metadata.DirectoryEntrySize
	for _, r := range b.records {
		n += len(r)
	}
	return n
}

// Flush emits the pending records as one block, if any.
func (b *MetadataBuilder) Flush() error {
	if b.count == 0 {
		return nil
	}
	raw, err := metadata.Build(b.entries, b.records)
	if err != nil {
		return err
	}
	var compressed []byte
	if b.enc != nil && b.compress == fileformat.CompressionZstd {
		compressed, err = EncodeZstdWith(b.enc, raw)
	} else {
		compressed, err = Compress(b.compress, b.level, raw)
	}
	if err != nil {
		return err
	}
	h := fileformat.BlockHeader{
		BlockKind:   fileformat.BlockKindMetadata,
		Compression: b.compress,
		SnapshotID:  b.snapshotID,
		TableID:     b.tableID,
		ItemCount:   b.count,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   fileformat.CRC32C(raw),
	}
	if err := b.onFlush(&FlushedBlock{Header: h, Stored: compressed, Raw: raw, Meta: b.entries}); err != nil {
		return err
	}
	b.entries = b.entries[:0]
	b.records = b.records[:0]
	b.count = 0
	return nil
}

// Pending returns the number of buffered records.
func (b *MetadataBuilder) Pending() int { return int(b.count) }
