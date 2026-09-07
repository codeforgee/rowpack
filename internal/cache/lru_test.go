package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLRUBasic(t *testing.T) {
	l := NewLRU(100)
	l.Put(1, 30, bytesN(30))
	l.Put(2, 30, bytesN(30))
	require.Equal(t, uint64(60), l.UsedBytes(), "used = %d", l.UsedBytes())
	_, ok := l.Get(1)
	require.True(t, ok, "miss on cached")
	require.Equal(t, uint64(1), l.Hits(), "hits/misses = %d/%d", l.Hits(), l.Misses())
	require.Equal(t, uint64(0), l.Misses(), "hits/misses = %d/%d", l.Hits(), l.Misses())
	_, ok = l.Get(99)
	require.False(t, ok, "hit on uncached")
	require.Equal(t, uint64(1), l.Misses(), "misses = %d", l.Misses())
}

func TestLRUEviction(t *testing.T) {
	l := NewLRU(100)
	l.Put(1, 40, bytesN(40))
	l.Put(2, 40, bytesN(40))
	l.Put(3, 40, bytesN(40)) // 120 > 100 -> evict 1
	require.Equal(t, uint64(80), l.UsedBytes(), "used = %d", l.UsedBytes())
	_, ok := l.Get(1)
	require.False(t, ok, "evicted entry still cached")
	require.Equal(t, uint64(1), l.Evictions(), "evictions = %d", l.Evictions())
	// Accessing 2 makes it most-recent; inserting 4 evicts 3.
	l.Get(2)
	l.Put(4, 40, bytesN(40))
	_, ok = l.Get(3)
	require.False(t, ok, "LRU did not evict least-recently-used")
	_, ok = l.Get(2)
	require.True(t, ok, "recently used entry evicted")
}

func TestLRUUsesLRUOrder(t *testing.T) {
	l := NewLRU(80)
	l.Put(1, 30, bytesN(30))
	l.Put(2, 30, bytesN(30))
	l.Get(1)                 // 1 becomes most recent
	l.Put(3, 30, bytesN(30)) // 90 > 80 -> evict 2 (least recent)
	_, ok := l.Get(1)
	require.True(t, ok, "1 evicted")
	_, ok = l.Get(2)
	require.False(t, ok, "2 should be evicted")
	_, ok = l.Get(3)
	require.True(t, ok, "3 evicted")
}

func TestLRUOversizeBlockNotCached(t *testing.T) {
	l := NewLRU(50)
	l.Put(1, 60, bytesN(60)) // > capacity
	require.Equal(t, 0, l.Len(), "oversize block cached")
	_, ok := l.Get(1)
	require.False(t, ok, "oversize block readable from cache")
	// Disabled cache.
	l2 := NewLRU(-1)
	l2.Put(1, 10, bytesN(10))
	_, ok = l2.Get(1)
	require.False(t, ok, "disabled cache returned entry")
}

func TestLRUUpdateExisting(t *testing.T) {
	l := NewLRU(100)
	l.Put(1, 30, bytesN(30))
	l.Put(1, 50, bytesN(50))
	require.Equal(t, 1, l.Len(), "update failed: len=%d used=%d", l.Len(), l.UsedBytes())
	require.Equal(t, uint64(50), l.UsedBytes(), "update failed: len=%d used=%d", l.Len(), l.UsedBytes())
}

func TestLRUConcurrent(t *testing.T) {
	l := NewLRU(1 << 20)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := uint64((seed*1000 + i) % 64)
				l.Put(id, int64(32+(id%16)), bytesN(int64(32+(id%16))))
				_, _ = l.Get(id)
			}
		}(g)
	}
	wg.Wait()
	require.LessOrEqual(t, l.Len(), 64, "too many entries: %d", l.Len())
}

func TestSingleflight(t *testing.T) {
	var g Group
	var count syncAtomic
	// fn blocks long enough that all 10 goroutines overlap on the same key.
	run := func() (any, error) {
		return g.Do("k", func() (any, error) {
			count.inc()
			time.Sleep(100 * time.Millisecond)
			return 42, nil
		})
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := run()
			if err != nil || v != 42 {
				assert.Fail(t, "bad result: %v %v", v, err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), count.get(), "fn ran %d times, want 1", count.get())
}

type syncAtomic struct{ v int64 }

func (a *syncAtomic) inc()       { atomic.AddInt64(&a.v, 1) }
func (a *syncAtomic) get() int64 { return atomic.LoadInt64(&a.v) }

func bytesN(n int64) []byte { return make([]byte, n) }
