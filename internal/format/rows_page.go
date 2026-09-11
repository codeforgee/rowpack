package format

import (
	"encoding/binary"
)

// Rows Page layout (uncompressed form; this is what Page CRC covers):
//
//	[RowsPageHeader]        64 B fixed
//	[rowID stream]          first RowID absolute uvarint, then
//	                        zigzag(uvarint) deltas in call order — records
//	                        are NOT assumed sorted by RowID
//	[end-offset stream]     EntryCount uvarint deltas of record end offsets
//	                        relative to the tuples region start (first delta
//	                        is the absolute end of record 0; DELETE ends
//	                        where the previous record ended, i.e. delta 0)
//	[schema RLE stream]     pairs (uvarint schemaVersion, uvarint run);
//	                        runs sum to EntryCount
//	[change-type bits]      2 bits per entry, LSB-first, packed value
//	                        0=Insert 1=Update 2=Delete, 3 reserved illegal
//	                        (corruption marker)
//	[tuple bodies]          body-only TypedTuples (null bitmap + values; the
//	                        8-byte ColumnCount/NullBitmapBytes header is
//	                        dropped — the schema resolves ColumnCount and the
//	                        bitmap length), concatenated
//
// The per-row 24 B RowDirectoryEntry + 24 B RowRecordHeader duplication of
// the v1 block payload is replaced by these column streams: RowID,
// SchemaVersion and ChangeType are stored once each instead of twice, and
// record lengths collapse into monotonic end-offset deltas.

const (
	// MagicRowsPage opens every Rows Page.
	MagicRowsPage = "RPKROWPG"
	// RowsPageVersion is the current rows page layout version.
	RowsPageVersion = 1
	// RowsPageHeaderSize is the fixed size of RowsPageHeader.
	RowsPageHeaderSize = 64
)

// RowsPageHeader is the fixed 64-byte header of a Rows Page. All stream
// byte-lengths are stored here so a reader can validate the page geometry
// (HeaderSize + sum(streams) == RawBytes) before touching any stream.
type RowsPageHeader struct {
	EntryCount      uint32
	RowIDsBytes     uint32
	OffsetsBytes    uint32
	SchemaRLEBytes  uint32
	ChangeBitsBytes uint32
	TuplesBytes     uint32
	FirstRowID      uint64
	MinRowID        uint64
	MaxRowID        uint64
	CRC32C          uint32 // over the streams region only (RawBytes[64:])
}

// StreamsBytes returns the total byte length of the five streams.
func (h *RowsPageHeader) StreamsBytes() uint64 {
	return uint64(h.RowIDsBytes) + uint64(h.OffsetsBytes) + uint64(h.SchemaRLEBytes) +
		uint64(h.ChangeBitsBytes) + uint64(h.TuplesBytes)
}

// MarshalTo writes h into dst (RowsPageHeaderSize bytes).
func (h *RowsPageHeader) MarshalTo(dst []byte) error {
	if len(dst) < RowsPageHeaderSize {
		return formatError("RowsPageHeader", -1, "destination too short: have %d want %d", len(dst), RowsPageHeaderSize)
	}
	for i := range dst[:RowsPageHeaderSize] {
		dst[i] = 0
	}
	copy(dst[0:8], MagicRowsPage)
	dst[8] = RowsPageVersion
	putU32(dst[12:], h.EntryCount)
	putU32(dst[16:], h.RowIDsBytes)
	putU32(dst[20:], h.OffsetsBytes)
	putU32(dst[24:], h.SchemaRLEBytes)
	putU32(dst[28:], h.ChangeBitsBytes)
	putU32(dst[32:], h.TuplesBytes)
	putU64(dst[36:], h.FirstRowID)
	putU64(dst[44:], h.MinRowID)
	putU64(dst[52:], h.MaxRowID)
	putU32(dst[60:], h.CRC32C)
	return nil
}

