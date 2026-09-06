package fileformat

import (
	"bytes"
	"encoding/binary"
)

// BlockHeader is the fixed 64-byte header preceding a stored (possibly
// compressed) block payload. RawCRC32C covers the uncompressed payload;
// HeaderCRC32C covers the whole 64-byte header with the header CRC field
// zeroed.
type BlockHeader struct {
	BlockKind   BlockKind
	Compression Compression
	BlockID     uint64
	SnapshotID  uint64
	TableID     uint32
	ItemCount   uint32
	RawSize     uint32
	StoredSize  uint32
	RawCRC32C   uint32
}

// Size returns the serialized size.
func (h *BlockHeader) Size() int { return BlockHeaderSize }

// MarshalTo writes h into dst. HeaderCRC32C is computed and stored.
func (h *BlockHeader) MarshalTo(dst []byte) error {
	if len(dst) < BlockHeaderSize {
		return formatError("BlockHeader", -1, "destination too short: have %d want %d", len(dst), BlockHeaderSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], MagicBlockHdr)
	putU32(dst[8:], BlockHeaderSize)
	dst[12] = byte(h.BlockKind)
	dst[13] = byte(h.Compression)
	putU64(dst[16:], h.BlockID)
	putU64(dst[24:], h.SnapshotID)
	putU32(dst[32:], h.TableID)
	putU32(dst[36:], h.ItemCount)
	putU32(dst[40:], h.RawSize)
	putU32(dst[44:], h.StoredSize)
	putU32(dst[48:], h.RawCRC32C)
	// offset 52..60 header CRC
	finalizeCRC(dst[:BlockHeaderSize], 52)
	// offset 56..64 reserved (zeroed above)
	return nil
}

// Unmarshal validates src and fills h.
func (h *BlockHeader) Unmarshal(src []byte) error {
	if len(src) < BlockHeaderSize {
		return formatError("BlockHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicBlockHdr)) {
		return formatError("BlockHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if sz := binary.LittleEndian.Uint32(src[8:]); sz != BlockHeaderSize {
		return formatError("BlockHeader", 8, "%s: size=%d want %d", errBadSize, sz, BlockHeaderSize)
	}
	if _, err := verifyCRC(src[:BlockHeaderSize], 52); err != nil {
		return formatError("BlockHeader", 52, "%v", err)
	}
	h.BlockKind = BlockKind(src[12])
	h.Compression = Compression(src[13])
	h.BlockID = binary.LittleEndian.Uint64(src[16:])
	h.SnapshotID = binary.LittleEndian.Uint64(src[24:])
	h.TableID = binary.LittleEndian.Uint32(src[32:])
	h.ItemCount = binary.LittleEndian.Uint32(src[36:])
	h.RawSize = binary.LittleEndian.Uint32(src[40:])
	h.StoredSize = binary.LittleEndian.Uint32(src[44:])
	h.RawCRC32C = binary.LittleEndian.Uint32(src[48:])
	return nil
}
