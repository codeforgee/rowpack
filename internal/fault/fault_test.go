package fault

import (
	"sync/atomic"
	"testing"
)

func TestInjectAndCheck(t *testing.T) {
	defer Clear()
	var count atomic.Int32
	Inject("commit.beforeflush", func() { count.Add(1) })
	Inject("commit.beforeflush", func() { count.Add(2) })

	Check("commit.beforeflush")
	if got := count.Load(); got != 3 {
		t.Fatalf("actions not run in order: got %d, want 3", got)
	}

	Check("commit.beforeflush")
	if got := count.Load(); got != 6 {
		t.Fatalf("actions should run every check: got %d, want 6", got)
	}
}

func TestCheckNoopWithoutInjection(t *testing.T) {
	defer Clear()
	Check("nonexistent.point")
	Check("")
}

func TestClearRemovesInjections(t *testing.T) {
	var count atomic.Int32
	Inject("clear.point", func() { count.Add(1) })
	Clear()

	Check("clear.point")
	if got := count.Load(); got != 0 {
		t.Fatalf("Clear did not remove actions: got %d, want 0", got)
	}

	Inject("clear.point", func() { count.Add(1) })
	Check("clear.point")
	if got := count.Load(); got != 1 {
		t.Fatalf("reinjection after Clear failed: got %d, want 1", got)
	}
}
