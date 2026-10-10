package format

import (
	"encoding/binary"
	"math"
	"math/bits"
)

// uvarintLen reports how many bytes binary.AppendUvarint will spend on v.
func uvarintLen(v uint64) int {
	if v < 0x80 {
		return 1
	}
	return (bits.Len64(v) + 6) / 7
}

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
//
// The field reads below have no per-field bounds checks: the single top-level
// length check guarantees len(src) >= RowsPageHeaderSize, and every field
// offset is fixed, so each LittleEndian read is in-bounds by construction.
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
	h.EntryCount = binary.LittleEndian.Uint32(src[12:])
	h.RowIDsBytes = binary.LittleEndian.Uint32(src[16:])
	h.OffsetsBytes = binary.LittleEndian.Uint32(src[20:])
	h.SchemaRLEBytes = binary.LittleEndian.Uint32(src[24:])
	h.ChangeBitsBytes = binary.LittleEndian.Uint32(src[28:])
	h.TuplesBytes = binary.LittleEndian.Uint32(src[32:])
	h.FirstRowID = binary.LittleEndian.Uint64(src[36:])
	h.MinRowID = binary.LittleEndian.Uint64(src[44:])
	h.MaxRowID = binary.LittleEndian.Uint64(src[52:])
	h.CRC32C = binary.LittleEndian.Uint32(src[60:])
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

// RowsPageDirEntry is the per-page directory entry stored after the Rows Block
// header (page layout). It stays plaintext so readers locate and skip pages
// without decrypting the block; the block header CRC (and a dedicated
// directory CRC at the block level) authenticates it.
//
// The entry is eight uvarints written back to back with no padding: ordinals,
// sizes and row ids are small in practice, so a typical entry costs ~9 bytes
// instead of the 56 the fixed-width form spent on mostly-zero high bytes.
//
// Two fields of the old fixed form are gone:
//
//	StoredOffset — pages tile the container back to back, so the offset of a
//	               page is the directory end plus the preceding StoredSizes.
//	               Storing it would also be circular: the offset depends on the
//	               directory length, which depends on how many bytes the offset
//	               itself encodes. Parsers recompute it into the field below.
//	PageCRC32C   — the page header's own CRC32C already covers the page
//	               streams, so the directory copy was a second opinion on the
//	               same bytes.
type RowsPageDirEntry struct {
	PageOrdinal        uint32
	FirstRecordOrdinal uint32 // record ordinal base within the block
	RecordCount        uint32
	StoredOffset       uint64 // not encoded; recomputed by the directory parser
	StoredSize         uint32
	RawSize            uint32
	MinRowID           uint64
	MaxRowID           uint64
	Flags              uint32 // bit0: oversized (single-row) page
}

// dirEntryFields is the number of uvarints in one encoded entry.
const dirEntryFields = 8

const (
	// MinRowsPageDirEntrySize is the smallest an entry can be: eight uvarints
	// of one byte each. A header claiming fewer bytes for N pages cannot be a
	// directory of N entries.
	MinRowsPageDirEntrySize = dirEntryFields
	// MaxRowsPageDirEntrySize is the largest an entry can be. It bounds the
	// allocation a hostile DirectoryBytes can ask for before any parsing.
	MaxRowsPageDirEntrySize = dirEntryFields * binary.MaxVarintLen64
)

// EncodedLen returns the number of bytes AppendTo will produce for e.
func (e *RowsPageDirEntry) EncodedLen() int {
	return uvarintLen(uint64(e.PageOrdinal)) +
		uvarintLen(uint64(e.FirstRecordOrdinal)) +
		uvarintLen(uint64(e.RecordCount)) +
		uvarintLen(uint64(e.StoredSize)) +
		uvarintLen(uint64(e.RawSize)) +
		uvarintLen(e.MinRowID) +
		uvarintLen(e.MaxRowID) +
		uvarintLen(uint64(e.Flags))
}

// AppendTo appends e's encoding to buf. StoredOffset is recomputed on parse
// and is not part of the encoding.
func (e *RowsPageDirEntry) AppendTo(buf []byte) []byte {
	buf = binary.AppendUvarint(buf, uint64(e.PageOrdinal))
	buf = binary.AppendUvarint(buf, uint64(e.FirstRecordOrdinal))
	buf = binary.AppendUvarint(buf, uint64(e.RecordCount))
	buf = binary.AppendUvarint(buf, uint64(e.StoredSize))
	buf = binary.AppendUvarint(buf, uint64(e.RawSize))
	buf = binary.AppendUvarint(buf, e.MinRowID)
	buf = binary.AppendUvarint(buf, e.MaxRowID)
	return binary.AppendUvarint(buf, uint64(e.Flags))
}

// Unmarshal decodes one entry from src and returns the bytes it consumed. A
// truncated varint, a varint wider than 10 bytes or a value that does not fit
// its uint32 field is rejected, so an untrusted directory can never yield a
// silently truncated page geometry. StoredOffset is left untouched: only the
// directory parser knows where the pages start.
func (e *RowsPageDirEntry) Unmarshal(src []byte) (int, error) {
	var vals [dirEntryFields]uint64
	pos := 0
	for i := range vals {
		v, n := binary.Uvarint(src[pos:])
		if n <= 0 {
			return 0, formatError("RowsPageDirEntry", int64(pos), errShortInput)
		}
		vals[i] = v
		pos += n
	}
	for _, i := range [...]int{0, 1, 2, 3, 4, 7} {
		if vals[i] > math.MaxUint32 {
			return 0, formatError("RowsPageDirEntry", int64(pos), "field value %d does not fit uint32", vals[i])
		}
	}
	e.PageOrdinal = uint32(vals[0])
	e.FirstRecordOrdinal = uint32(vals[1])
	e.RecordCount = uint32(vals[2])
	e.StoredSize = uint32(vals[3])
	e.RawSize = uint32(vals[4])
	e.MinRowID = vals[5]
	e.MaxRowID = vals[6]
	e.Flags = uint32(vals[7])
	return pos, nil
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
