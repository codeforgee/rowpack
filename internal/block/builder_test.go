package block

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/metadata"
	"github.com/stretchr/testify/require"
)

type mockFlushedBlock struct {
	blocks []*FlushedBlock
}

func (m *mockFlushedBlock) onFlush(fb *FlushedBlock) error {
	m.blocks = append(m.blocks, fb)
	return nil
}

func TestRowsBlockBuilder(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewRowsBuilder(1, 1, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Add some records
	for i := uint64(1); i <= 10; i++ {
		tuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, byte(i), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		err := builder.Add(i, 1, format.ChangeInsert, tuple)
		require.NoError(t, err)
	}

	// Flush the remaining
	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 1, len(mock.blocks))
	block := mock.blocks[0]
	require.Equal(t, format.BlockKindRows, block.Header.BlockKind)
	require.Equal(t, uint64(1), block.Header.SnapshotID)
	require.Equal(t, uint32(1), block.Header.TableID)
	require.Equal(t, uint32(10), block.Header.ItemCount)
	require.Greater(t, block.Header.RawSize, uint32(0))
	require.Greater(t, block.Header.StoredSize, uint32(0))
	require.Equal(t, 10, len(block.Rows))
}

func TestRowsBlockBuilderFlushEmpty(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewRowsBuilder(1, 1, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Flush without adding any records
	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 0, len(mock.blocks))
}

