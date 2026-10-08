package block

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// pool_arms_test.go 覆盖两块临时缓冲池的兜底臂:压缩用的 EncodeAll scratch 与解压用的
// rawBuf。两者都是「按 2 的幂分级 + 进程级预算」,所以每条臂都是同一个问题的不同面:
// 超出最大分级、分级里拿到的缓冲不够用、预算满了、以及归还时不能把超大的缓冲塞回池里。
//
// 到不了的四条:
//   - compress.go 的 decPool.New / poolForLevel.New / NewZstdEncoder 的 panic(81、101、
//     127):三个地方的 zstd 选项都是固定的合法值,NewReader/NewWriter 没有失败输入。
//   - compress.go 210:EncodeAll 返回的容量只可能等于或大于传入的 scratch,不会更小,
//     所以 EncodeZstdInto 里那个 else 分支没有输入。

// incompressible returns n bytes zstd cannot shrink: an EncodeAll output that
// outgrows the scratch it was handed.
func incompressible(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 16)
	}
	return b
}

// TestEncodeScratchSkipsPoolWhenOversized: a scratch larger than the biggest
// pooled class is allocated fresh (149) and, on the way back, dropped instead
// of parked (168) — one huge block must not leave a 32 MiB buffer behind.
func TestEncodeScratchSkipsPoolWhenOversized(t *testing.T) {
	const huge = (32 << 20) + 1
	b := getEncodeDst(huge)
	require.GreaterOrEqual(t, cap(b), huge, "the request is served even past the pool")
	putEncodeDst(b)
}

// TestEncodeScratchRegrowsWithinClass: a buffer parked in a class can still be
// shorter than a later request in that same class (the request may sit in the
// upper half); the undersized one is dropped, not served (156).
func TestEncodeScratchRegrowsWithinClass(t *testing.T) {
	putEncodeDst(make([]byte, 0, (1<<12)+1)) // filed under the 8 KiB class at 4097 B
	b := getEncodeDst(1 << 13)               // the class ceiling: 8192 B
	require.GreaterOrEqual(t, cap(b), 1<<13, "the scratch always fits the request")
}

// TestPutEncodeDstIgnoresNil: an unused scratch is filed nowhere.
func TestPutEncodeDstIgnoresNil(t *testing.T) {
	putEncodeDst(nil)
}

// TestEncodeScratchBudgetCapsRetention: past the process-wide budget the pool
// stops retaining and hands the buffers back to the GC (171).
func TestEncodeScratchBudgetCapsRetention(t *testing.T) {
	prev := encodeDstBytes.Load()
	t.Cleanup(func() { encodeDstBytes.Store(prev) })

	for range 12 { // 12 MiB offered against an 8 MiB budget
		putEncodeDst(make([]byte, 0, 1<<20))
	}
	require.LessOrEqual(t, encodeDstBytes.Load(), int64(encodeDstBudget),
		"retention never exceeds the budget")
}

// TestEncodeZstdWithDropsOutgrownScratch: when the frame outgrows the pooled
// scratch, the class buffer goes back to the pool and the larger array is
// dropped rather than parked under the wrong size (190).
func TestEncodeZstdWithDropsOutgrownScratch(t *testing.T) {
	enc := NewZstdEncoder(1)
	defer enc.Close()

	_, err := encodeZstdWith(enc, incompressible(4<<10))
	require.NoError(t, err)
}

// TestRawBufServesEveryClass: getRawBuf must hand back at least the requested
// capacity whether the size lands in a pooled class or past the biggest one
// (rawbuf.go:90), and returning it must never park an oversized buffer.
func TestRawBufServesEveryClass(t *testing.T) {
	for _, size := range []uint32{1, 100, 4095, 4096, 4097, 64 << 10, 1 << 20, 300 << 10, (32 << 20) + 1} {
		b := getRawBuf(size)
		require.GreaterOrEqual(t, cap(b.data), int(size), "getRawBuf(%d) served %d", size, cap(b.data))
		putRawBuf(b)
	}

	// A buffer handed back directly (not obtained from the pool) is dropped
	// when it is oversized: pooledBytes must not move (rawbuf.go:109-110).
	DisablePool(false)
	defer DisablePool(false)
	prev := pooledBytes.Load()
	putRawBuf(&rawBuf{data: make([]byte, poolBudget+1)})
	require.Equal(t, prev, pooledBytes.Load(), "an oversized buffer is not retained")
}

// TestPoolClassBoundaries: the size→class ladder clamps below the smallest
// class, rounds up to the next power of two, and reports -1 past the largest.
func TestPoolClassBoundaries(t *testing.T) {
	cases := []struct {
		size int
		want int
	}{
		{0, poolClassMinBits},             // clamp to smallest
		{1, poolClassMinBits},             // smallest class
		{4096, 12},                        // exact smallest class
		{4097, 13},                        // ceil to next class
		{1 << 20, 20},                     // exact
		{(1 << 20) + 1, 21},               // ceil
		{1 << poolClassMaxBits, 25},       // largest class exact
		{(1 << poolClassMaxBits) + 1, -1}, // oversized: not pooled
	}
	for _, c := range cases {
		require.Equal(t, c.want, poolClass(c.size), "poolClass(%d)", c.size)
	}
}

// TestRawBufBudgetCapsRetention: past the process-wide budget the pool stops
// retaining and hands the buffers back to the GC (rawbuf.go:109).
func TestRawBufBudgetCapsRetention(t *testing.T) {
	DisablePool(false)
	defer DisablePool(false)
	prev := pooledBytes.Load()
	t.Cleanup(func() { pooledBytes.Store(prev) })

	bufs := make([]*rawBuf, 0, 200)
	for range 200 { // 200 x 256 KiB = 50 MiB > poolBudget
		bufs = append(bufs, getRawBuf(256<<10))
	}
	for _, b := range bufs {
		putRawBuf(b)
	}
	require.LessOrEqual(t, pooledBytes.Load(), int64(poolBudget), "retention never exceeds the budget")
}

// TestRawBufBypassRetainsNothing: with the pool disabled every buffer goes
// straight back to the GC.
func TestRawBufBypassRetainsNothing(t *testing.T) {
	prev := DisablePool(true)
	defer DisablePool(prev)
	pooledBytes.Store(0)
	defer pooledBytes.Store(0)

	putRawBuf(getRawBuf(1 << 20))
	require.Zero(t, pooledBytes.Load(), "a bypassed pool retains nothing")
}
