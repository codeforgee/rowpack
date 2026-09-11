package format

import (
	"bytes"
	"encoding/binary"
)

// BlockHeader is the fixed 64-byte header preceding a stored (possibly
// compressed, possibly encrypted) block payload. RawCRC32C covers the
// uncompressed payload; HeaderCRC32C covers the whole 64-byte header with the
// header CRC field zeroed. When Encrypted is set, the payload was compressed
// first and then sealed with AES-256-GCM: StoredSize is the ciphertext
// length (StoredSize = plaintext + AESGCMTagLen) and KeyEpoch selects the
// key at read time.
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
	Encrypted   bool   // Flags bit 0
	KeyEpoch    uint32 // first 4 reserved bytes
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
	if h.Encrypted {
		putU16(dst[14:], 1) // Flags bit 0
	}
	putU64(dst[16:], h.BlockID)
	putU64(dst[24:], h.SnapshotID)
	putU32(dst[32:], h.TableID)
	putU32(dst[36:], h.ItemCount)
	putU32(dst[40:], h.RawSize)
	putU32(dst[44:], h.StoredSize)
	putU32(dst[48:], h.RawCRC32C)
	putU32(dst[BlockHeaderKeyEpochOffset:], h.KeyEpoch)
	// offset 52..60 header CRC
	finalizeCRC(dst[:BlockHeaderSize], 52)
	// offset 60..64 reserved (zeroed above)
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
	h.Encrypted = src[14]&1 != 0
	h.BlockID = binary.LittleEndian.Uint64(src[16:])
	h.SnapshotID = binary.LittleEndian.Uint64(src[24:])
	h.TableID = binary.LittleEndian.Uint32(src[32:])
	h.ItemCount = binary.LittleEndian.Uint32(src[36:])
	h.RawSize = binary.LittleEndian.Uint32(src[40:])
	h.StoredSize = binary.LittleEndian.Uint32(src[44:])
	h.RawCRC32C = binary.LittleEndian.Uint32(src[48:])
	h.KeyEpoch = binary.LittleEndian.Uint32(src[BlockHeaderKeyEpochOffset:])
	return nil
}
