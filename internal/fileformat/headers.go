package fileformat

import (
	"bytes"
	"encoding/binary"
)

// FileHeader is the shared logical content of the 128-byte .rpk and .rpi
// headers. The two files carry the same version and StoreUUID so Open can
// verify they are a matching pair.
type FileHeader struct {
	StoreUUID          [16]byte
	CreatedUnixNano    int64
	RequiredFeatures   uint64
	OptionalFeatures   uint64
	DefaultBlockSize   uint32
	DefaultCompression Compression
	DefaultRowEncoding RowEncoding
	Flags              uint16

	// Encryption (v1, optional at Create). Zero values mean a plain store.
	// The fields live in the reserved region and leave non-encrypted stores
	// byte-identical to the previous format.
	EncryptionAlgorithm EncryptionAlgorithm
	NonceScheme         NonceScheme
	KeyID               []byte // <= FileHeaderKeyIDMaxLen bytes, ASCII
}

// Size returns the serialized size.
func (h *FileHeader) Size() int { return DataFileHeaderSize }

// marshalTo writes h for the given magic into dst. dst must have room for
// Size() bytes.
func (h *FileHeader) marshalTo(dst []byte, magic string) error {
	if len(dst) < DataFileHeaderSize {
		return formatError("FileHeader", -1, "destination too short: have %d want %d", len(dst), DataFileHeaderSize)
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst[0:8], magic)
	putU16(dst[8:], VersionMajor)
	putU16(dst[10:], VersionMinor)
	putU32(dst[12:], DataFileHeaderSize)
	copy(dst[16:32], h.StoreUUID[:])
	putU64(dst[32:], uint64(h.CreatedUnixNano))
	putU64(dst[40:], h.RequiredFeatures)
	putU64(dst[48:], h.OptionalFeatures)
	putU32(dst[56:], h.DefaultBlockSize)
	dst[60] = byte(h.DefaultCompression)
	dst[61] = byte(h.DefaultRowEncoding)
	putU16(dst[62:], h.Flags)
	// Encryption fields in the reserved region (offset 64..).
	dst[FileHeaderEncAlgoOffset] = byte(h.EncryptionAlgorithm)
	dst[FileHeaderNonceSchemeOff] = byte(h.NonceScheme)
	if len(h.KeyID) > FileHeaderKeyIDMaxLen {
		return formatError("FileHeader", FileHeaderKeyIDLenOffset, "key id too long: %d > %d", len(h.KeyID), FileHeaderKeyIDMaxLen)
	}
	dst[FileHeaderKeyIDLenOffset] = byte(len(h.KeyID))
	copy(dst[FileHeaderKeyIDOffset:], h.KeyID)
	// offset 98..120 reserved (zeroed above)
	finalizeCRC(dst[:DataFileHeaderSize], DataFileHeaderCRC32COffset)
	// offset 124..128 ReservedCRC (zeroed above)
	return nil
}

// unmarshal validates src as a header with the given magic and fills h. It
// returns the validated CRC. Reserved fields are ignored when reading v1.
func (h *FileHeader) unmarshal(src []byte, magic string) (uint32, error) {
	if len(src) < DataFileHeaderSize {
		return 0, formatError("FileHeader", -1, errShortInput)
	}
	if !bytes.Equal(src[0:8], []byte(magic)) {
		return 0, formatError("FileHeader", 0, "%s: got %q", errBadMagic, src[0:8])
	}
	maj, ok := getU16(src[8:])
	if !ok || maj != VersionMajor {
		v, _ := getU16(src[8:])
		return 0, formatError("FileHeader", 8, "%s: major=%d want %d", errBadVersion, v, VersionMajor)
	}
	sz, ok := getU32(src[12:])
	if !ok || sz != DataFileHeaderSize {
		v, _ := getU32(src[12:])
		return 0, formatError("FileHeader", 12, "%s: size=%d want %d", errBadSize, v, DataFileHeaderSize)
	}
	crc, err := verifyCRC(src[:DataFileHeaderSize], DataFileHeaderCRC32COffset)
	if err != nil {
		return 0, formatError("FileHeader", DataFileHeaderCRC32COffset, "%v", err)
	}
	copy(h.StoreUUID[:], src[16:32])
	h.CreatedUnixNano = int64(binary.LittleEndian.Uint64(src[32:]))
	h.RequiredFeatures = binary.LittleEndian.Uint64(src[40:])
	h.OptionalFeatures = binary.LittleEndian.Uint64(src[48:])
	h.DefaultBlockSize = binary.LittleEndian.Uint32(src[56:])
	h.DefaultCompression = Compression(src[60])
	h.DefaultRowEncoding = RowEncoding(src[61])
	h.Flags = binary.LittleEndian.Uint16(src[62:])
	h.EncryptionAlgorithm = EncryptionAlgorithm(src[FileHeaderEncAlgoOffset])
	h.NonceScheme = NonceScheme(src[FileHeaderNonceSchemeOff])
	kl := int(src[FileHeaderKeyIDLenOffset])
	if kl > FileHeaderKeyIDMaxLen {
		return 0, formatError("FileHeader", FileHeaderKeyIDLenOffset, "key id length %d exceeds %d", kl, FileHeaderKeyIDMaxLen)
	}
	if kl > 0 {
		h.KeyID = append([]byte(nil), src[FileHeaderKeyIDOffset:FileHeaderKeyIDOffset+kl]...)
	}
	return crc, nil
}

// DataFileHeader is the fixed 128-byte header of a .rpk data file.
type DataFileHeader struct {
	FileHeader
}

// MarshalTo writes the serialized form of h into dst.
func (h *DataFileHeader) MarshalTo(dst []byte) error {
	return h.FileHeader.marshalTo(dst, MagicDataFile)
}

// Unmarshal validates src and fills h.
func (h *DataFileHeader) Unmarshal(src []byte) error {
	_, err := h.FileHeader.unmarshal(src, MagicDataFile)
	return err
}

// IndexFileHeader was removed with the separate index file (v2 single-file
// stores embed the IndexTxn stream in the .rpk).

// CheckVersion verifies that a header's minor version and feature bits are
// openable. A higher minor version is acceptable only when every required
// feature bit is recognized.
func (h *FileHeader) CheckVersion() error {
	if h.RequiredFeatures & ^(uint64(0x0F)) != 0 {
		return formatError("FileHeader", 40, "unknown required feature bits: %b", h.RequiredFeatures)
	}
	return nil
}
