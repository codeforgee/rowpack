package fileformat

import (
	"bytes"
)

// Row Index Page / Fence on-disk structures (FILE_FORMAT_REFACTOR_PLAN §7.1 /
// §7.2, frozen by ADR-005 after the S3-⑦ prototype measured decision #2
// (Index Page = 4096 entries) and #6 (reuse the data compression level)). The
// row index of a snapshot is a set of independently compressed Row Index Pages
// plus a plaintext Fence directory; the fence lets a reader binary-search by
// (SnapshotID, TableID, RowID) to the page containing a row, then OPEN +
// decompress just that page (Lazy mode) or stream-build the Eager shard.

const (
	// MagicIndexPage opens every Row Index Page.
	MagicIndexPage = "RPKIDXPG"
	// IndexPageVersion is the current row index page layout version.
	IndexPageVersion = 1
	// IndexPageHeaderSize is the fixed size of RowIndexPageHeader.
	IndexPageHeaderSize = 64
	// IndexFenceEntrySize is the fixed size of RowIndexFenceEntry.
	IndexFenceEntrySize = 52
)

// RowIndexPageHeader is the fixed 64-byte header of a Row Index Page. All
// stream byte-lengths are stored here so a reader validates the page geometry
// (HeaderSize + sum(streams) == RawBytes) before touching any stream. The
// page CRC covers the streams region only (RawBytes[64:]).
//
//	0..8   MagicIndexPage
//	8      IndexPageVersion
//	9..12  reserved (0)
//	12..16 EntryCount
//	16..20 TableRunBytes
//	20..24 RowIDBytes
//	24..28 BlockRunBytes
//	28..32 OrdinalBytes
//	32..36 ChangeBitsBytes
//	36..44 FirstRowID
//	44..52 MinRowID
//	52..60 MaxRowID
//	60..64 CRC32C (over the streams region)
type RowIndexPageHeader struct {
	EntryCount      uint32
	TableRunBytes   uint32
	RowIDBytes      uint32
	BlockRunBytes   uint32
	OrdinalBytes    uint32
	ChangeBitsBytes uint32
	FirstRowID      uint64
	MinRowID        uint64
	MaxRowID        uint64
	CRC32C          uint32
}

// StreamsBytes returns the total byte length of the five streams.
func (h *RowIndexPageHeader) StreamsBytes() uint64 {
	return uint64(h.TableRunBytes) + uint64(h.RowIDBytes) + uint64(h.BlockRunBytes) +
		uint64(h.OrdinalBytes) + uint64(h.ChangeBitsBytes)
}

// Size returns the serialized size.
func (h *RowIndexPageHeader) Size() int { return IndexPageHeaderSize }

// MarshalTo writes h into dst (IndexPageHeaderSize bytes).
func (h *RowIndexPageHeader) MarshalTo(dst []byte) error {
	if len(dst) < IndexPageHeaderSize {
		return formatError("RowIndexPageHeader", -1, "destination too short: have %d want %d", len(dst), IndexPageHeaderSize)
	}
	for i := range dst[:IndexPageHeaderSize] {
		dst[i] = 0
	}
	copy(dst[0:8], MagicIndexPage)
	dst[8] = IndexPageVersion
	putU32(dst[12:], h.EntryCount)
	putU32(dst[16:], h.TableRunBytes)
	putU32(dst[20:], h.RowIDBytes)
	putU32(dst[24:], h.BlockRunBytes)
	putU32(dst[28:], h.OrdinalBytes)
	putU32(dst[32:], h.ChangeBitsBytes)
	putU64(dst[36:], h.FirstRowID)
	putU64(dst[44:], h.MinRowID)
	putU64(dst[52:], h.MaxRowID)
	putU32(dst[60:], h.CRC32C)
	return nil
}