// Unmarshal validates src and fills h. Geometry (stream sums) is validated
// against totalLen, the full page length; pass 0 to skip that check.
func (h *RowsPageHeader) Unmarshal(src []byte, totalLen int) error {
	if len(src) < RowsPageHeaderSize {
		return formatError("RowsPageHeader", -1, errShortInput)
	}
	if string(src[0:8]) != MagicRowsPage {
		return formatError("RowsPageHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if v := src[8]; v != RowsPageVersion {
		return formatError("RowsPageHeader", 8, "unsupported page version %d", v)
	}
	if src[9] != 0 || src[10] != 0 || src[11] != 0 {
		return formatError("RowsPageHeader", 9, "reserved bytes must be zero")
	}
	var ok bool
	if h.EntryCount, ok = getU32(src[12:]); !ok {
		return formatError("RowsPageHeader", 12, errShortInput)
	}
	if h.RowIDsBytes, ok = getU32(src[16:]); !ok {
		return formatError("RowsPageHeader", 16, errShortInput)
	}
	if h.OffsetsBytes, ok = getU32(src[20:]); !ok {
		return formatError("RowsPageHeader", 20, errShortInput)
	}
	if h.SchemaRLEBytes, ok = getU32(src[24:]); !ok {
		return formatError("RowsPageHeader", 24, errShortInput)
	}
	if h.ChangeBitsBytes, ok = getU32(src[28:]); !ok {
		return formatError("RowsPageHeader", 28, errShortInput)
	}
	if h.TuplesBytes, ok = getU32(src[32:]); !ok {
		return formatError("RowsPageHeader", 32, errShortInput)
	}
	if h.FirstRowID, ok = getU64(src[36:]); !ok {
		return formatError("RowsPageHeader", 36, errShortInput)
	}
	if h.MinRowID, ok = getU64(src[44:]); !ok {
		return formatError("RowsPageHeader", 44, errShortInput)
	}
	if h.MaxRowID, ok = getU64(src[52:]); !ok {
		return formatError("RowsPageHeader", 52, errShortInput)
	}
	if h.CRC32C, ok = getU32(src[60:]); !ok {
		return formatError("RowsPageHeader", 60, errShortInput)
	}
	if h.EntryCount == 0 {
		return formatError("RowsPageHeader", 12, "page must carry at least one record")
	}
	wantBits := (uint64(h.EntryCount) + 3) / 4
	if uint64(h.ChangeBitsBytes) != wantBits {
		return formatError("RowsPageHeader", 28, "change bits %d, want %d for %d entries", h.ChangeBitsBytes, wantBits, h.EntryCount)
	}
	if totalLen > 0 {
		total := uint64(RowsPageHeaderSize) + h.StreamsBytes()
		if total != uint64(totalLen) {
			return formatError("RowsPageHeader", -1, "page streams sum to %d bytes, page is %d", total, totalLen)
		}
	}
	return nil
}

// RowsPageDirEntry is the fixed 56-byte per-page directory entry stored after
// the Rows Block header (page layout). It stays plaintext so readers
// locate and skip pages without decrypting the block; the block header CRC
// (and a dedicated directory CRC at the block level) authenticates it.
type RowsPageDirEntry struct {
	PageOrdinal        uint32
	FirstRecordOrdinal uint32 // record ordinal base within the block
	RecordCount        uint32
	StoredOffset       uint64 // offset of the stored (compressed) page
	StoredSize         uint32
	RawSize            uint32
	MinRowID           uint64
	MaxRowID           uint64
	PageCRC32C         uint32
	Flags              uint32 // bit0: oversized (single-row) page
}

// RowsPageDirEntrySize is the fixed serialized size of RowsPageDirEntry.
const RowsPageDirEntrySize = 56

// Size returns the serialized size.
func (e *RowsPageDirEntry) Size() int { return RowsPageDirEntrySize }

// MarshalTo writes e into dst.
func (e *RowsPageDirEntry) MarshalTo(dst []byte) error {
	if len(dst) < RowsPageDirEntrySize {
		return formatError("RowsPageDirEntry", -1, "destination too short")
	}
	for i := range dst[:RowsPageDirEntrySize] {
		dst[i] = 0
	}
	putU32(dst[0:], e.PageOrdinal)
	putU32(dst[4:], e.FirstRecordOrdinal)
	putU32(dst[8:], e.RecordCount)
	putU64(dst[12:], e.StoredOffset)
	putU32(dst[20:], e.StoredSize)
	putU32(dst[24:], e.RawSize)
	putU64(dst[28:], e.MinRowID)
	putU64(dst[36:], e.MaxRowID)
	putU32(dst[44:], e.PageCRC32C)
	putU32(dst[48:], e.Flags)
	return nil
}

// Unmarshal validates src and fills e.
func (e *RowsPageDirEntry) Unmarshal(src []byte) error {
	if len(src) < RowsPageDirEntrySize {
		return formatError("RowsPageDirEntry", -1, errShortInput)
	}
	var ok bool
	if e.PageOrdinal, ok = getU32(src[0:]); !ok {
		return formatError("RowsPageDirEntry", 0, errShortInput)
	}
	if e.FirstRecordOrdinal, ok = getU32(src[4:]); !ok {
		return formatError("RowsPageDirEntry", 4, errShortInput)
	}
	if e.RecordCount, ok = getU32(src[8:]); !ok {
		return formatError("RowsPageDirEntry", 8, errShortInput)
	}
	if e.StoredOffset, ok = getU64(src[12:]); !ok {
		return formatError("RowsPageDirEntry", 12, errShortInput)
	}
	if e.StoredSize, ok = getU32(src[20:]); !ok {
		return formatError("RowsPageDirEntry", 20, errShortInput)
	}
	if e.RawSize, ok = getU32(src[24:]); !ok {
		return formatError("RowsPageDirEntry", 24, errShortInput)
	}
	if e.MinRowID, ok = getU64(src[28:]); !ok {
		return formatError("RowsPageDirEntry", 28, errShortInput)
	}
	if e.MaxRowID, ok = getU64(src[36:]); !ok {
		return formatError("RowsPageDirEntry", 36, errShortInput)
	}
	if e.PageCRC32C, ok = getU32(src[44:]); !ok {
		return formatError("RowsPageDirEntry", 44, errShortInput)
	}
	if e.Flags, ok = getU32(src[48:]); !ok {
		return formatError("RowsPageDirEntry", 48, errShortInput)
	}
	return nil
}

// PackChangeType maps a ChangeType onto its 2-bit page encoding:
// Insert=1→0, Update=2→1, Delete=3→2. Packed 3 is reserved as a corruption
// marker and never produced.
func PackChangeType(ct ChangeType) (uint8, error) {
	switch ct {
	case ChangeInsert:
		return 0, nil
	case ChangeUpdate:
		return 1, nil
	case ChangeDelete:
		return 2, nil
	}
	return 0, formatError("PackChangeType", -1, "change type %d not packable", ct)
}

// UnpackChangeType reverses PackChangeType; packed 3 is the reserved illegal
// value and yields an error.
func UnpackChangeType(v uint8) (ChangeType, error) {
	switch v {
	case 0:
		return ChangeInsert, nil
	case 1:
		return ChangeUpdate, nil
	case 2:
		return ChangeDelete, nil
	}
	return 0, formatError("UnpackChangeType", -1, "illegal packed change type %d", v)
}

func getU64(src []byte) (uint64, bool) {
	if len(src) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(src), true
}
