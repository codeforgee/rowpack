package cache

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLRUNew(t *testing.T) {
	lru := NewLRU(1024)
	require.NotNil(t, lru)
	require.Equal(t, uint64(1024), lru.CapacityBytes())
}

func TestLRUDisabled(t *testing.T) {
	lru := NewLRU(0)
	require.NotNil(t, lru)

	// Get should always miss
	val, ok := lru.Get(1)
	require.False(t, ok)
	require.Nil(t, val)

	// Put should be no-op
	lru.Put(1, 100, "value")
	val, ok = lru.Get(1)
	require.False(t, ok)
	require.Nil(t, val)
}

func TestLRUNil(t *testing.T) {
	var lru *LRU
	// Get should always miss
	val, ok := lru.Get(1)
	require.False(t, ok)
	require.Nil(t, val)

	// Put should be no-op
	lru.Put(1, 100, "value")

	// Stats should be zero
	require.Equal(t, uint64(0), lru.CapacityBytes())
	require.Equal(t, uint64(0), lru.Hits())
	require.Equal(t, uint64(0), lru.Misses())
	require.Equal(t, uint64(0), lru.Evictions())
	require.Equal(t, uint64(0), lru.Loads())
	require.Equal(t, uint64(0), lru.Remaining())
	require.Equal(t, uint64(0), lru.UsedBytes())
	require.Equal(t, uint64(0), lru.OverheadBytes())
	require.Equal(t, 0, lru.Len())
}

func TestLRUGetPut(t *testing.T) {
	lru := NewLRU(1024)

	// Put and get
	lru.Put(1, 100, "value1")
	val, ok := lru.Get(1)
	require.True(t, ok)
	require.Equal(t, "value1", val)

	// Miss
	val, ok = lru.Get(2)
	require.False(t, ok)
	require.Nil(t, val)

	// Stats
	require.Equal(t, uint64(1), lru.Hits())
	require.Equal(t, uint64(1), lru.Misses())
	require.Equal(t, 1, lru.Len())
	require.Equal(t, uint64(100), lru.UsedBytes())
}

func TestLRUUpdateExisting(t *testing.T) {
	lru := NewLRU(1024)

	lru.Put(1, 100, "value1")
	lru.Put(1, 200, "value2") // Update with larger size

	val, ok := lru.Get(1)
	require.True(t, ok)
	require.Equal(t, "value2", val)

	// Used bytes should reflect the new size
	require.Equal(t, uint64(200), lru.UsedBytes())
	require.Equal(t, 1, lru.Len())
}

func TestLRUEviction(t *testing.T) {
	lru := NewLRU(300)

	lru.Put(1, 100, "value1")
	lru.Put(2, 100, "value2")
	lru.Put(3, 100, "value3") // This should not evict yet (300 bytes total)

	require.Equal(t, 3, lru.Len())

	lru.Put(4, 100, "value4") // This should evict LRU (key 1)

	require.Equal(t, 3, lru.Len())
	require.Equal(t, uint64(1), lru.Evictions())

	// Key 1 should be evicted
	_, ok := lru.Get(1)
	require.False(t, ok)

	// Keys 2, 3, 4 should exist
	for _, key := range []uint64{2, 3, 4} {
		val, ok := lru.Get(key)
		require.True(t, ok)
		require.Equal(t, "value"+string(rune('0'+key)), val)
	}
}

func TestLRUEvictionOrder(t *testing.T) {
	lru := NewLRU(300)

	lru.Put(1, 100, "value1")
	lru.Put(2, 100, "value2")
	lru.Put(3, 100, "value3")

	// Access key 1 to make it recently used
	lru.Get(1)

	// Add key 4, should evict key 2 (least recently used)
	lru.Put(4, 100, "value4")

	_, ok := lru.Get(1)
	require.True(t, ok)

	_, ok = lru.Get(2)
	require.False(t, ok)

	_, ok = lru.Get(3)
	require.True(t, ok)

	_, ok = lru.Get(4)
	require.True(t, ok)
}

func TestLRUValueLargerThanCapacity(t *testing.T) {
	lru := NewLRU(100)

	// Value larger than capacity should not be cached
	lru.Put(1, 200, "value1")

	_, ok := lru.Get(1)
	require.False(t, ok)
	require.Equal(t, 0, lru.Len())
	require.Equal(t, uint64(0), lru.UsedBytes())
}

func TestLRUDelete(t *testing.T) {
	lru := NewLRU(1024)

	lru.Put(1, 100, "value1")
	lru.Put(2, 100, "value2")

	require.Equal(t, 2, lru.Len())

	lru.Delete(1)

	require.Equal(t, 1, lru.Len())
	require.Equal(t, uint64(100), lru.UsedBytes())

	_, ok := lru.Get(1)
	require.False(t, ok)

	val, ok := lru.Get(2)
	require.True(t, ok)
	require.Equal(t, "value2", val)
}

