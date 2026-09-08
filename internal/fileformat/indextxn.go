package fileformat

import (
	"bytes"
	"encoding/binary"
)

// IndexTxnHeader is the fixed 80-byte header of one index transaction. Each
// committed snapshot has exactly one index transaction, embedded in the
// single store file between the blocks and the SnapshotFooter.
type IndexTxnHeader struct {
	TxnSequence        uint64
	SnapshotID         uint64
	DataSnapshotStart  uint64
	DataSnapshotEnd    uint64
	MetadataEntryCount uint32
	BlockEntryCount    uint32
	RowEntryCount      uint64
	BodyBytes          uint64 // length between header and footer
}

// Size returns the serialized size.
func (h *IndexTxnHeader) Size() int { return IndexTxnHeaderSize }

// MarshalTo writes h into dst.
func (h *IndexTxnHeader) MarshalTo(dst []byte) error {
	if len(dst) < IndexTxnHeaderSize {
		return formatError("IndexTxnHeader", -1, "destination too short: have %d want %d", len(dst), IndexTxnHeaderSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], MagicIndexTxnHdr)
	putU32(dst[8:], IndexTxnHeaderSize)
	putU64(dst[16:], h.TxnSequence)
	putU64(dst[24:], h.SnapshotID)
	putU64(dst[32:], h.DataSnapshotStart)
	putU64(dst[40:], h.DataSnapshotEnd)
	putU32(dst[48:], h.MetadataEntryCount)
	putU32(dst[52:], h.BlockEntryCount)
	putU64(dst[56:], h.RowEntryCount)
	putU64(dst[64:], h.BodyBytes)
	// offset 72..76 header CRC
	finalizeCRC(dst[:IndexTxnHeaderSize], 72)
	// offset 76..80 reserved (zeroed above)
	return nil
}

