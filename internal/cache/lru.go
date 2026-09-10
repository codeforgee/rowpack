// Package cache implements the concurrent, byte-capacity-bounded LRU used for
// decompressed RowPack blocks. It is safe for concurrent use; eviction never
// affects an in-flight reader because cached values are immutable bytes that
// the reader copies before returning.
package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// LRU is a thread-safe LRU bounded by total value bytes. Keys are BlockIDs
// (the cache is per-store, so Store identity is implicit). A value larger than
// the whole capacity is not cached.
type LRU struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	items    map[uint64]*list.Element
	ll       *list.List

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
	loads     atomic.Uint64
}

type lruEntry struct {
	key   uint64
	size  int64
	value any
}

// NewLRU creates an LRU with the given byte capacity. A capacity <= 0 means
// caching is disabled (Get always misses, Put is a no-op).
func NewLRU(capacityBytes int64) *LRU {
	return &LRU{
		capacity: capacityBytes,
		items:    make(map[uint64]*list.Element),
		ll:       list.New(),
	}
}

// Get returns the cached value for key.
func (c *LRU) Get(key uint64) (any, bool) {
	if c == nil {
		return nil, false
	}
	if c.capacity <= 0 {
		c.misses.Add(1)
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		c.hits.Add(1)
		return el.Value.(*lruEntry).value, true
	}
	c.misses.Add(1)
	return nil, false
}

// Put caches value with the given byte size, evicting least-recently-used
// entries to stay within capacity. A value larger than capacity is not cached.
func (c *LRU) Put(key uint64, size int64, value any) {
	if c == nil || c.capacity <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.capacity {
		// A cached value may grow after insertion (RowsContainer gains decoded
		// pages). Remove the old entry instead of leaving an under-accounted
		// value resident.
		if el, ok := c.items[key]; ok {
			e := el.Value.(*lruEntry)
			c.ll.Remove(el)
			delete(c.items, key)
			c.used -= e.size
			c.evictions.Add(1)
		}
		return
	}
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		c.used += size - el.Value.(*lruEntry).size
		el.Value.(*lruEntry).size = size
		el.Value.(*lruEntry).value = value
		c.evictLocked()
		return
	}
	el := c.ll.PushFront(&lruEntry{key: key, size: size, value: value})
	c.items[key] = el
	c.used += size
	c.evictLocked()
}

func (c *LRU) evictLocked() {
	for c.used > c.capacity && c.ll.Len() > 0 {
		back := c.ll.Back()
		e := back.Value.(*lruEntry)
		c.ll.Remove(back)
		delete(c.items, e.key)
		c.used -= e.size
		c.evictions.Add(1)
	}
}

// Delete removes a key from the cache.
func (c *LRU) Delete(key uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*lruEntry)
		c.ll.Remove(el)
		delete(c.items, key)
		c.used -= e.size
	}
}

// CapacityBytes returns the configured capacity.
func (c *LRU) CapacityBytes() uint64 {
	if c == nil {
		return 0
	}
	return uint64(c.capacity)
}

// Remaining returns the free byte budget, or 0 when full or nil. Callers use
// it to decide whether promoting another entry is worthwhile without
// allocating the entry first.
func (c *LRU) Remaining() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	free := c.capacity - c.used
	if free < 0 {
		return 0
	}
	return uint64(free)
}

// UsedBytes returns the currently used bytes.
func (c *LRU) UsedBytes() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return uint64(c.used)
}

// Hits returns the number of cache hits.
func (c *LRU) Hits() uint64 {
	if c == nil {
		return 0
	}
	return c.hits.Load()
}

// Misses returns the number of cache misses.
func (c *LRU) Misses() uint64 {
	if c == nil {
		return 0
	}
	return c.misses.Load()
}

// Evictions returns the number of evictions.
func (c *LRU) Evictions() uint64 {
	if c == nil {
		return 0
	}
	return c.evictions.Load()
}

// Loads returns the number of underlying block loads (used for miss-merge
// accounting by the caller).
func (c *LRU) Loads() uint64 {
	if c == nil {
		return 0
	}
	return c.loads.Load()
}

// NoteLoad increments the loads counter.
func (c *LRU) NoteLoad() {
	if c != nil {
		c.loads.Add(1)
	}
}

// Len returns the number of cached entries (for tests).
func (c *LRU) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// overheadPerEntry estimates the management memory of one cached entry: the
// map slot (~16 B with bucket sharing), the list.Element (~56 B), the
// lruEntry struct (~32 B) and GC pointer overhead. Reported separately in
// Stats so CacheBytes stays a value-bytes budget while total resident cost
// remains visible (GO_API_DESIGN_V1.md §2).
const overheadPerEntry = 128

// OverheadBytes returns the estimated management memory (map/list nodes),
// distinct from the value bytes counted against the capacity.
func (c *LRU) OverheadBytes() uint64 {
	if c == nil {
		return 0
	}
	return uint64(c.Len() * overheadPerEntry)
}
