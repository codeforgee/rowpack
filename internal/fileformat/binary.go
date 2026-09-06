package fileformat

import "encoding/binary"

// align8 rounds n up to the next multiple of 8. Top-level structures and
// snapshot end offsets are 8-byte aligned.
func align8(n int) int { return (n + 7) &^ 7 }

// align8u is the uint64 variant of align8.
func align8u(n uint64) uint64 { return (n + 7) &^ 7 }

// Put helpers write Little Endian values. They are used only on buffers whose
// capacity is guaranteed by the caller (MarshalTo checks size first).
func putU16(dst []byte, v uint16) { binary.LittleEndian.PutUint16(dst, v) }
func putU32(dst []byte, v uint32) { binary.LittleEndian.PutUint32(dst, v) }
func putU64(dst []byte, v uint64) { binary.LittleEndian.PutUint64(dst, v) }

// Get helpers read Little Endian values with bounds validation. The bool
// result is false when src is shorter than the field, so untrusted input can
// never trigger an out-of-range panic.
func getU16(src []byte) (uint16, bool) {
	if len(src) < 2 {
		return 0, false
	}
	return binary.LittleEndian.Uint16(src), true
}

func getU32(src []byte) (uint32, bool) {
	if len(src) < 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(src), true
}

func getU64(src []byte) (uint64, bool) {
	if len(src) < 8 {
		return 0, false
	}
	return binary.LittleEndian.Uint64(src), true
}

// isZeroBytes reports whether b is empty or all zero.
func isZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

// padTo writes zero padding from len(b) up to align8(len(b)) into b and
// returns the padded slice. Padding bytes are never part of any CRC.
func padTo(b []byte) []byte {
	for len(b)%Align != 0 {
		b = append(b, 0)
	}
	return b
}
