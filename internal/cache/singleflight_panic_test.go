package cache

import (
	"testing"
	"time"
)

func TestGroupPanicReleasesKey(t *testing.T) {
	var g Group
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Do did not propagate panic")
			}
		}()
		_, _ = g.Do(1, func() (any, error) { panic("boom") })
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err := g.Do(1, func() (any, error) { return "ok", nil })
		if err != nil || v != "ok" {
			t.Errorf("retry = (%v, %v), want (ok, nil)", v, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("key remained blocked after panic")
	}
}
