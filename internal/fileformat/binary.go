package fileformat

import "encoding/binary"

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
