package format

import (
	"bytes"
	"encoding/binary"
)

// IndexTxn chunk layout: the txn body is split into independently
// compressed/authenticated chunks, each preceded by a fixed IndexChunkHeader,
// followed by a plaintext Chunk Directory right before the IndexTxnFooter.
//
//	[IndexTxnHeader]
//	[IndexChunkHeader][stored payload]        // snapshot/metadata/block/row
//	[IndexChunkHeader][stored payload] ...
//	[IndexChunkDirectory]                     // plaintext, one entry per chunk
//	[IndexTxnFooter]
//
// IndexTxnHeader.BodyBytes counts the STORED bytes between header and footer
// (chunks + directory, including per-chunk GCM tags when encrypted) — the
// same semantics for plain and encrypted stores, so scanners hop the txn
// without understanding chunks and no read-side header patching is needed.

const (
	// MagicIndexChunkHdr opens every chunk header inside an index txn body.
	MagicIndexChunkHdr = "RPICHNK1"

	// IndexChunkHeaderSize is the fixed chunk header size.
	IndexChunkHeaderSize = 64
	// IndexChunkDirEntrySize is the fixed directory entry size.
	IndexChunkDirEntrySize = 32

	// Chunk entry kinds (EntryKind).
	IndexChunkKindSnapshot uint8 = 1
	IndexChunkKindMetadata uint8 = 2
	IndexChunkKindBlock    uint8 = 3
	IndexChunkKindRow      uint8 = 4

	// IndexChunkCompressionNone / Zstd mirror the block compression enum
	// values (kept as named constants so the chunk layout is self-describing).
	IndexChunkCompressionNone uint8 = 0
	IndexChunkCompressionZstd uint8 = 1

	// IndexChunkEncryptionNone / AESGCM mirror the file header encryption
	// enum values.
	IndexChunkEncryptionNone   uint8 = 0
	IndexChunkEncryptionAESGCM uint8 = uint8(EncAES256GCM)

	// IndexChunkTargetEntries is the default row-entry count per chunk.
	IndexChunkTargetEntries = 4096
	// IndexChunkTargetRawBytes is the default encoded size per chunk.
	IndexChunkTargetRawBytes = 256 << 10

	// Hard parse limits: enforced before any allocation derived from chunk
	// header fields (compression-bomb and overflow rejection).
	IndexChunkMaxEntries     = 1 << 20
	IndexChunkMaxRawBytes    = 16 << 20
	IndexChunkMaxStoredBytes = 16 << 20
)

// IndexChunkHeader is the fixed 64-byte header of one index txn chunk.
//
//	 0..7  Magic "RPICHNK"
//	 8..9  HeaderSize (=64)
//	10     EntryKind
//	11     Compression
//	12     Encryption
//	13     Flags (0)
//	14..15 reserved
//	16..19 ChunkSequence (0-based within the txn)
//	20..23 EntryCount (decoded entries in this chunk)
//	24..27 FirstEntryOrdinal (ordinal of the first entry within its kind stream)
//	28..31 RawBytes (encoded bytes before compression)
//	32..35 StoredBytes (final payload bytes: compressed, + tag when encrypted)
//	36..39 KeyEpoch
//	40..43 PayloadCRC32C (over the stored payload; diagnostic)
//	44..47 HeaderCRC32C (over bytes 0..43)
//	48..63 reserved (zero)
type IndexChunkHeader struct {
	EntryKind         uint8
	Compression       uint8
	Encryption        uint8
	Flags             uint8
	ChunkSequence     uint32
	EntryCount        uint32
	FirstEntryOrdinal uint32
	RawBytes          uint32
	StoredBytes       uint32
	KeyEpoch          uint32
	PayloadCRC32C     uint32
}

// Size returns the serialized size.
func (h *IndexChunkHeader) Size() int { return IndexChunkHeaderSize }

