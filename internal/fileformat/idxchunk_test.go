package fileformat

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIndexChunkFrozenConstants locks the v2 chunk format identifiers and
// sizes. Changing any value is an on-disk format change and requires a new
// format version and golden family.
func TestIndexChunkFrozenConstants(t *testing.T) {
	require.Equal(t, "RPICHNK1", MagicIndexChunkHdr)
	require.Equal(t, 64, IndexChunkHeaderSize)
	require.Equal(t, 32, IndexChunkDirEntrySize)
	require.Equal(t, uint8(1), IndexChunkKindSnapshot)
	require.Equal(t, uint8(2), IndexChunkKindMetadata)
	require.Equal(t, uint8(3), IndexChunkKindBlock)
	require.Equal(t, uint8(4), IndexChunkKindRow)
	require.Equal(t, uint8(0), IndexChunkCompressionNone)
	require.Equal(t, uint8(1), IndexChunkCompressionZstd)
	require.Equal(t, uint8(0), IndexChunkEncryptionNone)
	require.Equal(t, uint8(1), IndexChunkEncryptionAESGCM)
}

func TestIndexChunkHeaderFrozenLayout(t *testing.T) {
	h := IndexChunkHeader{
		EntryKind:         IndexChunkKindRow,
		Compression:       IndexChunkCompressionZstd,
		Encryption:        IndexChunkEncryptionAESGCM,
		ChunkSequence:     0x01020304,
		EntryCount:        0x11121314,
		FirstEntryOrdinal: 0x21222324,
		RawBytes:          0x31323334,
		StoredBytes:       0x41424344,
		KeyEpoch:          0x51525354,
		PayloadCRC32C:     0x61626364,
	}
	var buf [IndexChunkHeaderSize]byte
	require.NoError(t, h.MarshalTo(buf[:]))
	require.Equal(t, MagicIndexChunkHdr, string(buf[0:8]))
	require.Equal(t, uint16(IndexChunkHeaderSize), binary.LittleEndian.Uint16(buf[8:10]))
	require.Equal(t, h.EntryKind, buf[10])
	require.Equal(t, h.Compression, buf[11])
	require.Equal(t, h.Encryption, buf[12])
	require.Equal(t, h.ChunkSequence, binary.LittleEndian.Uint32(buf[16:20]))
	require.Equal(t, h.EntryCount, binary.LittleEndian.Uint32(buf[20:24]))
	require.Equal(t, h.FirstEntryOrdinal, binary.LittleEndian.Uint32(buf[24:28]))
	require.Equal(t, h.RawBytes, binary.LittleEndian.Uint32(buf[28:32]))
	require.Equal(t, h.StoredBytes, binary.LittleEndian.Uint32(buf[32:36]))
	require.Equal(t, h.KeyEpoch, binary.LittleEndian.Uint32(buf[36:40]))
	require.Equal(t, h.PayloadCRC32C, binary.LittleEndian.Uint32(buf[40:44]))
	require.NotZero(t, binary.LittleEndian.Uint32(buf[44:48]))
	require.Equal(t, make([]byte, 16), buf[48:64])

	var got IndexChunkHeader
	require.NoError(t, got.Unmarshal(buf[:]))
	require.Equal(t, h, got)
}

func TestIndexChunkDirectoryFrozenLayout(t *testing.T) {
	e := IndexChunkDirEntry{
		ChunkSequence:     0x01020304,
		EntryCount:        0x11121314,
		FirstEntryOrdinal: 0x21222324,
		RawBytes:          0x31323334,
		StoredBytes:       0x41424344,
		EntryKind:         IndexChunkKindBlock,
		RegionOffset:      0x5152535455565758,
	}
	var buf [IndexChunkDirEntrySize]byte
	require.NoError(t, e.MarshalTo(buf[:]))
	require.Equal(t, e.ChunkSequence, binary.LittleEndian.Uint32(buf[0:4]))
	require.Equal(t, e.EntryCount, binary.LittleEndian.Uint32(buf[4:8]))
	require.Equal(t, e.FirstEntryOrdinal, binary.LittleEndian.Uint32(buf[8:12]))
	require.Equal(t, e.RawBytes, binary.LittleEndian.Uint32(buf[12:16]))
	require.Equal(t, e.StoredBytes, binary.LittleEndian.Uint32(buf[16:20]))
	require.Equal(t, e.EntryKind, buf[20])
	require.Equal(t, []byte{0, 0, 0}, buf[21:24])
	require.Equal(t, e.RegionOffset, binary.LittleEndian.Uint64(buf[24:32]))

	var got IndexChunkDirEntry
	require.NoError(t, got.Unmarshal(buf[:]))
	require.Equal(t, e, got)
}
