package format

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
// zero. It avoids building a concatenated slice on decode hot paths
// (verifyCRC runs once per fixed structure and per block payload).
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

// CRC32CConcat extends a running CRC-32C with one more slice.
func CRC32CConcat(c uint32, b []byte) uint32 {
	return crc32.Update(c, castagnoli, b)
}

// CRC32CAdvance returns the CRC-32C register state as if n zero bytes had
// been appended after the bytes that produced crc. It is the linear "shift"
// half of CRC combining: because the CRC is GF(2)-linear in (initial
// register, message), appending bytes advances the register through a fixed
// linear operator, so for any byte strings X and Y
//
//	CRC32C(X || Y) == CRC32CAdvance(CRC32C(X), len(Y)) ^ CRC32C(Y)
//
// regardless of how Y was chunked or in which order its parts were folded
// into CRC32C(Y) starting from zero (crc32.Update is associative). This lets
// a producer extend a plaintext CRC with bytes that only become known at the
// end — e.g. a body whose first segment's content depends on the total
// length — without retaining every earlier segment for a final pass.
func CRC32CAdvance(crc uint32, n uint64) uint32 {
	if n == 0 {
		return crc
	}
	const poly = 0x82f63b78 // Castagnoli, reflected form
	// Operator rows: mat[i] is the image of register bit i (1<<i) under the
	// operator. One zero BIT: bit 0 shifts out and pulls the polynomial in;
	// every other bit shifts down one place.
	bit := [32]uint32{poly}
	for i := 1; i < 32; i++ {
		bit[i] = 1 << (i - 1)
	}
	// Square up from the one-bit operator to the one-BYTE operator (8 zero
	// bits), then exponentiate byte-wise over n.
	byteOp := bit
	for k := 0; k < 3; k++ {
		byteOp = crc32MatSquare(&byteOp)
	}
	// result starts as the identity operator; square-and-multiply folds in
	// byteOp^n. All factors are powers of the same operator, so composition
	// order does not matter.
	result := [32]uint32{}
	for i := 0; i < 32; i++ {
		result[i] = 1 << i
	}
	base := byteOp
	for m := n; m > 0; m >>= 1 {
		if m&1 != 0 {
			var next [32]uint32
			for i := 0; i < 32; i++ {
				next[i] = crc32MatTime(&base, result[i])
			}
			result = next
		}
		if m > 1 {
			base = crc32MatSquare(&base)
		}
	}
	return crc32MatTime(&result, crc)
}

// crc32MatTime applies the linear operator mat to the register value vec:
// the XOR of the rows selected by vec's set bits.
func crc32MatTime(mat *[32]uint32, vec uint32) uint32 {
	sum := uint32(0)
	for i := 0; vec != 0; i++ {
		if vec&1 != 0 {
			sum ^= mat[i]
		}
		vec >>= 1
	}
	return sum
}

// crc32MatSquare returns the operator mat composed with itself.
func crc32MatSquare(mat *[32]uint32) [32]uint32 {
	var out [32]uint32
	for i := 0; i < 32; i++ {
		out[i] = crc32MatTime(mat, mat[i])
	}
	return out
}
