package rowpack

import (
	"errors"
	"strings"
	"testing"
)

func TestCorruptionError(t *testing.T) {
	kind := errors.New("rowpack: bad magic")
	e := &CorruptionError{
		File: "s.rpk", Offset: 4096, SnapshotID: 7, TableID: 2, BlockID: 11,
		Kind: kind, Reason: "magic mismatch",
	}
	msg := e.Error()
	for _, want := range []string{"s.rpk", "4096", "snapshot=7", "table=2", "block=11", "magic mismatch"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Error() missing %q: %s", want, msg)
		}
	}
	if !errors.Is(e, kind) {
		t.Fatal("Unwrap should expose Kind")
	}
}

func TestNewCorruption(t *testing.T) {
	e := newCorruption("s.rpi", 128, 3, 1, 5, "crc mismatch")
	if e.Kind != ErrCorruptData {
		t.Fatalf("Kind = %v", e.Kind)
	}
	if !errors.Is(e, ErrCorruptData) {
		t.Fatal("errors.Is(e, ErrCorruptData) failed")
	}
	if e.File != "s.rpi" || e.Offset != 128 || e.SnapshotID != 3 || e.TableID != 1 || e.BlockID != 5 {
		t.Fatalf("fields wrong: %+v", e)
	}
}

func TestCommitError(t *testing.T) {
	inner := errors.New("disk full")
	unknown := &CommitError{SnapshotID: 9, Unknown: true, Err: inner}
	if !strings.Contains(unknown.Error(), "outcome unknown") || !strings.Contains(unknown.Error(), "9") {
		t.Fatalf("unknown Error() = %s", unknown.Error())
	}
	known := &CommitError{SnapshotID: 9, Err: inner}
	if strings.Contains(known.Error(), "outcome unknown") {
		t.Fatalf("known Error() = %s", known.Error())
	}
	if !errors.Is(unknown, inner) || !errors.Is(known, inner) {
		t.Fatal("Unwrap should expose Err")
	}
}
