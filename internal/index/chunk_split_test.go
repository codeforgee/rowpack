package index

import (
	"bytes"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// txnBody strips the IndexTxnHeader/Footer, leaving the chunk region that
// countChunks scans.
func txnBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var h format.IndexTxnHeader
	require.NoError(t, h.Unmarshal(data))
	end := format.IndexTxnHeaderSize + int(h.BodyBytes)
	require.LessOrEqual(t, end, len(data))
	return data[format.IndexTxnHeaderSize : format.IndexTxnHeaderSize+int(h.BodyBytes)]
}

// countChunks walks the chunk-header region of a stored body (the same magic
// scan parseTxnChunked uses) and returns how many chunks carry kind.
func countChunks(t *testing.T, body []byte, kind uint8) int {
	t.Helper()
	n := 0
	pos := 0
	for pos < len(body) && bytes.HasPrefix(body[pos:], []byte(format.MagicIndexChunkHdr)) {
		var h format.IndexChunkHeader
		require.NoError(t, h.Unmarshal(body[pos:]))
		if h.EntryKind == kind {
			n++
		}
		pos += format.IndexChunkHeaderSize + int(h.StoredBytes)
	}
	return n
}

// TestEmitChunksMetadataSplitRoundTrip drives emitChunks past
// IndexChunkTargetEntries so the metadata stream is cut into several chunks,
// then verifies the stored body parses back with every entry intact and in
// order (ordinal contiguity is enforced by the parser itself).
func TestEmitChunksMetadataSplitRoundTrip(t *testing.T) {
	const extra = 512
	n := format.IndexChunkTargetEntries + extra
	b := NewBuilder(0)
	require.NoError(t, b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID: 7, SnapshotType: format.SnapshotFull,
	}))
	for i := range n {
		require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
			SnapshotID: 7,
			ObjectID:   uint64(i + 1),
			Revision:   1,
			RecordType: 1,
			Operation:  format.OperationUpsert,
		}))
	}
	body, txn, err := b.BuildStored(nil, 0, nil, 0, 0)
	require.NoError(t, err)
	require.Len(t, txn.Metadata, n)

	chunks := countChunks(t, txnBody(t, body), format.IndexChunkKindMetadata)
	require.Greater(t, chunks, 1, "metadata stream must be split into multiple chunks")

	parsed, err := ParseTxn(body, nil)
	require.NoError(t, err)
	require.Len(t, parsed.Metadata, n)
	for i := range parsed.Metadata {
		require.Equal(t, uint64(i+1), parsed.Metadata[i].ObjectID, "entry %d out of order", i)
	}
}

// TestEmitChunksBlockSplitRoundTrip does the same for the block-entry stream.
func TestEmitChunksBlockSplitRoundTrip(t *testing.T) {
	const extra = 64
	n := format.IndexChunkTargetEntries + extra
	b := NewBuilder(0)
	require.NoError(t, b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID: 9, SnapshotType: format.SnapshotFull,
	}))
	for i := range n {
		require.NoError(t, b.AddBlock(format.BlockIndexEntry{
			BlockID:    uint64(i + 1),
			SnapshotID: 9,
			TableID:    1,
			BlockKind:  format.BlockKindRows,
		}))
	}
	body, txn, err := b.BuildStored(nil, 0, nil, 0, 0)
	require.NoError(t, err)
	require.Len(t, txn.Blocks, n)

	chunks := countChunks(t, txnBody(t, body), format.IndexChunkKindBlock)
	require.Greater(t, chunks, 1, "block stream must be split into multiple chunks")

	parsed, err := ParseTxn(body, nil)
	require.NoError(t, err)
	require.Len(t, parsed.Blocks, n)
	for i := range parsed.Blocks {
		require.Equal(t, uint64(i+1), parsed.Blocks[i].BlockID, "entry %d out of order", i)
	}
}

// TestEmitChunksSplitFirstOrdinals checks the cut bookkeeping directly: the
// second chunk of a split stream must start at ordinal IndexChunkTargetEntries
// (first chunk owns ordinals [0, target)).
func TestEmitChunksSplitFirstOrdinals(t *testing.T) {
	const extra = 3
	n := format.IndexChunkTargetEntries + extra
	b := NewBuilder(0)
	require.NoError(t, b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID: 11, SnapshotType: format.SnapshotFull,
	}))
	for i := range n {
		require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
			SnapshotID: 11, ObjectID: uint64(i + 1), RecordType: 1,
			Operation: format.OperationUpsert,
		}))
	}
	body, _, err := b.BuildStored(nil, 0, nil, 0, 0)
	require.NoError(t, err)
	region := txnBody(t, body)

	firstOrdinals := []uint32{}
	pos := 0
	for pos < len(region) && bytes.HasPrefix(region[pos:], []byte(format.MagicIndexChunkHdr)) {
		var h format.IndexChunkHeader
		require.NoError(t, h.Unmarshal(region[pos:]))
		if h.EntryKind == format.IndexChunkKindMetadata {
			firstOrdinals = append(firstOrdinals, h.FirstEntryOrdinal)
		}
		pos += format.IndexChunkHeaderSize + int(h.StoredBytes)
	}
	require.Len(t, firstOrdinals, 2)
	require.Equal(t, uint32(0), firstOrdinals[0])
	require.Equal(t, uint32(format.IndexChunkTargetEntries), firstOrdinals[1])
}