func TestRowsBlockBuilderPending(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewRowsBuilder(1, 1, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	require.Equal(t, 0, len(builder.entries))

	for i := uint64(1); i <= 5; i++ {
		tuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, byte(i), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		err := builder.Add(i, 1, format.ChangeInsert, tuple)
		require.NoError(t, err)
		require.Equal(t, int(i), len(builder.entries))
	}
}

func TestRowsBlockBuilderOversizedRow(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	// Small block size to force flush
	builder := NewRowsBuilder(1, 1, Config{BlockSize: 512, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Add a row larger than page size but smaller than block size
	largeTuple := bytes.Repeat([]byte{0xFF}, 300)
	err := builder.Add(1, 1, format.ChangeInsert, largeTuple)
	require.NoError(t, err)

	// Add another row
	tuple2 := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	err = builder.Add(2, 1, format.ChangeInsert, tuple2)
	require.NoError(t, err)

	err = builder.Flush()
	require.NoError(t, err)

	// Should have at least one block
	require.GreaterOrEqual(t, len(mock.blocks), 1)
}

func TestRowsBlockBuilderOversizedRowLargerThanBlock(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	// Small block size
	builder := NewRowsBuilder(1, 1, Config{BlockSize: 512, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Add a row larger than block size - should flush current block first
	largeTuple := bytes.Repeat([]byte{0xFF}, 600)
	err := builder.Add(1, 1, format.ChangeInsert, largeTuple)
	require.NoError(t, err)

	err = builder.Flush()
	require.NoError(t, err)

	// Should have at least one block
	require.GreaterOrEqual(t, len(mock.blocks), 1)
}

func TestRowsBlockBuilderMultiplePages(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	// Small page size to force multiple pages
	builder := NewRowsBuilder(1, 1, Config{BlockSize: 2048, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})
	builder.SetPageSize(256)

	// Add many records to fill multiple pages
	for i := uint64(1); i <= 50; i++ {
		tuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, byte(i), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		err := builder.Add(i, 1, format.ChangeInsert, tuple)
		require.NoError(t, err)
	}

	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 1, len(mock.blocks))
	block := mock.blocks[0]
	// PageCount is in the container header, parse it from Raw
	var containerHeader format.RowsBlockHeader
	require.NoError(t, containerHeader.Unmarshal(block.Raw))
	require.Greater(t, containerHeader.PageCount, uint32(1))
	require.Equal(t, uint32(50), block.Header.ItemCount)
}

func TestRowsBlockBuilderSetZstdEncoder(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewRowsBuilder(1, 1, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Create a ZstdEncoder and set it
	enc := NewZstdEncoder(3)
	builder.SetZstdEncoder(enc)

	for i := uint64(1); i <= 5; i++ {
		tuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, byte(i), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		err := builder.Add(i, 1, format.ChangeInsert, tuple)
		require.NoError(t, err)
	}

	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 1, len(mock.blocks))
}

func TestRowsBlockBuilderAddErrors(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 100, MaxRawBytes: 100} // Small limits

	builder := NewRowsBuilder(1, 1, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Row exceeds MaxRawBytes
	largeTuple := bytes.Repeat([]byte{0xFF}, 200)
	err := builder.Add(1, 1, format.ChangeInsert, largeTuple)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds limit")
}

func TestMetadataBlockBuilder(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewMetadataBuilder(1, 0, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Add some metadata records
	for i := uint32(1); i <= 5; i++ {
		entry := metadata.DirectoryEntry{
			ObjectID:     1,
			Revision:     1,
			RecordType:   2, // Table
			RecordOffset: 0,
			RecordLength: uint32(20 + i),
			Operation:    1, // Upsert
			Critical:     false,
		}
		record := bytes.Repeat([]byte{byte(i)}, 20+int(i))
		err := builder.Add(entry, record)
		require.NoError(t, err)
	}

	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 1, len(mock.blocks))
	block := mock.blocks[0]
	require.Equal(t, format.BlockKindMetadata, block.Header.BlockKind)
	require.Equal(t, uint64(1), block.Header.SnapshotID)
	require.Equal(t, uint32(5), block.Header.ItemCount)
	require.Equal(t, 5, len(block.Meta))
}

func TestMetadataBlockBuilderFlushEmpty(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewMetadataBuilder(1, 0, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 0, len(mock.blocks))
}

func TestMetadataBlockBuilderPending(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewMetadataBuilder(1, 0, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	require.Equal(t, 0, int(builder.count))

	for i := uint32(1); i <= 3; i++ {
		entry := metadata.DirectoryEntry{ObjectID: 1, Revision: 1, RecordType: 2, Operation: 1}
		record := []byte{byte(i)}
		err := builder.Add(entry, record)
		require.NoError(t, err)
		require.Equal(t, int(i), int(builder.count))
	}
}

func TestMetadataBlockBuilderAutoFlush(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	// Small block size to trigger auto-flush
	builder := NewMetadataBuilder(1, 0, Config{BlockSize: 200, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Add records that will exceed block size
	for i := uint32(1); i <= 10; i++ {
		entry := metadata.DirectoryEntry{ObjectID: 1, Revision: 1, RecordType: 2, Operation: 1}
		record := bytes.Repeat([]byte{byte(i)}, 50)
		err := builder.Add(entry, record)
		require.NoError(t, err)
	}

	err := builder.Flush()
	require.NoError(t, err)

	// Should have at least one block
	require.GreaterOrEqual(t, len(mock.blocks), 1)
}

func TestMetadataBlockBuilderSetZstdEncoder(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewMetadataBuilder(1, 0, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	enc := NewZstdEncoder(3)
	builder.SetZstdEncoder(enc)

	entry := metadata.DirectoryEntry{ObjectID: 1, Revision: 1, RecordType: 2, Operation: 1}
	record := []byte{0x01, 0x02, 0x03}
	err := builder.Add(entry, record)
	require.NoError(t, err)

	err = builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 1, len(mock.blocks))
}

func TestMetadataBlockBuilderAddErrors(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 100, MaxRawBytes: 100}

	builder := NewMetadataBuilder(1, 0, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Record exceeds MaxRawBytes
	entry := metadata.DirectoryEntry{ObjectID: 1, Revision: 1, RecordType: 2, Operation: 1}
	largeRecord := bytes.Repeat([]byte{0xFF}, 200)
	err := builder.Add(entry, largeRecord)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds limit")
}

func TestRowsBlockBuilderChangeTypes(t *testing.T) {
	mock := &mockFlushedBlock{}
	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}

	builder := NewRowsBuilder(1, 1, Config{BlockSize: 1024, Compression: format.CompressionZstd, Level: 3, Limits: limits, OnFlush: mock.onFlush})

	// Test all change types
	changes := []format.ChangeType{
		format.ChangeInsert,
		format.ChangeUpdate,
		format.ChangeDelete,
	}

	for i, change := range changes {
		tuple := []byte{0x01, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, byte(i + 1), 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
		err := builder.Add(uint64(i+1), 1, change, tuple)
		require.NoError(t, err)
	}

	err := builder.Flush()
	require.NoError(t, err)

	require.Equal(t, 1, len(mock.blocks))
	require.Equal(t, 3, len(mock.blocks[0].Rows))
	require.Equal(t, format.ChangeInsert, mock.blocks[0].Rows[0].ChangeType)
	require.Equal(t, format.ChangeUpdate, mock.blocks[0].Rows[1].ChangeType)
	require.Equal(t, format.ChangeDelete, mock.blocks[0].Rows[2].ChangeType)
}
