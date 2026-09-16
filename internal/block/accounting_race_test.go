package block

import (
	"sync"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
)

// TestConcurrentPageMemoizeAccountingConverges: concurrent first access of
// different pages of one cached container must never leave the LRU's recorded
// size behind the container's true retained bytes. The accounting update runs
// under pagesMu, so the last applied update always carries the final size.
func TestConcurrentPageMemoizeAccountingConverges(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		// Small pages force a multi-page container.
		var rows []expectedPageRow
		var bodies [][]byte
		for i := uint64(1); i <= 12; i++ {
			rows = append(rows, expectedPageRow{rowID: i, version: 1, ct: format.ChangeInsert, bodyLen: 64})
			bodies = append(bodies, make([]byte, 64))
		}
		_, rc := buildContainer(t, 128, 1<<20, format.CompressionNone, rows, bodies)
		if rc.PageCount() < 2 {
			t.Fatal("test needs a multi-page container")
		}

		var last int64
		var mu sync.Mutex
		rc.SetCacheAccounting(func(size int64) {
			mu.Lock()
			last = size
			mu.Unlock()
		})

		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(seed int) {
				defer wg.Done()
				for k := 0; k < 200; k++ {
					pi := (seed + k) % rc.PageCount()
					p, release, err := rc.PageScratch(pi)
					if err != nil {
						t.Errorf("PageScratch(%d): %v", pi, err)
						return
					}
					release()
					_ = p
				}
			}(g)
		}
		wg.Wait()

		mu.Lock()
		got := last
		mu.Unlock()
		if got != rc.RetainedLen() {
			t.Fatalf("trial %d: accounted %d != retained %d", trial, got, rc.RetainedLen())
		}
	}
}
