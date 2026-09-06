package metadata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// PayloadHeaderSize is the fixed 32-byte metadata block payload header.
const PayloadHeaderSize = 32

// DirectoryEntrySize is the fixed 32-byte metadata directory entry.
const DirectoryEntrySize = 32

// PayloadHeader is the fixed 32-byte header of an uncompressed Metadata Block
// payload, followed by directory entries and record bytes.
type PayloadHeader struct {
	ItemCount      uint32
	DirectoryBytes uint32
	RecordsBytes   uint64
}

// MarshalTo writes the header into dst.
func (h *PayloadHeader) MarshalTo(dst []byte) error {
	if len(dst) < PayloadHeaderSize {
		return fmt.Errorf("rowpack: metadata payload header destination too short")
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], fileformat.MagicMetaPayload)
	binary.LittleEndian.PutUint32(dst[8:], 1)
	binary.LittleEndian.PutUint32(dst[12:], DirectoryEntrySize)
	binary.LittleEndian.PutUint32(dst[16:], h.ItemCount)
	binary.LittleEndian.PutUint32(dst[20:], h.DirectoryBytes)
	binary.LittleEndian.PutUint64(dst[24:], h.RecordsBytes)
	return nil
}

// Unmarshal validates src and fills h.
func (h *PayloadHeader) Unmarshal(src []byte) error {
	if len(src) < PayloadHeaderSize {
		return errors.New("rowpack: truncated metadata payload header")
	}
	if !bytes.Equal(src[0:8], []byte(fileformat.MagicMetaPayload)) {
		return errors.New("rowpack: bad metadata payload magic")
	}
	if binary.LittleEndian.Uint32(src[8:]) != 1 {
		return errors.New("rowpack: unsupported metadata payload version")
	}
	if binary.LittleEndian.Uint32(src[12:]) != DirectoryEntrySize {
		return errors.New("rowpack: bad metadata directory entry size")
	}
	h.ItemCount = binary.LittleEndian.Uint32(src[16:])
	h.DirectoryBytes = binary.LittleEndian.Uint32(src[20:])
	if h.DirectoryBytes != h.ItemCount*DirectoryEntrySize {
		return fmt.Errorf("rowpack: metadata directory bytes %d != count %d * %d", h.DirectoryBytes, h.ItemCount, DirectoryEntrySize)
	}
	h.RecordsBytes = binary.LittleEndian.Uint64(src[24:])
	return nil
}

// DirectoryEntry is the fixed 32-byte metadata directory entry. recordCRC is
// the CRC of the associated record body (stored at offset 28).
type DirectoryEntry struct {
	ObjectID     uint64
	Revision     uint32
	RecordType   uint32
	RecordOffset uint32
	RecordLength uint32
	Operation    fileformat.Operation
	Critical     bool
	recordCRC    uint32
}

// RecordCRC returns the CRC of the associated record body.
func (e *DirectoryEntry) RecordCRC() uint32 { return e.recordCRC }

// SetRecordCRC sets the CRC of the associated record body.
func (e *DirectoryEntry) SetRecordCRC(crc uint32) { e.recordCRC = crc }

// MarshalTo writes the entry into dst.
func (e *DirectoryEntry) MarshalTo(dst []byte) error {
	if len(dst) < DirectoryEntrySize {
		return fmt.Errorf("rowpack: metadata directory entry destination too short")
	}
	for i := range dst {
		dst[i] = 0
	}
	binary.LittleEndian.PutUint64(dst[0:], e.ObjectID)
	binary.LittleEndian.PutUint32(dst[8:], e.Revision)
	binary.LittleEndian.PutUint32(dst[12:], e.RecordType)
	binary.LittleEndian.PutUint32(dst[16:], e.RecordOffset)
	binary.LittleEndian.PutUint32(dst[20:], e.RecordLength)
	dst[24] = byte(e.Operation)
	if e.Critical {
		dst[25] = fileformat.FlagCritical
	}
	binary.LittleEndian.PutUint32(dst[28:], e.recordCRC)
	return nil
}

