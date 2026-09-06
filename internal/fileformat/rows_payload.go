package fileformat

import (
	"bytes"
	"encoding/binary"
)

// RowsPayloadHeader is the fixed 32-byte header of the uncompressed Rows
// Block payload, followed by RowDirectoryEntry items and record bytes.
type RowsPayloadHeader struct {
	ItemCount      uint32
	DirectoryBytes uint32
	RecordsBytes   uint64
}

// Size returns the serialized size.
func (h *RowsPayloadHeader) Size() int { return RowsPayloadHeaderSize }

// MarshalTo writes h into dst.
func (h *RowsPayloadHeader) MarshalTo(dst []byte) error {
	if len(dst) < RowsPayloadHeaderSize {
		return formatError("RowsPayloadHeader", -1, "destination too short: have %d want %d", len(dst), RowsPayloadHeaderSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], MagicRowsPayload)
	putU32(dst[8:], RowsPayloadVersion)
	putU32(dst[12:], RowDirectoryEntrySize)
	putU32(dst[16:], h.ItemCount)
	putU32(dst[20:], h.DirectoryBytes)
	putU64(dst[24:], h.RecordsBytes)
	return nil
}

// Unmarshal validates src and fills h. ItemCount and DirectoryBytes must be
// mutually consistent.
func (h *RowsPayloadHeader) Unmarshal(src []byte) error {
	if len(src) < RowsPayloadHeaderSize {
		return formatError("RowsPayloadHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicRowsPayload)) {
		return formatError("RowsPayloadHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if v := binary.LittleEndian.Uint32(src[8:]); v != RowsPayloadVersion {
		return formatError("RowsPayloadHeader", 8, "unsupported payload version %d", v)
	}
	if v := binary.LittleEndian.Uint32(src[12:]); v != RowDirectoryEntrySize {
		return formatError("RowsPayloadHeader", 12, "directory entry size %d, want %d", v, RowDirectoryEntrySize)
	}
	h.ItemCount = binary.LittleEndian.Uint32(src[16:])
	h.DirectoryBytes = binary.LittleEndian.Uint32(src[20:])
	if h.DirectoryBytes != h.ItemCount*RowDirectoryEntrySize {
		return formatError("RowsPayloadHeader", 20, "directory bytes %d != itemCount %d * %d", h.DirectoryBytes, h.ItemCount, RowDirectoryEntrySize)
	}
	h.RecordsBytes = binary.LittleEndian.Uint64(src[24:])
	return nil
}

// RowDirectoryEntry is the fixed 24-byte per-record directory entry of a Rows
// Block payload. The directory is stored in call order.
type RowDirectoryEntry struct {
	RowID         uint64
	RecordOffset  uint32
	RecordLength  uint32
	ChangeType    ChangeType
	SchemaVersion uint32
}

// Size returns the serialized size.
func (e *RowDirectoryEntry) Size() int { return RowDirectoryEntrySize }

// MarshalTo writes e into dst.
func (e *RowDirectoryEntry) MarshalTo(dst []byte) error {
	if len(dst) < RowDirectoryEntrySize {
		return formatError("RowDirectoryEntry", -1, "destination too short")
	}
	for i := range dst {
		dst[i] = 0
	}
	putU64(dst[0:], e.RowID)
	putU32(dst[8:], e.RecordOffset)
	putU32(dst[12:], e.RecordLength)
	dst[16] = byte(e.ChangeType)
	// flags and reserved zero
	putU32(dst[20:], e.SchemaVersion)
	return nil
}

// Unmarshal validates src and fills e.
func (e *RowDirectoryEntry) Unmarshal(src []byte) error {
	if len(src) < RowDirectoryEntrySize {
		return formatError("RowDirectoryEntry", -1, errShortInput)
	}
	e.RowID = binary.LittleEndian.Uint64(src[0:])
	e.RecordOffset = binary.LittleEndian.Uint32(src[8:])
	e.RecordLength = binary.LittleEndian.Uint32(src[12:])
	e.ChangeType = ChangeType(src[16])
	e.SchemaVersion = binary.LittleEndian.Uint32(src[20:])
	return nil
}

// RowRecordHeader is the fixed 24-byte header of one row record inside the
// Rows Block payload. The record body follows immediately; a DELETE record
// carries RowEncoding=0, RowLength=0 and RowCRC=0.
type RowRecordHeader struct {
	RowID         uint64
	SchemaVersion uint32
	ChangeType    ChangeType
	RowEncoding   RowEncoding
	RowLength     uint32
	RowCRC32C     uint32
}

// Size returns the serialized size.
func (h *RowRecordHeader) Size() int { return RowRecordHeaderSize }

// MarshalTo writes h into dst.
func (h *RowRecordHeader) MarshalTo(dst []byte) error {
	if len(dst) < RowRecordHeaderSize {
		return formatError("RowRecordHeader", -1, "destination too short")
	}
	for i := range dst {
		dst[i] = 0
	}
	putU64(dst[0:], h.RowID)
	putU32(dst[8:], h.SchemaVersion)
	dst[12] = byte(h.ChangeType)
	dst[13] = byte(h.RowEncoding)
	putU32(dst[16:], h.RowLength)
	putU32(dst[20:], h.RowCRC32C)
	return nil
}

// Unmarshal validates src and fills h.
func (h *RowRecordHeader) Unmarshal(src []byte) error {
	if len(src) < RowRecordHeaderSize {
		return formatError("RowRecordHeader", -1, errShortInput)
	}
	h.RowID = binary.LittleEndian.Uint64(src[0:])
	h.SchemaVersion = binary.LittleEndian.Uint32(src[8:])
	h.ChangeType = ChangeType(src[12])
	h.RowEncoding = RowEncoding(src[13])
	h.RowLength = binary.LittleEndian.Uint32(src[16:])
	h.RowCRC32C = binary.LittleEndian.Uint32(src[20:])
	return nil
}