// Unmarshal validates src and fills h. totalLen is the full page length; when
// nonzero the streams geometry is cross-checked against it.
func (h *RowIndexPageHeader) Unmarshal(src []byte, totalLen int) error {
	if len(src) < IndexPageHeaderSize {
		return formatError("RowIndexPageHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicIndexPage)) {
		return formatError("RowIndexPageHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if v := src[8]; v != IndexPageVersion {
		return formatError("RowIndexPageHeader", 8, "unsupported index page version %d", v)
	}
	if src[9] != 0 || src[10] != 0 || src[11] != 0 {
		return formatError("RowIndexPageHeader", 9, "reserved bytes must be zero")
	}
	var ok bool
	if h.EntryCount, ok = getU32(src[12:]); !ok {
		return formatError("RowIndexPageHeader", 12, errShortInput)
	}
	if h.TableRunBytes, ok = getU32(src[16:]); !ok {
		return formatError("RowIndexPageHeader", 16, errShortInput)
	}
	if h.RowIDBytes, ok = getU32(src[20:]); !ok {
		return formatError("RowIndexPageHeader", 20, errShortInput)
	}
	if h.BlockRunBytes, ok = getU32(src[24:]); !ok {
		return formatError("RowIndexPageHeader", 24, errShortInput)
	}
	if h.OrdinalBytes, ok = getU32(src[28:]); !ok {
		return formatError("RowIndexPageHeader", 28, errShortInput)
	}
	if h.ChangeBitsBytes, ok = getU32(src[32:]); !ok {
		return formatError("RowIndexPageHeader", 32, errShortInput)
	}
	if h.FirstRowID, ok = getU64(src[36:]); !ok {
		return formatError("RowIndexPageHeader", 36, errShortInput)
	}
	if h.MinRowID, ok = getU64(src[44:]); !ok {
		return formatError("RowIndexPageHeader", 44, errShortInput)
	}
	if h.MaxRowID, ok = getU64(src[52:]); !ok {
		return formatError("RowIndexPageHeader", 52, errShortInput)
	}
	if h.CRC32C, ok = getU32(src[60:]); !ok {
		return formatError("RowIndexPageHeader", 60, errShortInput)
	}
	if h.EntryCount == 0 {
		return formatError("RowIndexPageHeader", 12, "index page must carry at least one entry")
	}
	wantBits := (uint64(h.EntryCount) + 3) / 4
	if uint64(h.ChangeBitsBytes) != wantBits {
		return formatError("RowIndexPageHeader", 32, "change bits %d, want %d for %d entries", h.ChangeBitsBytes, wantBits, h.EntryCount)
	}
	if totalLen > 0 {
		total := uint64(IndexPageHeaderSize) + h.StreamsBytes()
		if total != uint64(totalLen) {
			return formatError("RowIndexPageHeader", -1, "index page streams sum to %d bytes, page is %d", total, totalLen)
		}
	}
	return nil
}

// RowIndexFenceEntry is the fixed 52-byte entry of a Row Index Fence directory
// (§7.2). It stays plaintext so a reader binary-searches pages by RowID before
// touching any page; the fence is authenticated by the enclosing IndexTxn
// body CRC and (optionally) per-page sealing.
type RowIndexFenceEntry struct {
	SnapshotID   uint64
	TableID      uint32
	MinRowID     uint64
	MaxRowID     uint64
	StoredOffset uint64 // offset of the stored page in the txn body
	StoredSize   uint32
	RawSize      uint32
	EntryCount   uint32
	PageCRC32C   uint32
}

// Size returns the serialized size.
func (e *RowIndexFenceEntry) Size() int { return IndexFenceEntrySize }

// MarshalTo writes e into dst.
func (e *RowIndexFenceEntry) MarshalTo(dst []byte) error {
	if len(dst) < IndexFenceEntrySize {
		return formatError("RowIndexFenceEntry", -1, "destination too short")
	}
	for i := range dst[:IndexFenceEntrySize] {
		dst[i] = 0
	}
	putU64(dst[0:], e.SnapshotID)
	putU32(dst[8:], e.TableID)
	putU64(dst[12:], e.MinRowID)
	putU64(dst[20:], e.MaxRowID)
	putU64(dst[28:], e.StoredOffset)
	putU32(dst[36:], e.StoredSize)
	putU32(dst[40:], e.RawSize)
	putU32(dst[44:], e.EntryCount)
	putU32(dst[48:], e.PageCRC32C)
	return nil
}

// Unmarshal validates src and fills e.
func (e *RowIndexFenceEntry) Unmarshal(src []byte) error {
	if len(src) < IndexFenceEntrySize {
		return formatError("RowIndexFenceEntry", -1, errShortInput)
	}
	var ok bool
	if e.SnapshotID, ok = getU64(src[0:]); !ok {
		return formatError("RowIndexFenceEntry", 0, errShortInput)
	}
	if e.TableID, ok = getU32(src[8:]); !ok {
		return formatError("RowIndexFenceEntry", 8, errShortInput)
	}
	if e.MinRowID, ok = getU64(src[12:]); !ok {
		return formatError("RowIndexFenceEntry", 12, errShortInput)
	}
	if e.MaxRowID, ok = getU64(src[20:]); !ok {
		return formatError("RowIndexFenceEntry", 20, errShortInput)
	}
	if e.StoredOffset, ok = getU64(src[28:]); !ok {
		return formatError("RowIndexFenceEntry", 28, errShortInput)
	}
	if e.StoredSize, ok = getU32(src[36:]); !ok {
		return formatError("RowIndexFenceEntry", 36, errShortInput)
	}
	if e.RawSize, ok = getU32(src[40:]); !ok {
		return formatError("RowIndexFenceEntry", 40, errShortInput)
	}
	if e.EntryCount, ok = getU32(src[44:]); !ok {
		return formatError("RowIndexFenceEntry", 44, errShortInput)
	}
	if e.PageCRC32C, ok = getU32(src[48:]); !ok {
		return formatError("RowIndexFenceEntry", 48, errShortInput)
	}
	if e.EntryCount == 0 {
		return formatError("RowIndexFenceEntry", 44, "fence entry has zero entries")
	}
	return nil
}
