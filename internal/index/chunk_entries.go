package index

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/codeforgee/rowpack/internal/format"
)

// Frozen v1 delta/varint encodings of the metadata and block chunk entry
// streams (docs/INDEX_TXN_FORMAT_V1.md §4.3). SnapshotID is not stored —
// Builder pins every entry to the txn snapshot and the decoder stamps the
// header's ID. The first entry of each chunk encodes absolutely, the rest
// relatively, so chunks stay independently decodable.
//
// Metadata stream (IndexChunkKindMetadata):
//
//	first entry: uvarint(objectID) uvarint(recordType) uvarint(revision)
//	             uvarint(blockID) uvarint(itemOrdinal)
//	             byte(operation) byte(flags)
//	next entry:  byte tag             // bit0 objectID==prev · bit1 recordType==prev
//	                                  // bit2 revision==prev · bit3 blockID==prev
//	                                  // unknown tag bits reject the chunk
//	             [uvarint zigzag objectDelta]      // only when !bit0
//	             [uvarint zigzag recordTypeDelta]  // only when !bit1
//	             [uvarint zigzag revisionDelta]    // only when !bit2
//	             [uvarint zigzag blockDelta]       // only when !bit3
//	             uvarint zigzag ordinalDelta
//	             byte(operation) byte(flags)
//
// Block stream (IndexChunkKindBlock):
//
//	first entry: uvarint(blockID) uvarint(tableID) byte(blockKind)
//	             byte(compression) uvarint(dataOffset) uvarint(rawSize)
//	             uvarint(storedSize) uvarint(itemCount) 4B-LE(rawCRC32C)
//	next entry:  byte tag             // bit0 tableID==prev · bit1 blockKind==prev
//	                                  // bit2 compression==prev · unknown bits reject
//	             [uvarint zigzag tableDelta]       // only when !bit0
//	             [byte blockKind]                  // only when !bit1
//	             [byte compression]                // only when !bit2
//	             uvarint zigzag blockIDDelta
//	             uvarint zigzag dataOffsetDelta
//	             uvarint(rawSize) uvarint(storedSize) uvarint(itemCount)
//	             4B-LE(rawCRC32C)
//
// No per-entry CRC: chunk PayloadCRC32C + txn body CRC (+ AEAD when
// encrypted) cover integrity. Enums and uint32 widths are validated at parse
// time; corruption rebuilds the index instead of poisoning the view.

// errChunkStreamCorrupt rejects a stream that ends early or carries
// out-of-range values; callers wrap it with the entry index.
var errChunkStreamCorrupt = errors.New("chunk entry stream corrupt")

// validateOperation rejects unknown operation enums at parse time.
func validateOperation(op byte) error {
	switch format.Operation(op) {
	case format.OperationUpsert, format.OperationDelete:
		return nil
	}
	return fmt.Errorf("unknown operation %d", op)
}

// validateBlockKind rejects unknown block-kind enums at parse time.
func validateBlockKind(k byte) error {
	switch format.BlockKind(k) {
	case format.BlockKindRows, format.BlockKindMetadata, format.BlockKindSnapshotMeta:
		return nil
	}
	return fmt.Errorf("unknown block kind %d", k)
}

// validateCompression rejects unknown compression enums at parse time.
func validateCompression(c byte) error {
	switch format.Compression(c) {
	case format.CompressionNone, format.CompressionZstd:
		return nil
	}
	return fmt.Errorf("unknown compression %d", c)
}

// Unknown-bit masks for the two tag bytes.
const (
	metaTagKnown  = metaTagObjectSame | metaTagRecordTypeSame | metaTagRevisionSame | metaTagBlockSame
	blockTagKnown = blockTagTableSame | blockTagKindSame | blockTagCompSame
)

// Tag bits of the metadata stream (frozen v1).
const (
	metaTagObjectSame     = 1 << 0
	metaTagRecordTypeSame = 1 << 1
	metaTagRevisionSame   = 1 << 2
	metaTagBlockSame      = 1 << 3
)

// Tag bits of the block stream (frozen v1).
const (
	blockTagTableSame = 1 << 0
	blockTagKindSame  = 1 << 1
	blockTagCompSame  = 1 << 2
)

// ---- encoding (write path) ----

// metaEncoder is the stateful encoder of one metadata chunk stream. The zero
// value encodes the first entry absolutely; restart re-bases the delta chain
// at a chunk boundary. Not safe for concurrent use.
type metaEncoder struct {
	prev    format.MetadataIndexEntry
	hasPrev bool
}

// restart makes the next entry encode absolutely (new chunk).
func (e *metaEncoder) restart() { e.hasPrev = false }

