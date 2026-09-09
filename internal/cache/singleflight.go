package cache

import (
	"sync"
)

// Group merges concurrent calls for the same key into a single execution,
// so N goroutines cold-reading the same block trigger one load (singleflight).
type Group struct {
	mu sync.Mutex
	m  map[any]*call
}

type call struct {
	wg  sync.WaitGroup
	val any
	err error
}

// Do runs fn once for key while concurrent callers wait for the same result.
func (g *Group) Do(key any, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[any]*call)
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.val, c.err
}
