package fileformat

import "encoding/binary"

// RowDirectoryEntry is the fixed 24-byte per-record directory entry of a Rows
// Block payload. The directory is stored in call order. It is the only
// surviving structure of the v1 Rows payload: the page-container block format
// (S2) reuses the same per-row entry to address a record within a page, and
// FlushedBlock.Rows carries these entries to the index builder.
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