// MarshalTo writes h into dst.
func (h *IndexChunkHeader) MarshalTo(dst []byte) error {
	if len(dst) < IndexChunkHeaderSize {
		return formatError("IndexChunkHeader", -1, "destination too short: have %d want %d", len(dst), IndexChunkHeaderSize)
	}
	for i := range dst[:IndexChunkHeaderSize] {
		dst[i] = 0
	}
	copy(dst[0:8], MagicIndexChunkHdr)
	putU16(dst[8:], IndexChunkHeaderSize)
	dst[10] = h.EntryKind
	dst[11] = h.Compression
	dst[12] = h.Encryption
	dst[13] = h.Flags
	putU32(dst[16:], h.ChunkSequence)
	putU32(dst[20:], h.EntryCount)
	putU32(dst[24:], h.FirstEntryOrdinal)
	putU32(dst[28:], h.RawBytes)
	putU32(dst[32:], h.StoredBytes)
	putU32(dst[36:], h.KeyEpoch)
	putU32(dst[40:], h.PayloadCRC32C)
	finalizeCRC(dst[:IndexChunkHeaderSize], 44)
	return nil
}

// Unmarshal validates src and fills h.
func (h *IndexChunkHeader) Unmarshal(src []byte) error {
	if len(src) < IndexChunkHeaderSize {
		return formatError("IndexChunkHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicIndexChunkHdr)) {
		return formatError("IndexChunkHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if sz := binary.LittleEndian.Uint16(src[8:]); sz != IndexChunkHeaderSize {
		return formatError("IndexChunkHeader", 8, "%s: size=%d want %d", errBadSize, sz, IndexChunkHeaderSize)
	}
	if _, err := verifyCRC(src[:IndexChunkHeaderSize], 44); err != nil {
		return formatError("IndexChunkHeader", 44, "%v", err)
	}
	h.EntryKind = src[10]
	h.Compression = src[11]
	h.Encryption = src[12]
	h.Flags = src[13]
	h.ChunkSequence = binary.LittleEndian.Uint32(src[16:])
	h.EntryCount = binary.LittleEndian.Uint32(src[20:])
	h.FirstEntryOrdinal = binary.LittleEndian.Uint32(src[24:])
	h.RawBytes = binary.LittleEndian.Uint32(src[28:])
	h.StoredBytes = binary.LittleEndian.Uint32(src[32:])
	h.KeyEpoch = binary.LittleEndian.Uint32(src[36:])
	h.PayloadCRC32C = binary.LittleEndian.Uint32(src[40:])
	if h.Flags != 0 {
		return formatError("IndexChunkHeader", 13, "unsupported flags %d", h.Flags)
	}
	switch h.EntryKind {
	case IndexChunkKindSnapshot, IndexChunkKindMetadata, IndexChunkKindBlock, IndexChunkKindRow:
	default:
		return formatError("IndexChunkHeader", 10, "unknown entry kind %d", h.EntryKind)
	}
	switch h.Compression {
	case IndexChunkCompressionNone, IndexChunkCompressionZstd:
	default:
		return formatError("IndexChunkHeader", 11, "unknown compression %d", h.Compression)
	}
	switch h.Encryption {
	case IndexChunkEncryptionNone, IndexChunkEncryptionAESGCM:
	default:
		return formatError("IndexChunkHeader", 12, "unknown encryption %d", h.Encryption)
	}
	return nil
}

// CheckLimits enforces the hard parse caps on the size/count fields before
// any allocation derived from them.
func (h *IndexChunkHeader) CheckLimits() error {
	if h.EntryCount == 0 || h.EntryCount > IndexChunkMaxEntries {
		return formatError("IndexChunkHeader", 20, "entry count %d outside [1,%d]", h.EntryCount, IndexChunkMaxEntries)
	}
	if h.RawBytes == 0 || h.RawBytes > IndexChunkMaxRawBytes {
		return formatError("IndexChunkHeader", 28, "raw bytes %d outside [1,%d]", h.RawBytes, IndexChunkMaxRawBytes)
	}
	if h.StoredBytes == 0 || h.StoredBytes > IndexChunkMaxStoredBytes {
		return formatError("IndexChunkHeader", 32, "stored bytes %d outside [1,%d]", h.StoredBytes, IndexChunkMaxStoredBytes)
	}
	plainStored := h.StoredBytes
	if h.Encryption == IndexChunkEncryptionAESGCM {
		if plainStored < AESGCMTagLen {
			return formatError("IndexChunkHeader", 32, "encrypted chunk stored %d is smaller than tag", h.StoredBytes)
		}
		plainStored -= AESGCMTagLen
	}
	if h.Compression == IndexChunkCompressionNone && h.RawBytes != plainStored {
		return formatError("IndexChunkHeader", 32, "none-compressed chunk stored %d != raw %d", h.StoredBytes, h.RawBytes)
	}
	return nil
}

// IndexChunkDirEntry is the fixed 32-byte directory record of one chunk.
// The directory is plaintext (even in encrypted stores) so readers can locate
// chunks without a key; it reveals only chunk sizes and entry counts.
//
//	 0..3  ChunkSequence
//	 4..7  EntryCount
//	 8..11 FirstEntryOrdinal
//	12..15 RawBytes
//	16..19 StoredBytes (payload only; the chunk header adds a fixed 64B)
//	20    EntryKind
//	21..23 reserved
//	24..31 RegionOffset (chunk header offset relative to the IndexTxn body start)
type IndexChunkDirEntry struct {
	ChunkSequence     uint32
	EntryCount        uint32
	FirstEntryOrdinal uint32
	RawBytes          uint32
	StoredBytes       uint32
	EntryKind         uint8
	RegionOffset      uint64
}

// Size returns the serialized size.
func (e *IndexChunkDirEntry) Size() int { return IndexChunkDirEntrySize }

// MarshalTo writes e into dst.
func (e *IndexChunkDirEntry) MarshalTo(dst []byte) error {
	if len(dst) < IndexChunkDirEntrySize {
		return formatError("IndexChunkDirEntry", -1, "destination too short: have %d want %d", len(dst), IndexChunkDirEntrySize)
	}
	for i := range dst[:IndexChunkDirEntrySize] {
		dst[i] = 0
	}
	putU32(dst[0:], e.ChunkSequence)
	putU32(dst[4:], e.EntryCount)
	putU32(dst[8:], e.FirstEntryOrdinal)
	putU32(dst[12:], e.RawBytes)
	putU32(dst[16:], e.StoredBytes)
	dst[20] = e.EntryKind
	putU64(dst[24:], e.RegionOffset)
	return nil
}

// Unmarshal fills e from src (no CRC: the directory is covered by the txn
// footer's region CRC and each chunk header's own CRC).
func (e *IndexChunkDirEntry) Unmarshal(src []byte) error {
	if len(src) < IndexChunkDirEntrySize {
		return formatError("IndexChunkDirEntry", -1, errShortInput)
	}
	e.ChunkSequence = binary.LittleEndian.Uint32(src[0:])
	e.EntryCount = binary.LittleEndian.Uint32(src[4:])
	e.FirstEntryOrdinal = binary.LittleEndian.Uint32(src[8:])
	e.RawBytes = binary.LittleEndian.Uint32(src[12:])
	e.StoredBytes = binary.LittleEndian.Uint32(src[16:])
	e.EntryKind = src[20]
	e.RegionOffset = binary.LittleEndian.Uint64(src[24:])
	return nil
}

// ParseIndexChunkDirectory parses the directory region (n*32 bytes) and
// returns one entry per chunk in sequence order.
func ParseIndexChunkDirectory(dir []byte) ([]IndexChunkDirEntry, error) {
	if len(dir)%IndexChunkDirEntrySize != 0 {
		return nil, formatError("IndexChunkDirectory", -1, "directory %d bytes not a multiple of %d", len(dir), IndexChunkDirEntrySize)
	}
	n := len(dir) / IndexChunkDirEntrySize
	if n == 0 {
		return nil, formatError("IndexChunkDirectory", -1, "empty directory")
	}
	out := make([]IndexChunkDirEntry, n)
	for i := range out {
		if err := out[i].Unmarshal(dir[i*IndexChunkDirEntrySize : (i+1)*IndexChunkDirEntrySize]); err != nil {
			return nil, err
		}
	}
	return out, nil
}
