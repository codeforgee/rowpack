package block

import (
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/metadata"
)

// MetadataBlockBuilder accumulates metadata records of one snapshot and emits
// Metadata Blocks. A Metadata Block belongs to one snapshot; its TableID is
// the single-table object or 0.
type MetadataBlockBuilder struct {
	snapshotID uint64
	tableID    uint32
	blockSize  int
	compress   fileformat.Compression
	level      int
	limits     Limits

	entries []metadata.DirectoryEntry
	records [][]byte
	count   uint32

	onFlush func(h fileformat.BlockHeader, stored, raw []byte) error
}

// NewMetadataBlockBuilder creates a builder for the given snapshot.
func NewMetadataBlockBuilder(snapshotID uint64, tableID uint32, blockSize int, compress fileformat.Compression, level int, limits Limits, onFlush func(fileformat.BlockHeader, []byte, []byte) error) *MetadataBlockBuilder {
	return &MetadataBlockBuilder{
		snapshotID: snapshotID,
		tableID:    tableID,
		blockSize:  blockSize,
		compress:   compress,
		level:      level,
		limits:     limits,
		onFlush:    onFlush,
	}
}

// Add appends one metadata record body with its directory entry, flushing
// when the pending payload reaches the target size.
func (b *MetadataBlockBuilder) Add(e metadata.DirectoryEntry, rec []byte) error {
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

func (b *MetadataBlockBuilder) rawSize() int {
	n := metadata.PayloadHeaderSize + len(b.entries)*metadata.DirectoryEntrySize
	for _, r := range b.records {
		n += len(r)
	}
	return n
}

// Flush emits the pending records as one block, if any.
func (b *MetadataBlockBuilder) Flush() error {
	if b.count == 0 {
		return nil
	}
	raw, err := metadata.Build(b.entries, b.records)
	if err != nil {
		return err
	}
	compressed, err := Compress(b.compress, b.level, raw)
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
	if err := b.onFlush(h, compressed, raw); err != nil {
		return err
	}
	b.entries = b.entries[:0]
	b.records = b.records[:0]
	b.count = 0
	return nil
}

// Pending returns the number of buffered records.
func (b *MetadataBlockBuilder) Pending() int { return int(b.count) }

var _ = errors.New
