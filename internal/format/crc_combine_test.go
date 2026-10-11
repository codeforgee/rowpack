package format

import (
	"math/rand/v2"
	"testing"
)

// TestCRC32CAdvanceCombine pins the combining identity that CRC32CAdvance
// exists for: for any byte strings X and Y,
//
//	CRC32C(X || Y) == CRC32CAdvance(CRC32C(X), len(Y)) ^ CRC32C(Y)
//
// across sizes that exercise every exponentiation path (sub-byte, exactly one
// byte, multi-byte, and >2^32 zero bytes via the matrix exponent).
func TestCRC32CAdvanceCombine(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	randBytes := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		return b
	}
	// Deterministic sizes plus randomized ones; include len(Y) values that
	// straddle byte and matrix-power boundaries.
	sizes := []int{0, 1, 3, 8, 9, 64, 100, 4096, 1 << 16}
	for i := 0; i < 40; i++ {
		sizes = append(sizes, rng.IntN(1<<18))
	}
	for _, yLen := range sizes {
		x := randBytes(rng.IntN(1 << 12))
		y := randBytes(yLen)
		want := CRC32CConcat(CRC32C(x), y)
		got := CRC32CAdvance(CRC32C(x), uint64(len(y))) ^ CRC32C(y)
		if got != want {
			t.Fatalf("len(Y)=%d: combined CRC %#08x, want %#08x", yLen, got, want)
		}
	}
	// Huge shift lengths: the operator exponentiation must stay consistent
	// when composed, without materializing that many zero bytes — advancing
	// by n1+n2 equals advancing by n1 then n2.
	x := randBytes(64)
	for _, pair := range [][2]uint64{
		{1 << 20, 1 << 21},
		{(uint64(1) << 32), 1 << 12},
		{(uint64(1) << 40) + 12345, 1 << 20},
	} {
		once := CRC32CAdvance(CRC32C(x), pair[0]+pair[1])
		twice := CRC32CAdvance(CRC32CAdvance(CRC32C(x), pair[0]), pair[1])
		if once != twice {
			t.Fatalf("advance not composable at %v: %#08x vs %#08x", pair, once, twice)
		}
	}
}

// TestCRC32CAdvanceZero pins the degenerate cases: advancing zero bytes is a
// no-op, and the operator is compatible with chunked Update chains (feeding
// the tail in any partition from zero yields the same CRC).
func TestCRC32CAdvanceZero(t *testing.T) {
	if got := CRC32CAdvance(0xdeadbeef, 0); got != 0xdeadbeef {
		t.Fatalf("zero-length advance changed the register: %#08x", got)
	}
	// Associativity of Update across partitions: CRC(0, A||B||C) computed as
	// one chain equals per-part CRCs folded in any order via Advance+XOR.
	rng := rand.New(rand.NewPCG(3, 4))
	a, b, c := make([]byte, 1000), make([]byte, 777), make([]byte, 13)
	for i := range a {
		a[i] = byte(rng.IntN(256))
	}
	for i := range b {
		b[i] = byte(rng.IntN(256))
	}
	for i := range c {
		c[i] = byte(rng.IntN(256))
	}
	direct := CRC32CConcat(CRC32CConcat(CRC32C(a), b), c)
	// Fold the tail (B then C — the physical order; Update chains are
	// order-dependent for content, only chunk boundaries are free) starting
	// from zero, then shift the prefix CRC past both and combine.
	tail := CRC32CConcat(CRC32C(b), c)
	got := CRC32CAdvance(CRC32C(a), uint64(len(b)+len(c))) ^ tail
	if got != direct {
		t.Fatalf("partitioned combine %#08x, want %#08x", got, direct)
	}
}
