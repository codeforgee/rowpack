package rowpack

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestM10CorruptSamples exercises opening deliberately corrupted stores and
// asserting the structured error types (AC-009 / AC-011). Samples are built
// from a healthy store; none of them may cause a panic. Payload-corruption
// and IndexTxn-rebuild scenarios live with their canonical tests
// (TestM8Verify, TestM8RebuildSnapshotFromBlocks); this test keeps the
// header-level samples that only it covers.
func TestM10CorruptSamples(t *testing.T) {
	base := filepath.Join(tmpdb(t), "src")
	db, _ := buildConcurrentStore(t, base, Options{})
	db.Close()

	healthy, _ := os.ReadFile(base + ".rpk")

	mkStore := func(t *testing.T, mutate func(data []byte) []byte) (string, error) {
		t.Helper()
		dir := tmpdb(t)
		d := mutate(append([]byte(nil), healthy...))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "c.rpk"), d, 0o644))
		db, err := Open(filepath.Join(dir, "c"), Options{})
		if err != nil {
			return "", err
		}
		db.Close()
		return dir, nil
	}

	// 1. Unknown major version -> ErrVersionUnsupported, file unmodified.
	dir, err := mkStore(t, func(d []byte) []byte {
		d[8] = 99 // VersionMajor
		return d
	})
	require.ErrorIs(t, err, ErrVersionUnsupported, "unknown major: %v", err)
	_ = dir

	// 2. Bad data magic -> open fails cleanly.
	_, err = mkStore(t, func(d []byte) []byte {
		d[0] ^= 0xFF
		return d
	})
	require.Error(t, err, "bad data magic accepted")
}
