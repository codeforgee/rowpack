package cache

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// cache_arms_test.go 覆盖缓存的三条兜底臂:一个条目在缓存里长大到超过整个容量(不能再
// 按预算记账,只能剔除)、nil 缓存上的操作必须安静地什么也不做、以及合并并发加载时
// 领跑者 panic 必须同样抛给等待者(否则等待者会拿到一个永远不返回的结果)。
//
// Remaining 的 free < 0 分支(145)到不了:Put 之后总有 evictLocked 把 used 收回容量之内,
// 而超过整个容量的条目根本不入缓存,所以 used 永远不大于 capacity。

// TestPutDropsEntryThatGrewPastCapacity: a cached value may grow after
// insertion. Once it outgrows the whole cache it cannot be accounted for, so
// the entry is dropped instead of staying resident under-charged.
func TestPutDropsEntryThatGrewPastCapacity(t *testing.T) {
	c := NewLRU(100)
	c.Put(1, 40, "small")
	require.Equal(t, 1, c.Len())
	require.Equal(t, uint64(40), c.UsedBytes())

	c.Put(1, 500, "grown")
	require.Equal(t, 0, c.Len(), "an entry bigger than the cache is not cached")
	require.Equal(t, uint64(0), c.UsedBytes(), "no byte budget is left charged")
	require.Equal(t, uint64(1), c.Evictions())

	_, ok := c.Get(1)
	require.False(t, ok, "the oversized entry is gone, not served")
}

// TestNilCacheOperationsAreInert: every store carries caches, but a disabled
// cache is a nil *LRU. Operations on it must do nothing and report empty
// numbers instead of panicking.
func TestNilCacheOperationsAreInert(t *testing.T) {
	var c *LRU

	c.Delete(7)
	require.Equal(t, uint64(0), c.Remaining(), "a disabled cache has no free budget")
	require.Equal(t, uint64(0), c.CapacityBytes())
	require.Equal(t, 0, c.Len())
}

// TestDoPropagatesPanicToWaiters: callers merged into one in-flight load must
// see the leader's panic, not wait forever or receive a zero result. The call
// is dropped, so the next caller may retry.
func TestDoPropagatesPanicToWaiters(t *testing.T) {
	var g Group
	inFlight := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // leader
		defer wg.Done()
		defer func() { _ = recover() }()
		_, _ = g.Do("block", func() (any, error) {
			close(inFlight)
			<-release
			panic("boom")
		})
	}()
	<-inFlight

	go func() { // waiter joins the in-flight call
		defer wg.Done()
		defer func() {
			if r := recover(); r == nil {
				t.Error("the waiter must see the leader's panic")
			}
		}()
		_, _ = g.Do("block", func() (any, error) { return nil, errors.New("must not run") })
	}()
	// Let the waiter reach the wait before the leader panics.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	v, err := g.Do("block", func() (any, error) { return "retried", nil })
	require.NoError(t, err, "the failed call leaves nothing behind: a later caller retries")
	require.Equal(t, "retried", v)
}
