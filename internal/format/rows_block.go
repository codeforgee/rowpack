package format

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
// bytes.
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
// container. The directory immediately follows it; the stored pages close
// the container.
type RowsBlockHeader struct {
	PageCount      uint32
	DirectoryBytes uint32
	TotalRecords   uint32 // == block ItemCount
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
	return checkDirectoryBytes(h)
}

// Unmarshal validates src and fills h, cross-checking DirectoryBytes against
// PageCount. Field reads have no per-field bounds checks: the single
// top-level length check guarantees len(src) >= RowsBlockHeaderSize.
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
	if binary.LittleEndian.Uint16(src[10:]) != 0 {
		return formatError("RowsBlockHeader", 10, "reserved bytes must be zero")
	}
	h.PageCount = binary.LittleEndian.Uint32(src[12:])
	h.DirectoryBytes = binary.LittleEndian.Uint32(src[16:])
	h.TotalRecords = binary.LittleEndian.Uint32(src[20:])
	return checkDirectoryBytes(h)
}

// checkDirectoryBytes bounds DirectoryBytes against PageCount. Entries are
// varint-encoded, so the exact length is no longer derivable from PageCount —
// only the range is. The bounds still stop an untrusted header from claiming a
// directory large enough to trigger a huge allocation, or small enough that
// PageCount entries could not possibly fit.
func checkDirectoryBytes(h *RowsBlockHeader) error {
	lo := uint64(h.PageCount) * MinRowsPageDirEntrySize
	hi, ok := maxDirectoryBytes(h.PageCount)
	if !ok || uint64(h.DirectoryBytes) < lo || uint64(h.DirectoryBytes) > uint64(hi) {
		return formatError("RowsBlockHeader", 16,
			"directory bytes %d outside [%d, %d] for pageCount %d", h.DirectoryBytes, lo, hi, h.PageCount)
	}
	return nil
}

// maxDirectoryBytes is the most PageCount varint entries can occupy, guarding
// the uint32 wraparound an untrusted header could otherwise use to pass the
// geometry check and trigger a huge allocation later.
func maxDirectoryBytes(pageCount uint32) (uint32, bool) {
	n := uint64(pageCount) * MaxRowsPageDirEntrySize
	if n > math.MaxUint32 {
		return 0, false
	}
	return uint32(n), true
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
