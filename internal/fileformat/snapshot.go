package fileformat

import (
	"bytes"
	"encoding/binary"
)

// SnapshotHeader is the fixed 96-byte header written before a snapshot's
// blocks in the .rpk file. A complete, CRC-valid SnapshotFooter is the
// authoritative commit marker; the header alone is not.
type SnapshotHeader struct {
	SnapshotType     SnapshotType
	AllowEmpty       bool // Flags bit 0
	SnapshotID       uint64
	ParentSnapshotID uint64
	CreatedUnixNano  int64
	FirstBlockID     uint64
	WriterNonce      uint64
}

// Size returns the serialized size.
func (h *SnapshotHeader) Size() int { return SnapshotHeaderSize }

// MarshalTo writes h into dst.
func (h *SnapshotHeader) MarshalTo(dst []byte) error {
	if len(dst) < SnapshotHeaderSize {
		return formatError("SnapshotHeader", -1, "destination too short: have %d want %d", len(dst), SnapshotHeaderSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], MagicSnapshotHdr)
	putU32(dst[8:], SnapshotHeaderSize)
	dst[12] = byte(h.SnapshotType)
	if h.AllowEmpty {
		dst[13] = 1
	}
	putU64(dst[16:], h.SnapshotID)
	putU64(dst[24:], h.ParentSnapshotID)
	putU64(dst[32:], uint64(h.CreatedUnixNano))
	putU64(dst[40:], h.FirstBlockID)
	putU64(dst[48:], h.WriterNonce)
	// offset 56..88 reserved (zeroed above)
	finalizeCRC(dst[:SnapshotHeaderSize], 88)
	// offset 92..96 ReservedCRC (zeroed above)
	return nil
}

// Unmarshal validates src and fills h.
func (h *SnapshotHeader) Unmarshal(src []byte) error {
	if len(src) < SnapshotHeaderSize {
		return formatError("SnapshotHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicSnapshotHdr)) {
		return formatError("SnapshotHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if sz := binary.LittleEndian.Uint32(src[8:]); sz != SnapshotHeaderSize {
		return formatError("SnapshotHeader", 8, "%s: size=%d want %d", errBadSize, sz, SnapshotHeaderSize)
	}
	if _, err := verifyCRC(src[:SnapshotHeaderSize], 88); err != nil {
		return formatError("SnapshotHeader", 88, "%v", err)
	}
	h.SnapshotType = SnapshotType(src[12])
	h.AllowEmpty = src[13]&1 != 0
	h.SnapshotID = binary.LittleEndian.Uint64(src[16:])
	h.ParentSnapshotID = binary.LittleEndian.Uint64(src[24:])
	h.CreatedUnixNano = int64(binary.LittleEndian.Uint64(src[32:]))
	h.FirstBlockID = binary.LittleEndian.Uint64(src[40:])
	h.WriterNonce = binary.LittleEndian.Uint64(src[48:])
	return nil
}

// SnapshotFooter is the fixed 96-byte snapshot commit marker and the
// authoritative proof that a snapshot is committed. SnapshotEndOffset is the
// 8-byte aligned position just after the footer.
type SnapshotFooter struct {
	SnapshotType        SnapshotType
	SnapshotID          uint64
	ParentSnapshotID    uint64
	SnapshotStartOffset uint64
	SnapshotEndOffset   uint64
	FirstBlockID        uint64
	BlockCount          uint32
	MetadataBlockCount  uint32
	RowRecordCount      uint64
	RawBytes            uint64
	BlocksCRC32C        uint32 // CRC of the block header CRC value string
}

// Size returns the serialized size.
func (f *SnapshotFooter) Size() int { return SnapshotFooterSize }

// MarshalTo writes f into dst.
func (f *SnapshotFooter) MarshalTo(dst []byte) error {
	if len(dst) < SnapshotFooterSize {
		return formatError("SnapshotFooter", -1, "destination too short: have %d want %d", len(dst), SnapshotFooterSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], MagicSnapshotFtr)
	putU32(dst[8:], SnapshotFooterSize)
	dst[12] = byte(f.SnapshotType)
	putU64(dst[16:], f.SnapshotID)
	putU64(dst[24:], f.ParentSnapshotID)
	putU64(dst[32:], f.SnapshotStartOffset)
	putU64(dst[40:], f.SnapshotEndOffset)
	putU64(dst[48:], f.FirstBlockID)
	putU32(dst[56:], f.BlockCount)
	putU32(dst[60:], f.MetadataBlockCount)
	putU64(dst[64:], f.RowRecordCount)
	putU64(dst[72:], f.RawBytes)
	putU32(dst[80:], f.BlocksCRC32C)
	// offset 88..96 reserved (zeroed above)
	finalizeCRC(dst[:SnapshotFooterSize], 84)
	return nil
}

// Unmarshal validates src and fills f.
func (f *SnapshotFooter) Unmarshal(src []byte) error {
	if len(src) < SnapshotFooterSize {
		return formatError("SnapshotFooter", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicSnapshotFtr)) {
		return formatError("SnapshotFooter", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if sz := binary.LittleEndian.Uint32(src[8:]); sz != SnapshotFooterSize {
		return formatError("SnapshotFooter", 8, "%s: size=%d want %d", errBadSize, sz, SnapshotFooterSize)
	}
	if _, err := verifyCRC(src[:SnapshotFooterSize], 84); err != nil {
		return formatError("SnapshotFooter", 84, "%v", err)
	}
	f.SnapshotType = SnapshotType(src[12])
	f.SnapshotID = binary.LittleEndian.Uint64(src[16:])
	f.ParentSnapshotID = binary.LittleEndian.Uint64(src[24:])
	f.SnapshotStartOffset = binary.LittleEndian.Uint64(src[32:])
	f.SnapshotEndOffset = binary.LittleEndian.Uint64(src[40:])
	f.FirstBlockID = binary.LittleEndian.Uint64(src[48:])
	f.BlockCount = binary.LittleEndian.Uint32(src[56:])
	f.MetadataBlockCount = binary.LittleEndian.Uint32(src[60:])
	f.RowRecordCount = binary.LittleEndian.Uint64(src[64:])
	f.RawBytes = binary.LittleEndian.Uint64(src[72:])
	f.BlocksCRC32C = binary.LittleEndian.Uint32(src[80:])
	return nil
}
