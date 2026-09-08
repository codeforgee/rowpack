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

// zero4 is a shared zero block for CRC span bridging; it is never mutated.
var zero4 [4]byte

// crc32cZeroGap computes CRC-32C over buf as if the 4 bytes at crcOff were
// zero. It avoids the variadic allocation of CRC32CConcat on decode hot
// paths (verifyCRC runs once per fixed structure and per block payload).
func crc32cZeroGap(buf []byte, crcOff int) uint32 {
	c := crc32.Update(0, castagnoli, buf[:crcOff])
	c = crc32.Update(c, castagnoli, zero4[:])
	return crc32.Update(c, castagnoli, buf[crcOff+4:])
}

// verifyCRC verifies the 4-byte CRC field at crcOff of buf, treating the field
// as zero during computation. buf is not modified: the CRC is computed over
// the spans before and after the field plus four zero bytes, so decoders can
// safely accept buffers that alias read-only file mappings (mmap views).
// The returned value is the stored CRC.
func verifyCRC(buf []byte, crcOff int) (uint32, error) {
	if len(buf) < crcOff+4 {
		return 0, fmt.Errorf("buffer too short for CRC field at %d: have %d", crcOff, len(buf))
	}
	want := binary.LittleEndian.Uint32(buf[crcOff:])
	if got := crc32cZeroGap(buf, crcOff); got != want {
		return 0, fmt.Errorf("CRC mismatch: stored=0x%08x computed=0x%08x", want, got)
	}
	return want, nil
}

// CRC32CConcat2 extends a running CRC-32C with one more slice.
func CRC32CConcat2(c uint32, b []byte) uint32 {
	return crc32.Update(c, castagnoli, b)
}
