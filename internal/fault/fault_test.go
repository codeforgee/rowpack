package fault

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInjectAndCheck(t *testing.T) {
	defer Clear()
	var count atomic.Int32
	Inject("commit.beforeflush", func() { count.Add(1) })
	Inject("commit.beforeflush", func() { count.Add(2) })

	Check("commit.beforeflush")
	require.Equal(t, int32(3), count.Load(), "actions not run in order: got %d, want 3", count.Load())

	Check("commit.beforeflush")
	require.Equal(t, int32(6), count.Load(), "actions should run every check: got %d, want 6", count.Load())
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
	require.Equal(t, int32(0), count.Load(), "Clear did not remove actions: got %d, want 0", count.Load())

	Inject("clear.point", func() { count.Add(1) })
	Check("clear.point")
	require.Equal(t, int32(1), count.Load(), "reinjection after Clear failed: got %d, want 1", count.Load())
}
