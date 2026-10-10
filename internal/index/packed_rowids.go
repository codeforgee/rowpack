package index

import (
	"encoding/binary"
	"math/bits"
	"sort"
)

// rowPackFrame is how many rows share one base value and one bit width inside a
// packed rowShard. RowIDs arrive sorted and, for any clustered key space, the
// delta inside a frame is far narrower than 64 bits — PackProfile measures
// ~3 bits/row on the dense sample and still ~20 bits/row at a 262k stride.
//
// It is a trade, not a free win. lookup narrows to a frame by binary search on
// base and then walks the frame linearly, so the frame width is also the
// point-read cost; meanwhile every frame carries a base/offset/width header, so
// a narrower frame buys latency with bytes. Measured on the sample: 128 gives
// the smallest column but costs ~25% on BenchmarkLatency/get_hot p50, 16 gives
// that latency back but costs ~1 B/row of headers and ~21% on
// BenchmarkOpenReplay. 64 lands between them on both axes.
const rowPackFrame = 16

// packedRowIDs is the frame-of-reference bit-packed form of a shard's RowID
// column. RowIDs are ascending, so each frame stores the absolute RowID of its
// first row plus the deltas to the following rows, all at the frame's width.
// Random access is O(rowPackFrame); sequential access amortizes to O(1) through
// the iterator's frame cursor (see rowShardIter).
//
// This replaces a []uint64 that cost 8 bytes/row regardless of how clustered
// the IDs were. It changes resident memory only — the on-disk index stream is
// already columnar and is untouched.
type packedRowIDs struct {
	n      int
	base   []uint64 // base[f]: absolute RowID of frame f's first row
	bitOff []uint64 // bitOff[f]: start bit of frame f within data
	width  []uint8  // width[f]: bits per delta in frame f; 0 when all are zero
	data   []byte   // bit-packed deltas, padded for safe multi-byte reads
}

// packRowIDs builds the packed form of a sorted RowID column.
func packRowIDs(rowIDs []uint64) packedRowIDs {
	n := len(rowIDs)
	if n == 0 {
		return packedRowIDs{}
	}
	frames := (n + rowPackFrame - 1) / rowPackFrame
	p := packedRowIDs{
		n:      n,
		base:   make([]uint64, frames),
		bitOff: make([]uint64, frames),
		width:  make([]uint8, frames),
	}
	// Pass 1: per-frame width. Slot 0 of every frame is the base itself, so
	// only the deltas after it set the width.
	var total uint64
	for f := 0; f < frames; f++ {
		start := f * rowPackFrame
		end := min(start+rowPackFrame, n)
		var maxDelta uint64
		for i := start + 1; i < end; i++ {
			if d := rowIDs[i] - rowIDs[i-1]; d > maxDelta {
				maxDelta = d
			}
		}
		w := bits.Len64(maxDelta)
		p.width[f] = uint8(w)
		p.bitOff[f] = total
		total += uint64(end-start) * uint64(w)
	}
	// +16: getBits reads up to 9 bytes past a bit offset; the slack lets it
	// stay branch-free without a per-read bounds check.
	p.data = make([]byte, total/8+16)
	// Pass 2: pack the deltas through a word buffer, so the cost is one store
	// per 8 bytes rather than one per bit. Slot 0 of each frame is the base, so
	// it writes a zero delta to keep the frame's slots aligned.
	w := bitWriter{dst: p.data}
	for f := 0; f < frames; f++ {
		start := f * rowPackFrame
		end := min(start+rowPackFrame, n)
		p.base[f] = rowIDs[start]
		width := int(p.width[f])
		w.write(0, width) // slot 0 == base
		for i := start + 1; i < end; i++ {
			w.write(rowIDs[i]-rowIDs[i-1], width)
		}
	}
	w.flush()
	return p
}

// at returns the RowID at shard index i.
func (p *packedRowIDs) at(i int) uint64 {
	f := i / rowPackFrame
	w := int(p.width[f])
	off := int(p.bitOff[f])
	v := p.base[f]
	for k := i - f*rowPackFrame; k > 0; k-- {
		v += getBits(p.data, off+k*w, w)
	}
	return v
}

// search finds rowID and returns its shard index, or the index where it would
// land plus false. Frame-level binary search on base, then a linear walk inside
// the frame: at most rowPackFrame delta reads, and the walk stops early as soon
// as the running RowID passes the target.
func (p *packedRowIDs) search(rowID uint64) (int, bool) {
	if p.n == 0 {
		return 0, false
	}
	f := sort.Search(len(p.base), func(f int) bool { return p.base[f] > rowID }) - 1
	if f < 0 {
		return 0, false
	}
	start := f * rowPackFrame
	end := min(start+rowPackFrame, p.n)
	w := int(p.width[f])
	off := int(p.bitOff[f])
	v := p.base[f]
	if v == rowID {
		return start, true
	}
	for k := 1; k < end-start; k++ {
		v += getBits(p.data, off+k*w, w)
		if v == rowID {
			return start + k, true
		}
		if v > rowID {
			return start + k, false
		}
	}
	return end, false
}

// bytes is the resident cost of the packed column: the packed data plus the
// per-frame base/offset/width tables.
func (p *packedRowIDs) bytes() uint64 {
	frames := uint64(len(p.base))
	return uint64(len(p.data)) + frames*8 + frames*8 + frames
}

// bitWriter packs fixed-width values into a byte slice, buffering bits in a
// uint64 and flushing whole words. Packing runs on every shard build (Open,
// replay, commit), so it is worth the buffer: one store per 8 bytes instead of
// one per bit, which is what made BenchmarkOpenReplay regress ~30% when it was
// written bit at a time.
type bitWriter struct {
	dst []byte
	off int    // byte offset of the next word to flush
	acc uint64 // buffered bits, little-endian
	n   uint   // valid bits in acc
}

// write appends the low width bits of v. width is at most 64.
func (w *bitWriter) write(v uint64, width int) {
	if width == 0 {
		return
	}
	if w.n+uint(width) > 64 {
		// Split across the word boundary: fill acc, flush, keep the rest.
		low := 64 - w.n
		w.acc |= (v & (1<<low - 1)) << w.n
		binary.LittleEndian.PutUint64(w.dst[w.off:], w.acc)
		w.off += 8
		v >>= low
		width -= int(low)
		w.acc = 0
		w.n = 0
	}
	w.acc |= v << w.n
	w.n += uint(width)
}

// flush stores any bits still buffered.
func (w *bitWriter) flush() {
	for w.n > 0 {
		w.dst[w.off] = byte(w.acc)
		w.off++
		w.acc >>= 8
		w.n -= min(w.n, 8)
	}
}

// getBits reads width bits starting at bit offset bitOff. It loads whole bytes
// and shifts rather than walking bit by bit, because it sits on the point-read
// and scan hot path.
func getBits(src []byte, bitOff, width int) uint64 {
	if width == 0 {
		return 0
	}
	byteIdx := bitOff >> 3
	shift := uint(bitOff & 7)
	var v uint64
	for k := uint(0); k < (shift+uint(width)+7)/8; k++ {
		v |= uint64(src[byteIdx+int(k)]) << (8 * k)
	}
	v >>= shift
	if width < 64 {
		v &= (uint64(1) << width) - 1
	}
	return v
}
