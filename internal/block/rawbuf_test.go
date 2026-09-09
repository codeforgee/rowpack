package block

import (
	"testing"
)

func TestPoolClass(t *testing.T) {
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
		if got := poolClass(c.size); got != c.want {
			t.Errorf("poolClass(%d) = %d, want %d", c.size, got, c.want)
		}
	}
}

func TestGetRawBufSatisfiesRequest(t *testing.T) {
	for _, size := range []uint32{1, 100, 4095, 4096, 4097, 64 << 10, 1 << 20, 300 << 10} {
		b := getRawBuf(size)
		if cap(b.data) < int(size) {
			t.Fatalf("getRawBuf(%d) capacity %d too small", size, cap(b.data))
		}
		putRawBuf(b)
	}
}

func TestPoolGradeDoesNotRetainOversized(t *testing.T) {
	prev := pooledBytes.Load()
	defer SetPoolDisabled(false)
	SetPoolDisabled(false)

	big := make([]byte, poolBudget+1)
	putRawBuf(&rawBuf{data: big}) // oversized: dropped, not counted
	if got := pooledBytes.Load(); got != prev {
		t.Fatalf("oversized buffer retained: %d -> %d", prev, got)
	}
}

func TestPoolBudgetCap(t *testing.T) {
	SetPoolDisabled(false)
	defer SetPoolDisabled(false)
	pooledBytes.Store(0)
	defer pooledBytes.Store(0)

	bufs := make([]*rawBuf, 0, 200)
	// 200 x 256 KiB = 50 MiB > poolBudget: retention must stop at the cap.
	for i := 0; i < 200; i++ {
		b := getRawBuf(256 << 10)
		bufs = append(bufs, b)
	}
	for _, b := range bufs {
		putRawBuf(b)
	}
	if got := pooledBytes.Load(); got > poolBudget {
		t.Fatalf("pooled retention %d exceeds budget %d", got, poolBudget)
	}
}

func TestSetPoolDisabledBypass(t *testing.T) {
	prev := SetPoolDisabled(true)
	defer SetPoolDisabled(prev)

	pooledBytes.Store(0)
	defer pooledBytes.Store(0)

	b := getRawBuf(1 << 20)
	putRawBuf(b) // must not be retained while bypassed
	if got := pooledBytes.Load(); got != 0 {
		t.Fatalf("bypassed pool retained %d bytes", got)
	}
}