// Unmarshal validates src and fills h.
func (h *IndexTxnHeader) Unmarshal(src []byte) error {
	if len(src) < IndexTxnHeaderSize {
		return formatError("IndexTxnHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicIndexTxnHdr)) {
		return formatError("IndexTxnHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if sz := binary.LittleEndian.Uint32(src[8:]); sz != IndexTxnHeaderSize {
		return formatError("IndexTxnHeader", 8, "%s: size=%d want %d", errBadSize, sz, IndexTxnHeaderSize)
	}
	if _, err := verifyCRC(src[:IndexTxnHeaderSize], 72); err != nil {
		return formatError("IndexTxnHeader", 72, "%v", err)
	}
	h.TxnSequence = binary.LittleEndian.Uint64(src[16:])
	h.SnapshotID = binary.LittleEndian.Uint64(src[24:])
	h.DataSnapshotStart = binary.LittleEndian.Uint64(src[32:])
	h.DataSnapshotEnd = binary.LittleEndian.Uint64(src[40:])
	h.MetadataEntryCount = binary.LittleEndian.Uint32(src[48:])
	h.BlockEntryCount = binary.LittleEndian.Uint32(src[52:])
	h.RowEntryCount = binary.LittleEndian.Uint64(src[56:])
	h.BodyBytes = binary.LittleEndian.Uint64(src[64:])
	return nil
}

// SnapshotIndexEntry is the fixed 72-byte snapshot summary entry.
type SnapshotIndexEntry struct {
	SnapshotID       uint64
	ParentSnapshotID uint64
	SnapshotType     SnapshotType
	BlockCount       uint32
	RowRecordCount   uint64
	DataStart        uint64
	DataEnd          uint64
	CreatedUnixNano  int64
	DataFooterCRC32C uint32
}

// Size returns the serialized size.
func (e *SnapshotIndexEntry) Size() int { return SnapshotIndexEntrySize }

// MarshalTo writes e into dst.
func (e *SnapshotIndexEntry) MarshalTo(dst []byte) error {
	if len(dst) < SnapshotIndexEntrySize {
		return formatError("SnapshotIndexEntry", -1, "destination too short: have %d want %d", len(dst), SnapshotIndexEntrySize)
	}
	for i := range dst {
		dst[i] = 0
	}
	putU64(dst[0:], e.SnapshotID)
	putU64(dst[8:], e.ParentSnapshotID)
	dst[16] = byte(e.SnapshotType)
	putU32(dst[20:], e.BlockCount)
	putU64(dst[24:], e.RowRecordCount)
	putU64(dst[32:], e.DataStart)
	putU64(dst[40:], e.DataEnd)
	putU64(dst[48:], uint64(e.CreatedUnixNano))
	putU32(dst[56:], e.DataFooterCRC32C)
	// offset 60..64 entry CRC
	finalizeCRC(dst[:SnapshotIndexEntrySize], 60)
	// offset 64..72 reserved (zeroed above)
	return nil
}

// Unmarshal validates src and fills e.
func (e *SnapshotIndexEntry) Unmarshal(src []byte) error {
	if len(src) < SnapshotIndexEntrySize {
		return formatError("SnapshotIndexEntry", -1, errShortInput)
	}
	if _, err := verifyCRC(src[:SnapshotIndexEntrySize], 60); err != nil {
		return formatError("SnapshotIndexEntry", 60, "%v", err)
	}
	e.SnapshotID = binary.LittleEndian.Uint64(src[0:])
	e.ParentSnapshotID = binary.LittleEndian.Uint64(src[8:])
	e.SnapshotType = SnapshotType(src[16])
	e.BlockCount = binary.LittleEndian.Uint32(src[20:])
	e.RowRecordCount = binary.LittleEndian.Uint64(src[24:])
	e.DataStart = binary.LittleEndian.Uint64(src[32:])
	e.DataEnd = binary.LittleEndian.Uint64(src[40:])
	e.CreatedUnixNano = int64(binary.LittleEndian.Uint64(src[48:]))
	e.DataFooterCRC32C = binary.LittleEndian.Uint32(src[56:])
	return nil
}

// MetadataIndexEntry is the fixed 48-byte per-object metadata index entry.
// The metadata body itself is read from the .rpk file.
type MetadataIndexEntry struct {
	SnapshotID  uint64
	ObjectID    uint64
	Revision    uint32
	RecordType  uint32
	BlockID     uint64
	ItemOrdinal uint32
	Operation   Operation
	Critical    bool
}

// Size returns the serialized size.
func (e *MetadataIndexEntry) Size() int { return MetadataIndexEntrySize }

// MarshalTo writes e into dst.
func (e *MetadataIndexEntry) MarshalTo(dst []byte) error {
	if len(dst) < MetadataIndexEntrySize {
		return formatError("MetadataIndexEntry", -1, "destination too short: have %d want %d", len(dst), MetadataIndexEntrySize)
	}
	for i := range dst {
		dst[i] = 0
	}
	putU64(dst[0:], e.SnapshotID)
	putU64(dst[8:], e.ObjectID)
	putU32(dst[16:], e.Revision)
	putU32(dst[20:], e.RecordType)
	putU64(dst[24:], e.BlockID)
	putU32(dst[32:], e.ItemOrdinal)
	dst[36] = byte(e.Operation)
	if e.Critical {
		dst[37] = FlagCritical
	}
	// offset 40..44 entry CRC
	finalizeCRC(dst[:MetadataIndexEntrySize], 40)
	// offset 44..48 Reserved2 (zeroed above)
	return nil
}

// Unmarshal validates src and fills e.
func (e *MetadataIndexEntry) Unmarshal(src []byte) error {
	if len(src) < MetadataIndexEntrySize {
		return formatError("MetadataIndexEntry", -1, errShortInput)
	}
	if _, err := verifyCRC(src[:MetadataIndexEntrySize], 40); err != nil {
		return formatError("MetadataIndexEntry", 40, "%v", err)
	}
	e.SnapshotID = binary.LittleEndian.Uint64(src[0:])
	e.ObjectID = binary.LittleEndian.Uint64(src[8:])
	e.Revision = binary.LittleEndian.Uint32(src[16:])
	e.RecordType = binary.LittleEndian.Uint32(src[20:])
	e.BlockID = binary.LittleEndian.Uint64(src[24:])
	e.ItemOrdinal = binary.LittleEndian.Uint32(src[32:])
	e.Operation = Operation(src[36])
	e.Critical = src[37]&FlagCritical != 0
	return nil
}

// BlockIndexEntry is the fixed 56-byte block location entry.
type BlockIndexEntry struct {
	BlockID     uint64
	SnapshotID  uint64
	TableID     uint32
	BlockKind   BlockKind
	Compression Compression
	DataOffset  uint64
	RawSize     uint32
	StoredSize  uint32
	ItemCount   uint32
	RawCRC32C   uint32
}

// Size returns the serialized size.
func (e *BlockIndexEntry) Size() int { return BlockIndexEntrySize }

// MarshalTo writes e into dst.
func (e *BlockIndexEntry) MarshalTo(dst []byte) error {
	if len(dst) < BlockIndexEntrySize {
		return formatError("BlockIndexEntry", -1, "destination too short: have %d want %d", len(dst), BlockIndexEntrySize)
	}
	for i := range dst {
		dst[i] = 0
	}
	putU64(dst[0:], e.BlockID)
	putU64(dst[8:], e.SnapshotID)
	putU32(dst[16:], e.TableID)
	dst[20] = byte(e.BlockKind)
	dst[21] = byte(e.Compression)
	putU64(dst[24:], e.DataOffset)
	putU32(dst[32:], e.RawSize)
	putU32(dst[36:], e.StoredSize)
	putU32(dst[40:], e.ItemCount)
	putU32(dst[44:], e.RawCRC32C)
	// offset 48..52 entry CRC
	finalizeCRC(dst[:BlockIndexEntrySize], 48)
	// offset 52..56 reserved (zeroed above)
	return nil
}

// Unmarshal validates src and fills e.
func (e *BlockIndexEntry) Unmarshal(src []byte) error {
	if len(src) < BlockIndexEntrySize {
		return formatError("BlockIndexEntry", -1, errShortInput)
	}
	if _, err := verifyCRC(src[:BlockIndexEntrySize], 48); err != nil {
		return formatError("BlockIndexEntry", 48, "%v", err)
	}
	e.BlockID = binary.LittleEndian.Uint64(src[0:])
	e.SnapshotID = binary.LittleEndian.Uint64(src[8:])
	e.TableID = binary.LittleEndian.Uint32(src[16:])
	e.BlockKind = BlockKind(src[20])
	e.Compression = Compression(src[21])
	e.DataOffset = binary.LittleEndian.Uint64(src[24:])
	e.RawSize = binary.LittleEndian.Uint32(src[32:])
	e.StoredSize = binary.LittleEndian.Uint32(src[36:])
	e.ItemCount = binary.LittleEndian.Uint32(src[40:])
	e.RawCRC32C = binary.LittleEndian.Uint32(src[44:])
	return nil
}

// RowIndexEntry is the fixed 40-byte row location entry.
type RowIndexEntry struct {
	SnapshotID  uint64
	TableID     uint32
	ChangeType  ChangeType
	RowID       uint64
	BlockID     uint64
	ItemOrdinal uint32
}

// Size returns the serialized size.
func (e *RowIndexEntry) Size() int { return RowIndexEntrySize }

// MarshalTo writes e into dst.
func (e *RowIndexEntry) MarshalTo(dst []byte) error {
	if len(dst) < RowIndexEntrySize {
		return formatError("RowIndexEntry", -1, "destination too short: have %d want %d", len(dst), RowIndexEntrySize)
	}
	for i := range dst {
		dst[i] = 0
	}
	putU64(dst[0:], e.SnapshotID)
	putU32(dst[8:], e.TableID)
	dst[12] = byte(e.ChangeType)
	putU64(dst[16:], e.RowID)
	putU64(dst[24:], e.BlockID)
	putU32(dst[32:], e.ItemOrdinal)
	// offset 36..40 entry CRC
	finalizeCRC(dst[:RowIndexEntrySize], 36)
	return nil
}

// Unmarshal validates src and fills e.
func (e *RowIndexEntry) Unmarshal(src []byte) error {
	if len(src) < RowIndexEntrySize {
		return formatError("RowIndexEntry", -1, errShortInput)
	}
	if _, err := verifyCRC(src[:RowIndexEntrySize], 36); err != nil {
		return formatError("RowIndexEntry", 36, "%v", err)
	}
	e.SnapshotID = binary.LittleEndian.Uint64(src[0:])
	e.TableID = binary.LittleEndian.Uint32(src[8:])
	e.ChangeType = ChangeType(src[12])
	e.RowID = binary.LittleEndian.Uint64(src[16:])
	e.BlockID = binary.LittleEndian.Uint64(src[24:])
	e.ItemOrdinal = binary.LittleEndian.Uint32(src[32:])
	return nil
}

// IndexTxnFooter is the fixed 80-byte footer of an index transaction. It
// cross-checks the transaction body and the corresponding data footer, so an
// index transaction is only valid when footer, entries and data footer all
// match.
type IndexTxnFooter struct {
	TxnSequence      uint64
	SnapshotID       uint64
	TxnStartOffset   uint64
	TxnEndOffset     uint64
	DataSnapshotEnd  uint64
	BodyCRC32C       uint32
	DataFooterCRC32C uint32
}

// Size returns the serialized size.
func (f *IndexTxnFooter) Size() int { return IndexTxnFooterSize }

// MarshalTo writes f into dst.
func (f *IndexTxnFooter) MarshalTo(dst []byte) error {
	if len(dst) < IndexTxnFooterSize {
		return formatError("IndexTxnFooter", -1, "destination too short: have %d want %d", len(dst), IndexTxnFooterSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], MagicIndexTxnFtr)
	putU32(dst[8:], IndexTxnFooterSize)
	putU64(dst[16:], f.TxnSequence)
	putU64(dst[24:], f.SnapshotID)
	putU64(dst[32:], f.TxnStartOffset)
	putU64(dst[40:], f.TxnEndOffset)
	putU64(dst[48:], f.DataSnapshotEnd)
	putU32(dst[56:], f.BodyCRC32C)
	putU32(dst[60:], f.DataFooterCRC32C)
	// offset 64..68 footer CRC
	finalizeCRC(dst[:IndexTxnFooterSize], 64)
	// offset 68..80 reserved (zeroed above)
	return nil
}

// Unmarshal validates src and fills f.
func (f *IndexTxnFooter) Unmarshal(src []byte) error {
	if len(src) < IndexTxnFooterSize {
		return formatError("IndexTxnFooter", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(MagicIndexTxnFtr)) {
		return formatError("IndexTxnFooter", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	if sz := binary.LittleEndian.Uint32(src[8:]); sz != IndexTxnFooterSize {
		return formatError("IndexTxnFooter", 8, "%s: size=%d want %d", errBadSize, sz, IndexTxnFooterSize)
	}
	if _, err := verifyCRC(src[:IndexTxnFooterSize], 64); err != nil {
		return formatError("IndexTxnFooter", 64, "%v", err)
	}
	f.TxnSequence = binary.LittleEndian.Uint64(src[16:])
	f.SnapshotID = binary.LittleEndian.Uint64(src[24:])
	f.TxnStartOffset = binary.LittleEndian.Uint64(src[32:])
	f.TxnEndOffset = binary.LittleEndian.Uint64(src[40:])
	f.DataSnapshotEnd = binary.LittleEndian.Uint64(src[48:])
	f.BodyCRC32C = binary.LittleEndian.Uint32(src[56:])
	f.DataFooterCRC32C = binary.LittleEndian.Uint32(src[60:])
	return nil
}