// Unmarshal validates src and fills e.
func (e *DirectoryEntry) Unmarshal(src []byte) error {
	if len(src) < DirectoryEntrySize {
		return errors.New("rowpack: truncated metadata directory entry")
	}
	e.ObjectID = binary.LittleEndian.Uint64(src[0:])
	e.Revision = binary.LittleEndian.Uint32(src[8:])
	e.RecordType = binary.LittleEndian.Uint32(src[12:])
	e.RecordOffset = binary.LittleEndian.Uint32(src[16:])
	e.RecordLength = binary.LittleEndian.Uint32(src[20:])
	e.Operation = fileformat.Operation(src[24])
	e.Critical = src[25]&fileformat.FlagCritical != 0
	e.recordCRC = binary.LittleEndian.Uint32(src[28:])
	return nil
}

// Payload is a fully parsed metadata block payload.
type Payload struct {
	Header  PayloadHeader
	Entries []DirectoryEntry
	Records [][]byte // record bodies in entry order; nil for DELETE
}

// Parse decodes a metadata block payload, validating all bounds and CRCs.
func Parse(data []byte) (*Payload, error) {
	var h PayloadHeader
	if err := h.Unmarshal(data); err != nil {
		return nil, err
	}
	dirBytes := int(h.DirectoryBytes)
	base := PayloadHeaderSize + dirBytes
	if base > len(data) {
		return nil, errors.New("rowpack: metadata directory exceeds payload")
	}
	if h.RecordsBytes > uint64(len(data)-base) {
		return nil, errors.New("rowpack: metadata records exceed payload")
	}
	if base+int(h.RecordsBytes) != len(data) {
		return nil, fmt.Errorf("rowpack: metadata payload %d bytes, want %d", len(data), base+int(h.RecordsBytes))
	}
	p := &Payload{Header: h}
	pos := PayloadHeaderSize
	for i := uint32(0); i < h.ItemCount; i++ {
		if pos+DirectoryEntrySize > len(data) {
			return nil, errors.New("rowpack: metadata directory truncated")
		}
		var e DirectoryEntry
		if err := e.Unmarshal(data[pos : pos+DirectoryEntrySize]); err != nil {
			return nil, err
		}
		pos += DirectoryEntrySize
		p.Entries = append(p.Entries, e)
	}
	for i := range p.Entries {
		e := &p.Entries[i]
		if e.Operation == fileformat.OperationDelete {
			if e.RecordOffset != 0 || e.RecordLength != 0 || e.RecordCRC() != 0 {
				return nil, fmt.Errorf("rowpack: DELETE metadata entry %d carries record bytes", i)
			}
			p.Records = append(p.Records, nil)
			continue
		}
		off := int(e.RecordOffset)
		ln := int(e.RecordLength)
		if off < 0 || ln < 0 || off+ln > int(h.RecordsBytes) {
			return nil, fmt.Errorf("rowpack: metadata record %d out of bounds", i)
		}
		body := data[base+off : base+off+ln]
		if fileformat.CRC32C(body) != e.RecordCRC() {
			return nil, fmt.Errorf("rowpack: metadata record %d CRC mismatch", i)
		}
		p.Records = append(p.Records, body)
	}
	return p, nil
}

// Build assembles a metadata payload from entries and record bodies. Records
// must be provided in entry order; DELETE entries have nil bodies.
func Build(entries []DirectoryEntry, records [][]byte) ([]byte, error) {
	if len(entries) != len(records) {
		return nil, errors.New("rowpack: metadata build entry/record count mismatch")
	}
	dirBytes := len(entries) * DirectoryEntrySize
	recBytes := 0
	for _, r := range records {
		recBytes += len(r)
	}
	h := PayloadHeader{ItemCount: uint32(len(entries)), DirectoryBytes: uint32(dirBytes), RecordsBytes: uint64(recBytes)}
	hdr := make([]byte, PayloadHeaderSize)
	if err := h.MarshalTo(hdr); err != nil {
		return nil, err
	}
	dst := make([]byte, 0, PayloadHeaderSize+dirBytes+recBytes)
	dst = append(dst, hdr...)
	off := 0
	for i, e := range entries {
		body := records[i]
		if e.Operation != fileformat.OperationDelete {
			e.RecordOffset = uint32(off)
			e.RecordLength = uint32(len(body))
			e.SetRecordCRC(fileformat.CRC32C(body))
		}
		ent := make([]byte, DirectoryEntrySize)
		if err := e.MarshalTo(ent); err != nil {
			return nil, err
		}
		dst = append(dst, ent...)
		off += len(body)
	}
	for _, b := range records {
		dst = append(dst, b...)
	}
	return dst, nil
}