func TestLRUDeleteNonExistent(t *testing.T) {
	lru := NewLRU(1024)
	lru.Put(1, 100, "value1")

	lru.Delete(999) // Non-existent key

	require.Equal(t, 1, lru.Len())
}

func TestLRURemaining(t *testing.T) {
	lru := NewLRU(1000)

	require.Equal(t, uint64(1000), lru.Remaining())

	lru.Put(1, 300, "value1")
	require.Equal(t, uint64(700), lru.Remaining())

	lru.Put(2, 400, "value2")
	require.Equal(t, uint64(300), lru.Remaining())

	// Delete frees space
	lru.Delete(1)
	require.Equal(t, uint64(600), lru.Remaining())
}

func TestLRUUsedBytes(t *testing.T) {
	lru := NewLRU(1024)

	require.Equal(t, uint64(0), lru.UsedBytes())

	lru.Put(1, 100, "value1")
	require.Equal(t, uint64(100), lru.UsedBytes())

	lru.Put(2, 200, "value2")
	require.Equal(t, uint64(300), lru.UsedBytes())

	lru.Delete(1)
	require.Equal(t, uint64(200), lru.UsedBytes())
}

func TestLRUOverheadBytes(t *testing.T) {
	lru := NewLRU(1024)

	require.Equal(t, uint64(0), lru.OverheadBytes())

	lru.Put(1, 100, "value1")
	require.Equal(t, uint64(128), lru.OverheadBytes())

	lru.Put(2, 100, "value2")
	require.Equal(t, uint64(256), lru.OverheadBytes())
}

func TestLRUConcurrent(t *testing.T) {
	lru := NewLRU(1024 * 1024) // 1MB
	var wg sync.WaitGroup

	// Multiple writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				key := uint64(start + j)
				lru.Put(key, 100, "value")
			}
		}(i * 100)
	}

	// Multiple readers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				lru.Get(uint64(j))
			}
		}()
	}

	wg.Wait()

	// Should not panic and should have some entries
	require.Greater(t, lru.Len(), 0)
	require.Greater(t, lru.UsedBytes(), uint64(0))
}

func TestLRUNoteLoad(t *testing.T) {
	lru := NewLRU(1024)

	require.Equal(t, uint64(0), lru.Loads())

	lru.NoteLoad()
	require.Equal(t, uint64(1), lru.Loads())

	lru.NoteLoad()
	require.Equal(t, uint64(2), lru.Loads())
}

func TestLRUNoteLoadNil(t *testing.T) {
	var lru *LRU
	lru.NoteLoad() // Should not panic
	require.Equal(t, uint64(0), lru.Loads())
}

func TestLRUStats(t *testing.T) {
	lru := NewLRU(1024)

	lru.Put(1, 100, "value1")
	lru.Get(1) // hit
	lru.Get(2) // miss
	lru.NoteLoad()

	require.Equal(t, uint64(1), lru.Hits())
	require.Equal(t, uint64(1), lru.Misses())
	require.Equal(t, uint64(1), lru.Loads())
	require.Equal(t, 1, lru.Len())
	require.Equal(t, uint64(100), lru.UsedBytes())
	require.Equal(t, uint64(128), lru.OverheadBytes())

	require.Equal(t, uint64(1024), lru.CapacityBytes())
	require.Equal(t, uint64(1024-100), lru.Remaining())
}

func TestLRUEvictionWithSizeChange(t *testing.T) {
	lru := NewLRU(500)

	lru.Put(1, 200, "value1")
	lru.Put(2, 200, "value2")

	// Update key 1 to larger size - should not cause immediate eviction
	// but may trigger eviction on next put
	lru.Put(1, 300, "value1_updated")

	require.Equal(t, 2, lru.Len())
	require.Equal(t, uint64(500), lru.UsedBytes()) // 300 + 200

	// Now add key 3 - should evict key 2 (LRU)
	lru.Put(3, 100, "value3")

	require.Equal(t, 2, lru.Len()) // key 2 evicted, keys 1 and 3 remain
	require.Equal(t, uint64(1), lru.Evictions())
}

func TestLRUNegativeRemaining(t *testing.T) {
	lru := NewLRU(100)

	// Put value larger than capacity
	lru.Put(1, 200, "value1")

	// Should not be cached
	require.Equal(t, 0, lru.Len())

	// Nothing cached, so remaining is the full capacity
	require.Equal(t, uint64(100), lru.Remaining())
}

