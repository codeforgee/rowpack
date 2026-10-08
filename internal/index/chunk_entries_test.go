package index

import (
	"strings"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// metaFixture returns a metadata stream exercising every encoded field.
func metaFixture(n int) []format.MetadataIndexEntry {
	out := make([]format.MetadataIndexEntry, n)
	for i := range out {
		out[i] = format.MetadataIndexEntry{
			SnapshotID: 42,
			ObjectID:   uint64(100 + i),
			RecordType: uint32(format.RecordTable),
			Revision:   1,
			Operation:  format.OperationUpsert,
		}
		if i%3 == 0 {
			out[i].RecordType = uint32(format.RecordColumn)
		}
		if i%5 == 0 {
			out[i].Revision = 7
		}
		if i%7 == 0 {
			out[i].Operation = format.OperationDelete
		}
		if i%2 == 0 {
			out[i].Critical = true
		}
		out[i].BlockID = uint64(i / 16)
		out[i].ItemOrdinal = uint32(i % 16)
	}
	return out
}

// blockFixture returns a block stream with runs on table/kind/compression and
// increasing block IDs/offsets.
func blockFixture(n int) []format.BlockIndexEntry {
	out := make([]format.BlockIndexEntry, n)
	off := uint64(128)
	for i := range out {
		out[i] = format.BlockIndexEntry{
			BlockID:     uint64(1000 + i),
			SnapshotID:  42,
			TableID:     3,
			BlockKind:   format.BlockKindRows,
			Compression: format.CompressionZstd,
			DataOffset:  off,
			RawSize:     200_000,
			StoredSize:  90_000,
			ItemCount:   512,
			RawCRC32C:   0xDEADBEEF,
		}
		off += 90_000
		if i%9 == 0 {
			out[i].TableID = uint32(i + 1)
			out[i].BlockKind = format.BlockKindMetadata
			out[i].Compression = format.CompressionNone
		}
	}
	return out
}

func encodeMetaStream(entries []format.MetadataIndexEntry) []byte {
	enc := &metaEncoder{}
	var dst []byte
	for i := range entries {
		dst = enc.add(dst, entries[i])
	}
	return dst
}

func encodeBlockStream(entries []format.BlockIndexEntry) []byte {
	enc := &blockEncoder{}
	var dst []byte
	for i := range entries {
		dst = enc.add(dst, entries[i])
	}
	return dst
}

func TestMetaStreamRoundTrip(t *testing.T) {
	entries := metaFixture(257)
	var got []format.MetadataIndexEntry
	require.NoError(t, decodeMetadataChunk(encodeMetaStream(entries), uint32(len(entries)), 42,
		func(e format.MetadataIndexEntry) error { got = append(got, e); return nil }))
	require.Equal(t, entries, got, "decoded entries must equal the inputs")
}

func TestBlockStreamRoundTrip(t *testing.T) {
	entries := blockFixture(257)
	var got []format.BlockIndexEntry
	require.NoError(t, decodeBlockChunk(encodeBlockStream(entries), uint32(len(entries)), 42,
		func(e format.BlockIndexEntry) error { got = append(got, e); return nil }))
	require.Equal(t, entries, got, "decoded entries must equal the inputs")
}

func TestMetaStreamRestartAtChunkBoundary(t *testing.T) {
	// Same cut shape as the writer: restart() re-bases at each cut, every
	// segment decodes standalone.
	entries := metaFixture(5000)
	enc := &metaEncoder{}
	var segs [][]byte
	cur := []byte{}
	for i := range entries {
		cur = enc.add(cur, entries[i])
		if (i+1)%1000 == 0 {
			segs = append(segs, cur)
			cur = nil
			enc.restart()
		}
	}
	require.Len(t, segs, 5)
	var got []format.MetadataIndexEntry
	for k, seg := range segs {
		base := uint32(k * 1000)
		require.NoError(t, decodeMetadataChunk(seg, 1000, 9,
			func(e format.MetadataIndexEntry) error { got = append(got, e); return nil }),
			"segment %d must decode standalone", k)
		_ = base
	}
	for i := range entries {
		entries[i].SnapshotID = 9
	}
	require.Equal(t, entries, got)
}

func TestEncodedStreamShrinks(t *testing.T) {
	// Must beat the old fixed-size concatenation even before zstd.
	meta := metaFixture(4096)
	require.Less(t, len(encodeMetaStream(meta)), len(meta)*format.MetadataIndexEntrySize/2,
		"metadata stream must be less than half the fixed layout")
	blk := blockFixture(4096)
	require.Less(t, len(encodeBlockStream(blk)), len(blk)*format.BlockIndexEntrySize/2,
		"block stream must be less than half the fixed layout")
}

func TestDecodeMetadataChunkRejects(t *testing.T) {
	entries := metaFixture(4)
	raw := encodeMetaStream(entries)
	sink := func(format.MetadataIndexEntry) error { return nil }

	t.Run("truncated", func(t *testing.T) {
		for cut := 1; cut < len(raw); cut++ {
			require.Error(t, decodeMetadataChunk(raw[:cut], uint32(len(entries)), 1, sink),
				"cut %d accepted", cut)
		}
	})
	t.Run("trailing slack", func(t *testing.T) {
		require.Error(t, decodeMetadataChunk(append(raw, 0), uint32(len(entries)), 1, sink))
	})
	t.Run("count overrun", func(t *testing.T) {
		require.Error(t, decodeMetadataChunk(raw, uint32(len(entries)+1), 1, sink))
	})
	t.Run("unknown tag bits", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[7] |= 0xF0 // tag byte of entry 1 (7-byte first entry)
		err := decodeMetadataChunk(bad, uint32(len(entries)), 1, sink)
		require.ErrorContains(t, err, "unknown tag bits")
	})
	t.Run("uint32 overflow", func(t *testing.T) {
		// Replace the record-type uvarint with 1<<32 (0x80×4, 0x10) after the
		// 1-byte objectID of the absolute first entry.
		raw := encodeMetaStream(metaFixture(1))
		craft := append(raw[:1:1], 0x80, 0x80, 0x80, 0x80, 0x10)
		err := decodeMetadataChunk(craft, 1, 1, func(format.MetadataIndexEntry) error { return nil })
		require.ErrorContains(t, err, "exceeds uint32")
	})
	t.Run("invalid operation", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[5] = 9 // operation byte of the absolute first entry
		err := decodeMetadataChunk(bad, uint32(len(entries)), 1, sink)
		require.ErrorContains(t, err, "unknown operation 9")
	})
}