// add appends the encoded form of m to dst.
func (e *metaEncoder) add(dst []byte, m format.MetadataIndexEntry) []byte {
	if !e.hasPrev {
		dst = binary.AppendUvarint(dst, m.ObjectID)
		dst = binary.AppendUvarint(dst, uint64(m.RecordType))
		dst = binary.AppendUvarint(dst, uint64(m.Revision))
		dst = binary.AppendUvarint(dst, m.BlockID)
		dst = binary.AppendUvarint(dst, uint64(m.ItemOrdinal))
	} else {
		at := len(dst)
		dst = append(dst, 0) // tag byte, patched below
		if m.ObjectID != e.prev.ObjectID {
			dst = binary.AppendUvarint(dst, zigzag(int64(m.ObjectID)-int64(e.prev.ObjectID)))
		} else {
			dst[at] |= metaTagObjectSame
		}
		if m.RecordType != e.prev.RecordType {
			dst = binary.AppendUvarint(dst, zigzag(int64(m.RecordType)-int64(e.prev.RecordType)))
		} else {
			dst[at] |= metaTagRecordTypeSame
		}
		if m.Revision != e.prev.Revision {
			dst = binary.AppendUvarint(dst, zigzag(int64(m.Revision)-int64(e.prev.Revision)))
		} else {
			dst[at] |= metaTagRevisionSame
		}
		if m.BlockID != e.prev.BlockID {
			dst = binary.AppendUvarint(dst, zigzag(int64(m.BlockID)-int64(e.prev.BlockID)))
		} else {
			dst[at] |= metaTagBlockSame
		}
		dst = binary.AppendUvarint(dst, zigzag(int64(m.ItemOrdinal)-int64(e.prev.ItemOrdinal)))
	}
	dst = append(dst, byte(m.Operation))
	if m.Critical {
		dst = append(dst, format.FlagCritical)
	} else {
		dst = append(dst, 0)
	}
	e.prev = m
	e.hasPrev = true
	return dst
}

// blockEncoder is the stateful encoder of one block chunk stream.
type blockEncoder struct {
	prev    format.BlockIndexEntry
	hasPrev bool
}

// restart makes the next entry encode absolutely (new chunk).
func (e *blockEncoder) restart() { e.hasPrev = false }

// add appends the encoded form of b to dst.
func (e *blockEncoder) add(dst []byte, b format.BlockIndexEntry) []byte {
	if !e.hasPrev {
		dst = binary.AppendUvarint(dst, b.BlockID)
		dst = binary.AppendUvarint(dst, uint64(b.TableID))
		dst = append(dst, byte(b.BlockKind), byte(b.Compression))
		dst = binary.AppendUvarint(dst, b.DataOffset)
	} else {
		at := len(dst)
		dst = append(dst, 0) // tag byte, patched below
		if b.TableID != e.prev.TableID {
			dst = binary.AppendUvarint(dst, zigzag(int64(b.TableID)-int64(e.prev.TableID)))
		} else {
			dst[at] |= blockTagTableSame
		}
		if b.BlockKind != e.prev.BlockKind {
			dst = append(dst, byte(b.BlockKind))
		} else {
			dst[at] |= blockTagKindSame
		}
		if b.Compression != e.prev.Compression {
			dst = append(dst, byte(b.Compression))
		} else {
			dst[at] |= blockTagCompSame
		}
		dst = binary.AppendUvarint(dst, zigzag(int64(b.BlockID)-int64(e.prev.BlockID)))
		dst = binary.AppendUvarint(dst, zigzag(int64(b.DataOffset)-int64(e.prev.DataOffset)))
	}
	dst = binary.AppendUvarint(dst, uint64(b.RawSize))
	dst = binary.AppendUvarint(dst, uint64(b.StoredSize))
	dst = binary.AppendUvarint(dst, uint64(b.ItemCount))
	dst = binary.LittleEndian.AppendUint32(dst, b.RawCRC32C)
	e.prev = b
	e.hasPrev = true
	return dst
}

// ---- decoding (read path) ----

// chunkStream is the sequential reader behind both stream decoders.
type chunkStream struct {
	src []byte
	pos int
}

func (s *chunkStream) uvarint() (uint64, error) {
	v, n := binary.Uvarint(s.src[s.pos:])
	if n <= 0 {
		return 0, errChunkStreamCorrupt
	}
	s.pos += n
	return v, nil
}

func (s *chunkStream) byteVal() (byte, error) {
	if s.pos >= len(s.src) {
		return 0, errChunkStreamCorrupt
	}
	b := s.src[s.pos]
	s.pos++
	return b, nil
}

// fixed32 reads a 4-byte little-endian word (random data such as CRCs).
func (s *chunkStream) fixed32() (uint32, error) {
	if len(s.src)-s.pos < 4 {
		return 0, errChunkStreamCorrupt
	}
	v := binary.LittleEndian.Uint32(s.src[s.pos:])
	s.pos += 4
	return v, nil
}

// u32Field reads a uvarint that must fit uint32.
func (s *chunkStream) u32Field(what string) (uint32, error) {
	v, err := s.uvarint()
	if err != nil {
		return 0, err
	}
	if v > maxUint32 {
		return 0, fmt.Errorf("chunk entry stream %s %d exceeds uint32", what, v)
	}
	return uint32(v), nil
}

// validateEntryEnums checks the enum byte fields of one decoded metadata
// entry (flags carry unknown bits through, mirroring the fixed-struct rule).
func validateEntryEnums(e *format.MetadataIndexEntry) error {
	return validateOperation(byte(e.Operation))
}

