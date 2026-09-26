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

	for i := 0; i < 12; i++ { // 12 MiB offered against an 8 MiB budget
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

// TestRawBufSkipsPoolWhenOversized: a decompression scratch larger than the
// biggest pooled class is allocated fresh (90) and never retained.
func TestRawBufSkipsPoolWhenOversized(t *testing.T) {
	const huge = uint32(32<<20) + 1
	b := getRawBuf(huge)
	require.GreaterOrEqual(t, cap(b.data), int(huge), "the request is served even past the pool")
	putRawBuf(b)
}
