package rowpack

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCorruptionError(t *testing.T) {
	kind := errors.New("rowpack: bad magic")
	e := &CorruptionError{
		File: "s.rpk", Offset: 4096, SnapshotID: 7, TableID: 2, BlockID: 11,
		Kind: kind, Reason: "magic mismatch",
	}
	msg := e.Error()
	for _, want := range []string{"s.rpk", "4096", "snapshot=7", "table=2", "block=11", "magic mismatch"} {
		require.Contains(t, msg, want)
	}
	require.ErrorIs(t, e, kind, "Unwrap should expose Kind")
}

func TestNewCorruption(t *testing.T) {
	e := newCorruption("s.rpi", 128, 3, 1, 5, "crc mismatch")
	require.Equal(t, ErrCorruptData, e.Kind)
	require.ErrorIs(t, e, ErrCorruptData)
	require.Equal(t, "s.rpi", e.File)
	require.Equal(t, int64(128), e.Offset)
	require.Equal(t, SnapshotID(3), e.SnapshotID)
	require.Equal(t, TableID(1), e.TableID)
	require.Equal(t, uint64(5), e.BlockID)
}

func TestCommitError(t *testing.T) {
	inner := errors.New("disk full")
	unknown := &CommitError{SnapshotID: 9, Unknown: true, Err: inner}
	require.Contains(t, unknown.Error(), "outcome unknown")
	require.Contains(t, unknown.Error(), "9")
	known := &CommitError{SnapshotID: 9, Err: inner}
	require.NotContains(t, known.Error(), "outcome unknown")
	require.ErrorIs(t, unknown, inner, "Unwrap should expose Err")
	require.ErrorIs(t, known, inner, "Unwrap should expose Err")
}
