// Package fault provides deterministic test-only fault injection points. The
// store calls Check at named points during commit/open; tests register actions
// that simulate crashes or errors at exactly those positions.
package fault

import "sync"

var (
	mu      sync.Mutex
	actions = map[string][]func(){}
	points  = map[string]int{} // counters per point
)

// Inject registers fn to run when Check(point) is called. Multiple actions at
// one point run in order.
func Inject(point string, fn func()) {
	mu.Lock()
	defer mu.Unlock()
	actions[point] = append(actions[point], fn)
}

// Clear removes all injections and resets counters.
func Clear() {
	mu.Lock()
	defer mu.Unlock()
	actions = map[string][]func(){}
	points = map[string]int{}
}

// Check runs the injected action at point, if any, exactly once per count.
// It is a no-op when nothing is injected.
func Check(point string) {
	mu.Lock()
	defer mu.Unlock()
	actions[point] = actions[point] // ensure key exists
	for _, fn := range actions[point] {
		fn()
	}
}
