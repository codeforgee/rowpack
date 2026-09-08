package rowpack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// TestM10CorruptSamples exercises opening deliberately corrupted stores and
// asserting the structured error types (AC-009 / AC-011). Samples are built
// from a healthy store; none of them may cause a panic.
func TestM10CorruptSamples(t *testing.T) {
	base := filepath.Join(tmpdb(t), "src")
	db, _ := buildConcurrentStore(t, base, Options{})
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	_ = w.Insert(context.Background(), 1, 7001, 1, Row{Uint64(7001), String("x")})
	_, err := w.Commit(context.Background())
	require.NoError(t, err)
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

	// 3. Corrupt a ROWS block payload byte -> opens, Verify fails.
	dir, err = mkStore(t, func(d []byte) []byte {
		// block 1 (metadata) header at 224; skip it to find block 2 (rows).
		rowsBlock := 224 + 64 + int(le32(d[224+44:]))
		d[rowsBlock+64+50] ^= 0xFF
		return d
	})
	require.NoError(t, err, "payload corruption should open: %v", err)
	db2, _ := Open(filepath.Join(dir, "c"), Options{})
	_, err = db2.Verify(context.Background(), VerifyFull)
	require.Error(t, err, "payload corruption not caught by verify")
	db2.Close()

	// 4. Corrupt an IndexTxn body byte -> the snapshot's txn is rebuilt in
	// memory from its blocks (BINARY_FORMAT_V2 §10.2); data stays
	// authoritative.
	dir, err = mkStore(t, func(d []byte) []byte {
		first := bytes.Index(d, []byte(fileformat.MagicIndexTxnHdr))
		require.Greater(t, first, 0, "no IndexTxnHeader found")
		body := first + fileformat.IndexTxnHeaderSize + 10
		d[body] ^= 0xFF
		return d
	})
	require.NoError(t, err, "index corruption should recover: %v", err)
	db3, err := Open(filepath.Join(dir, "c"), Options{})
	require.NoError(t, err)
	defer db3.Close()
	require.Equal(t, uint64(1), db3.Stats().Recovery.SnapshotsRebuilt, "SnapshotsRebuilt = %d, want 1", db3.Stats().Recovery.SnapshotsRebuilt)
	snaps, _ := db3.ListSnapshots(context.Background())
	require.Len(t, snaps, 2, "after index corruption, snapshots = %d, want 2", len(snaps))
}