// decodeMetadataChunk decodes exactly count metadata entries from raw,
// stamping snapshotID into each, handing them to add in stream order. It
// rejects truncated streams, unknown tag bits, invalid enum values and
// trailing slack.
func decodeMetadataChunk(raw []byte, count uint32, snapshotID uint64, add func(format.MetadataIndexEntry) error) error {
	s := chunkStream{src: raw}
	var prev format.MetadataIndexEntry
	for i := uint32(0); i < count; i++ {
		var e format.MetadataIndexEntry
		if i == 0 {
			v, err := s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.ObjectID = v
			rt, err := s.u32Field("record type")
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.RecordType = rt
			rev, err := s.u32Field("revision")
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.Revision = rev
			blk, err := s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.BlockID = blk
			ord, err := s.u32Field("item ordinal")
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.ItemOrdinal = ord
		} else {
			tag, err := s.byteVal()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			if tag&^metaTagKnown != 0 {
				return fmt.Errorf("entry %d: unknown tag bits %#x", i, tag&^metaTagKnown)
			}
			e = prev
			if tag&metaTagObjectSame == 0 {
				v, err := s.uvarint()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.ObjectID = uint64(int64(prev.ObjectID) + unzigzag(v))
			}
			if tag&metaTagRecordTypeSame == 0 {
				v, err := s.uvarint()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.RecordType = uint32(int64(prev.RecordType) + unzigzag(v))
			}
			if tag&metaTagRevisionSame == 0 {
				v, err := s.uvarint()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.Revision = uint32(int64(prev.Revision) + unzigzag(v))
			}
			if tag&metaTagBlockSame == 0 {
				v, err := s.uvarint()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.BlockID = uint64(int64(prev.BlockID) + unzigzag(v))
			}
			v, err := s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.ItemOrdinal = uint32(int64(prev.ItemOrdinal) + unzigzag(v))
		}
		op, err := s.byteVal()
		if err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		e.Operation = format.Operation(op)
		flags, err := s.byteVal()
		if err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		e.Critical = flags&format.FlagCritical != 0
		e.SnapshotID = snapshotID
		if err := validateEntryEnums(&e); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if err := add(e); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		prev = e
	}
	if s.pos != len(raw) {
		return fmt.Errorf("%d trailing bytes after %d entries", len(raw)-s.pos, count)
	}
	return nil
}

// validateBlockEntryEnums checks the enum byte fields of one decoded block
// entry.
func validateBlockEntryEnums(e *format.BlockIndexEntry) error {
	if err := validateBlockKind(byte(e.BlockKind)); err != nil {
		return err
	}
	return validateCompression(byte(e.Compression))
}

// decodeBlockChunk decodes exactly count block entries from raw, stamping
// snapshotID into each. Rejection rules mirror decodeMetadataChunk.
func decodeBlockChunk(raw []byte, count uint32, snapshotID uint64, add func(format.BlockIndexEntry) error) error {
	s := chunkStream{src: raw}
	var prev format.BlockIndexEntry
	for i := uint32(0); i < count; i++ {
		var e format.BlockIndexEntry
		if i == 0 {
			blk, err := s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.BlockID = blk
			tbl, err := s.u32Field("table id")
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.TableID = tbl
			kind, err := s.byteVal()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.BlockKind = format.BlockKind(kind)
			comp, err := s.byteVal()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.Compression = format.Compression(comp)
			off, err := s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.DataOffset = off
		} else {
			tag, err := s.byteVal()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			if tag&^blockTagKnown != 0 {
				return fmt.Errorf("entry %d: unknown tag bits %#x", i, tag&^blockTagKnown)
			}
			e = prev
			if tag&blockTagTableSame == 0 {
				v, err := s.uvarint()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.TableID = uint32(int64(prev.TableID) + unzigzag(v))
			}
			if tag&blockTagKindSame == 0 {
				k, err := s.byteVal()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.BlockKind = format.BlockKind(k)
			}
			if tag&blockTagCompSame == 0 {
				c, err := s.byteVal()
				if err != nil {
					return fmt.Errorf("entry %d: %w", i, err)
				}
				e.Compression = format.Compression(c)
			}
			v, err := s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.BlockID = uint64(int64(prev.BlockID) + unzigzag(v))
			v, err = s.uvarint()
			if err != nil {
				return fmt.Errorf("entry %d: %w", i, err)
			}
			e.DataOffset = uint64(int64(prev.DataOffset) + unzigzag(v))
		}
		var err error
		if e.RawSize, err = s.u32Field("raw size"); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if e.StoredSize, err = s.u32Field("stored size"); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if e.ItemCount, err = s.u32Field("item count"); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if e.RawCRC32C, err = s.fixed32(); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		e.SnapshotID = snapshotID
		if err := validateBlockEntryEnums(&e); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if err := add(e); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		prev = e
	}
	if s.pos != len(raw) {
		return fmt.Errorf("%d trailing bytes after %d entries", len(raw)-s.pos, count)
	}
	return nil
}