func TestLRUCapacityBytes(t *testing.T) {
	lru := NewLRU(1024)
	require.Equal(t, uint64(1024), lru.CapacityBytes())

	lru2 := NewLRU(0)
	require.Equal(t, uint64(0), lru2.CapacityBytes())

	var lru3 *LRU
	require.Equal(t, uint64(0), lru3.CapacityBytes())
}

func TestLRUEvictionNoDoubleCount(t *testing.T) {
	lru := NewLRU(200)

	lru.Put(1, 100, "value1")
	lru.Put(2, 100, "value2")
	lru.Put(3, 100, "value3") // Evicts 1

	require.Equal(t, uint64(1), lru.Evictions())

	lru.Put(4, 100, "value4") // Evicts 2

	require.Equal(t, uint64(2), lru.Evictions())
}

func TestLRUGetAfterEviction(t *testing.T) {
	lru := NewLRU(200)

	lru.Put(1, 100, "value1")
	lru.Put(2, 100, "value2")
	lru.Put(3, 100, "value3") // Evicts 1

	// Try to get evicted key
	val, ok := lru.Get(1)
	require.False(t, ok)
	require.Nil(t, val)

	// Get should count as miss
	require.Equal(t, uint64(1), lru.Misses())
}

func TestLRUEmptyValue(t *testing.T) {
	lru := NewLRU(1024)

	// Put nil value
	lru.Put(1, 100, nil)

	val, ok := lru.Get(1)
	require.True(t, ok)
	require.Nil(t, val)
}

func TestLRUZeroSizeValue(t *testing.T) {
	lru := NewLRU(1024)

	lru.Put(1, 0, "value1")

	require.Equal(t, uint64(0), lru.UsedBytes())
	require.Equal(t, 1, lru.Len())

	val, ok := lru.Get(1)
	require.True(t, ok)
	require.Equal(t, "value1", val)
}

func TestSingleflightDo(t *testing.T) {
	g := &Group{}

	var count int
	fn := func() (any, error) {
		count++
		return "result", nil
	}

	val, err := g.Do("key1", fn)
	require.NoError(t, err)
	require.Equal(t, "result", val)
	require.Equal(t, 1, count)

	// Sequential calls re-execute fn (singleflight only merges concurrent calls)
	val, err = g.Do("key1", fn)
	require.NoError(t, err)
	require.Equal(t, "result", val)
	require.Equal(t, 2, count)
}

func TestSingleflightDoConcurrent(t *testing.T) {
	g := &Group{}
	var wg sync.WaitGroup
	var count int
	var mu sync.Mutex

	fn := func() (any, error) {
		mu.Lock()
		count++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return "result", nil
	}

	// Launch 10 concurrent calls for the same key
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := g.Do("key1", fn)
			require.NoError(t, err)
			require.Equal(t, "result", val)
		}()
	}

	wg.Wait()

	// fn should only be called once
	mu.Lock()
	require.Equal(t, 1, count)
	mu.Unlock()
}

func TestSingleflightDoError(t *testing.T) {
	g := &Group{}

	fn := func() (any, error) {
		return nil, fmt.Errorf("test error")
	}

	_, err := g.Do("key1", fn)
	require.Error(t, err)
	require.Contains(t, err.Error(), "test error")
}

func TestSingleflightDifferentKeys(t *testing.T) {
	g := &Group{}

	var count1, count2 int
	fn1 := func() (any, error) {
		count1++
		return "result1", nil
	}
	fn2 := func() (any, error) {
		count2++
		return "result2", nil
	}

	val, err := g.Do("key1", fn1)
	require.NoError(t, err)
	require.Equal(t, "result1", val)

	val, err = g.Do("key2", fn2)
	require.NoError(t, err)
	require.Equal(t, "result2", val)

	require.Equal(t, 1, count1)
	require.Equal(t, 1, count2)
}

func TestSingleflightDifferentKeysConcurrent(t *testing.T) {
	g := &Group{}
	var wg sync.WaitGroup
	var count1, count2 int
	var mu sync.Mutex

	fn1 := func() (any, error) {
		mu.Lock()
		count1++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return "result1", nil
	}
	fn2 := func() (any, error) {
		mu.Lock()
		count2++
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		return "result2", nil
	}

	// Launch 5 concurrent calls for key1
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := g.Do("key1", fn1)
			require.NoError(t, err)
			require.Equal(t, "result1", val)
		}()
	}

	// Launch 5 concurrent calls for key2
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := g.Do("key2", fn2)
			require.NoError(t, err)
			require.Equal(t, "result2", val)
		}()
	}

	wg.Wait()

	mu.Lock()
	require.Equal(t, 1, count1)
	require.Equal(t, 1, count2)
	mu.Unlock()
}
