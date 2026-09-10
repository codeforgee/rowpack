package fileformat

import (
	"encoding/binary"
	"math"
)

// Rows Block page-container layout (the payload behind a BlockKindRows
// block header):
//
//	[RowsBlockHeader]         fixed container descriptor
//	[RowsPageDirEntry × N]    plaintext page directory (56 B each)
//	[stored page 0]           independently compressed (+ per-page encrypted)
//	[stored page 1]
//	...
//
// The page directory is the read-time locator: it is small enough to stay in
// memory and short enough to be read whole on a cold access, so a single-row
// read only pulls the one page it needs. Each page's stored bytes are
// compressed (and optionally AES-GCM sealed) as their own unit; PageCRC32C
// covers the uncompressed page and the AEAD tag authenticates the stored
// bytes, so the block has no whole-payload raw CRC anymore.
//
// The outer BlockHeader keeps aggregate totals: RawSize = sum of page raw
// sizes, StoredSize = Bytes of this container, RawCRC32C is repurposed as the
// CRC of the container plaintext (RowsBlockHeader + page directory), since
// the individual pages are verified by their own CRC/AEAD.

const (
	// MagicRowsBlockHdr opens a Rows block page container.
	MagicRowsBlockHdr = "RPKROWBL"
	// RowsBlockVersion is the current container layout version.
	RowsBlockVersion = 1
	// RowsBlockHeaderSize is the fixed size of RowsBlockHeader.
	RowsBlockHeaderSize = 24
)

// RowsBlockHeader is the fixed 24-byte descriptor of a Rows block page
// container. The directory immediately follows it.
type RowsBlockHeader struct {
	PageCount      uint32
	DirectoryBytes uint32
	TotalRecords   uint32 // == block ItemCount
	Reserved       uint32
}

// Size returns the serialized size.
func (h *RowsBlockHeader) Size() int { return RowsBlockHeaderSize }

// MarshalTo writes h into dst.
func (h *RowsBlockHeader) MarshalTo(dst []byte) error {
	if len(dst) < RowsBlockHeaderSize {
		return formatError("RowsBlockHeader", -1, "destination too short: have %d want %d", len(dst), RowsBlockHeaderSize)
	}
	for i := range dst[:RowsBlockHeaderSize] {
		dst[i] = 0
	}
	copy(dst[0:8], MagicRowsBlockHdr)
	dst[8] = RowsBlockVersion
	putU32(dst[12:], h.PageCount)
	putU32(dst[16:], h.DirectoryBytes)
	putU32(dst[20:], h.TotalRecords)
	want, ok := rowsDirectoryBytes(h.PageCount)
	if !ok || h.DirectoryBytes != want {
		return formatError("RowsBlockHeader", 16, "directory bytes %d != pageCount %d * %d", h.DirectoryBytes, h.PageCount, RowsPageDirEntrySize)
	}
	return nil
}

// Unmarshal validates src and fills h, cross-checking DirectoryBytes against
// PageCount.
func (h *RowsBlockHeader) Unmarshal(src []byte) error {
	if len(src) < RowsBlockHeaderSize {
		return formatError("RowsBlockHeader", -1, errShortInput)
	}
	if string(src[0:8]) != MagicRowsBlockHdr {
		return formatError("RowsBlockHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if v := src[8]; v != RowsBlockVersion {
		return formatError("RowsBlockHeader", 8, "unsupported block version %d", v)
	}
	if v, ok := getU16(src[10:]); !ok || v != 0 {
		return formatError("RowsBlockHeader", 10, "reserved bytes must be zero")
	}
	var ok bool
	if h.PageCount, ok = getU32(src[12:]); !ok {
		return formatError("RowsBlockHeader", 12, errShortInput)
	}
	if h.DirectoryBytes, ok = getU32(src[16:]); !ok {
		return formatError("RowsBlockHeader", 16, errShortInput)
	}
	if h.TotalRecords, ok = getU32(src[20:]); !ok {
		return formatError("RowsBlockHeader", 20, errShortInput)
	}
	want, valid := rowsDirectoryBytes(h.PageCount)
	if !valid || h.DirectoryBytes != want {
		return formatError("RowsBlockHeader", 16, "directory bytes %d != pageCount %d * %d", h.DirectoryBytes, h.PageCount, RowsPageDirEntrySize)
	}
	return nil
}

// rowsDirectoryBytes computes PageCount*entrySize without allowing the
// uint32 wraparound that an untrusted header could otherwise use to pass the
// geometry check and trigger a huge allocation later.
func rowsDirectoryBytes(pageCount uint32) (uint32, bool) {
	n := uint64(pageCount) * uint64(RowsPageDirEntrySize)
	if n > math.MaxUint32 {
		return 0, false
	}
	return uint32(n), true
}

// StoredDataBytes returns the total stored bytes of the page region (the sum
// of per-page stored sizes). This is what the outer block payload must
// contain beyond the header + directory.
func (h *RowsBlockHeader) StoredDataBytes(dir []RowsPageDirEntry) uint64 {
	var n uint64
	for _, e := range dir {
		n += uint64(e.StoredSize)
	}
	return n
}

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
