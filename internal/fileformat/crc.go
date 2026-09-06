package fileformat

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

// castagnoli is the CRC-32C (Castagnoli) table mandated by the v1 spec.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C computes CRC-32C over b.
func CRC32C(b []byte) uint32 {
	return crc32.Update(0, castagnoli, b)
}

// CRC32CConcat computes CRC-32C over a sequence of slices as if they were
// concatenated. Used for Block Header CRC value strings and txn bodies.
func CRC32CConcat(parts ...[]byte) uint32 {
	c := uint32(0)
	for _, p := range parts {
		c = crc32.Update(c, castagnoli, p)
	}
	return c
}

// finalizeCRC writes the CRC of buf into the 4-byte field at crcOff. The
// field itself is treated as zero during computation, per the v1 rule that a
// fixed structure's CRC covers the whole structure with its own CRC field
// zeroed. It returns the computed CRC.
func finalizeCRC(buf []byte, crcOff int) uint32 {
	putU32(buf[crcOff:], 0)
	c := CRC32C(buf)
	putU32(buf[crcOff:], c)
	return c
}

// verifyCRC verifies the 4-byte CRC field at crcOff of buf, treating the field
// as zero during computation. The field is restored afterwards so callers can
// keep using the buffer. The returned value is the stored CRC.
func verifyCRC(buf []byte, crcOff int) (uint32, error) {
	if len(buf) < crcOff+4 {
		return 0, fmt.Errorf("buffer too short for CRC field at %d: have %d", crcOff, len(buf))
	}
	want := binary.LittleEndian.Uint32(buf[crcOff:])
	putU32(buf[crcOff:], 0)
	got := CRC32C(buf)
	putU32(buf[crcOff:], want)
	if got != want {
		return 0, fmt.Errorf("CRC mismatch: stored=0x%08x computed=0x%08x", want, got)
	}
	return want, nil
}
