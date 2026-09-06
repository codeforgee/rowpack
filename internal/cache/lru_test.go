package cache

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLRUBasic(t *testing.T) {
	l := NewLRU(100)
	l.Put(1, 30, bytesN(30))
	l.Put(2, 30, bytesN(30))
	if l.UsedBytes() != 60 {
		t.Fatalf("used = %d", l.UsedBytes())
	}
	if _, ok := l.Get(1); !ok {
		t.Fatal("miss on cached")
	}
	if l.Hits() != 1 || l.Misses() != 0 {
		t.Fatalf("hits/misses = %d/%d", l.Hits(), l.Misses())
	}
	if _, ok := l.Get(99); ok {
		t.Fatal("hit on uncached")
	}
	if l.Misses() != 1 {
		t.Fatalf("misses = %d", l.Misses())
	}
}

func TestLRUEviction(t *testing.T) {
	l := NewLRU(100)
	l.Put(1, 40, bytesN(40))
	l.Put(2, 40, bytesN(40))
	l.Put(3, 40, bytesN(40)) // 120 > 100 -> evict 1
	if l.UsedBytes() != 80 {
		t.Fatalf("used = %d", l.UsedBytes())
	}
	if _, ok := l.Get(1); ok {
		t.Fatal("evicted entry still cached")
	}
	if l.Evictions() != 1 {
		t.Fatalf("evictions = %d", l.Evictions())
	}
	// Accessing 2 makes it most-recent; inserting 4 evicts 3.
	l.Get(2)
	l.Put(4, 40, bytesN(40))
	if _, ok := l.Get(3); ok {
		t.Fatal("LRU did not evict least-recently-used")
	}
	if _, ok := l.Get(2); !ok {
		t.Fatal("recently used entry evicted")
	}
}

func TestLRUUsesLRUOrder(t *testing.T) {
	l := NewLRU(80)
	l.Put(1, 30, bytesN(30))
	l.Put(2, 30, bytesN(30))
	l.Get(1)                 // 1 becomes most recent
	l.Put(3, 30, bytesN(30)) // 90 > 80 -> evict 2 (least recent)
	if _, ok := l.Get(1); !ok {
		t.Fatal("1 evicted")
	}
	if _, ok := l.Get(2); ok {
		t.Fatal("2 should be evicted")
	}
	if _, ok := l.Get(3); !ok {
		t.Fatal("3 evicted")
	}
}

func TestLRUOversizeBlockNotCached(t *testing.T) {
	l := NewLRU(50)
	l.Put(1, 60, bytesN(60)) // > capacity
	if l.Len() != 0 {
		t.Fatal("oversize block cached")
	}
	if _, ok := l.Get(1); ok {
		t.Fatal("oversize block readable from cache")
	}
	// Disabled cache.
	l2 := NewLRU(-1)
	l2.Put(1, 10, bytesN(10))
	if _, ok := l2.Get(1); ok {
		t.Fatal("disabled cache returned entry")
	}
}

func TestLRUUpdateExisting(t *testing.T) {
	l := NewLRU(100)
	l.Put(1, 30, bytesN(30))
	l.Put(1, 50, bytesN(50))
	if l.Len() != 1 || l.UsedBytes() != 50 {
		t.Fatalf("update failed: len=%d used=%d", l.Len(), l.UsedBytes())
	}
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
	if l.Len() > 64 {
		t.Fatalf("too many entries: %d", l.Len())
	}
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
				t.Errorf("bad result: %v %v", v, err)
			}
		}()
	}
	wg.Wait()
	if count.get() != 1 {
		t.Fatalf("fn ran %d times, want 1", count.get())
	}
}

type syncAtomic struct{ v int64 }

func (a *syncAtomic) inc()       { atomic.AddInt64(&a.v, 1) }
func (a *syncAtomic) get() int64 { return atomic.LoadInt64(&a.v) }

func bytesN(n int64) []byte { return make([]byte, n) }