func TestDecodeBlockChunkRejects(t *testing.T) {
	entries := blockFixture(4)
	raw := encodeBlockStream(entries)
	sink := func(format.BlockIndexEntry) error { return nil }

	t.Run("truncated", func(t *testing.T) {
		for cut := 1; cut < len(raw); cut++ {
			require.Error(t, decodeBlockChunk(raw[:cut], uint32(len(entries)), 1, sink),
				"cut %d accepted", cut)
		}
	})
	t.Run("trailing slack", func(t *testing.T) {
		require.Error(t, decodeBlockChunk(append(raw, 0), uint32(len(entries)), 1, sink))
	})
	t.Run("unknown tag bits", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		base := len(encodeBlockStream(entries[:1])) // first entry length
		bad[base] |= 0xF8                           // tag byte of entry 1
		err := decodeBlockChunk(bad, uint32(len(entries)), 1, sink)
		require.ErrorContains(t, err, "unknown tag bits")
	})
	t.Run("invalid block kind", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[3] = 99 // block kind byte of the absolute first entry
		err := decodeBlockChunk(bad, uint32(len(entries)), 1, sink)
		require.ErrorContains(t, err, "unknown block kind 99")
	})
	t.Run("invalid compression", func(t *testing.T) {
		bad := append([]byte(nil), raw...)
		bad[4] = 7 // compression byte of the absolute first entry
		err := decodeBlockChunk(bad, uint32(len(entries)), 1, sink)
		require.ErrorContains(t, err, "unknown compression 7")
	})
}

func TestDecodeEmptyChunkWithEntriesRejected(t *testing.T) {
	err := decodeMetadataChunk(nil, 1, 1, func(format.MetadataIndexEntry) error { return nil })
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "entry 0"))
}
