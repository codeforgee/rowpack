package index

import "math/bits"

// ordPackFrame is how many ordinals share one bit width.
//
// This column stores raw values, not deltas: ItemOrdinal is a row's index
// inside its block, so it resets to 0 at every block run and has no
// monotonicity to exploit. That difference matters — random access is a direct
// index (bitOff[f] + k*width[f]) rather than the frame walk packedRowIDs needs,
// so the frame width carries no latency cost the way rowPackFrame does. It is
// a pure memory trade: wider frames let one large ordinal widen the whole
// frame, narrower frames pay more per-frame headers.
//
// Measured on the sample (idxOrdinalsBytes / heapAfterOpenBytes):
//
//	16   1,250,226 / 3,287,408
//	32   1,073,378 / 3,039,304
//	64     986,795 / 2,822,152
//	128    946,535 / 2,786,616   <- smallest heap
//	256    930,999 / 2,902,496   column smaller, heap larger
//
// 128 it is: past that the column keeps shrinking by less than the allocator's
// rounding costs it, so the heap stops tracking the column.
const ordPackFrame = 128

// packedOrdinals is the bit-packed form of a shard's ItemOrdinal column.
// ItemOrdinal is bounded by the block's row count and resets per run, so it
// stays a handful of bits in practice, while the raw []uint32 paid 4 bytes per
// row regardless. Like packedRowIDs this only changes resident memory.
type packedOrdinals struct {
	n      int
	bitOff []uint64 // bitOff[f]: start bit of frame f within data
	width  []uint8  // width[f]: bits per ordinal in frame f; 0 when all are zero
	data   []byte   // bit-packed ordinals, padded for safe multi-byte reads
}

// packOrdinals builds the packed form of an ItemOrdinal column.
func packOrdinals(ordinals []uint32) packedOrdinals {
	n := len(ordinals)
	if n == 0 {
		return packedOrdinals{}
	}
	frames := (n + ordPackFrame - 1) / ordPackFrame
	p := packedOrdinals{
		n:      n,
		bitOff: make([]uint64, frames),
		width:  make([]uint8, frames),
	}
	// Pass 1: per-frame width from the largest ordinal in the frame.
	var total uint64
	for f := 0; f < frames; f++ {
		start := f * ordPackFrame
		end := min(start+ordPackFrame, n)
		var maxOrd uint32
		for i := start; i < end; i++ {
			if ordinals[i] > maxOrd {
				maxOrd = ordinals[i]
			}
		}
		w := bits.Len32(maxOrd)
		p.width[f] = uint8(w)
		p.bitOff[f] = total
		total += uint64(end-start) * uint64(w)
	}
	// +16: getBits reads up to 9 bytes past a bit offset.
	p.data = make([]byte, total/8+16)
	// Pass 2: pack. Every slot in a frame is used — there is no base row here,
	// unlike packedRowIDs where slot 0 holds the frame's absolute RowID.
	w := bitWriter{dst: p.data}
	for f := 0; f < frames; f++ {
		start := f * ordPackFrame
		end := min(start+ordPackFrame, n)
		width := int(p.width[f])
		for i := start; i < end; i++ {
			w.write(uint64(ordinals[i]), width)
		}
	}
	w.flush()
	return p
}

// at returns the ItemOrdinal at shard index i. A direct bit offset, not a
// scan: ordinals are stored as raw values, so there is no prefix sum to replay.
func (p *packedOrdinals) at(i int) uint32 {
	f := i / ordPackFrame
	w := int(p.width[f])
	return uint32(getBits(p.data, int(p.bitOff[f])+(i-f*ordPackFrame)*w, w))
}

// bytes is the resident cost of the packed column: the packed data plus the
// per-frame offset/width tables. There is no base table, because a frame's
// values are not deltas.
func (p *packedOrdinals) bytes() uint64 {
	frames := uint64(len(p.bitOff))
	return uint64(len(p.data)) + frames*8 + frames
}
