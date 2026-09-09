package cache

import "testing"

func TestNilLRUGetIsSafe(t *testing.T) {
	var c *LRU
	if _, ok := c.Get(1); ok {
		t.Fatal("nil cache returned a value")
	}
}

func TestGrowingValueIsEvictedWhenItExceedsCapacity(t *testing.T) {
	c := NewLRU(100)
	c.Put(1, 20, "small")
	c.Put(1, 101, "large")
	if _, ok := c.Get(1); ok {
		t.Fatal("oversized grown value remained cached")
	}
	if got := c.UsedBytes(); got != 0 {
		t.Fatalf("used bytes = %d, want 0", got)
	}
}
