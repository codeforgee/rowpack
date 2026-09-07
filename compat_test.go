package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	hidx, _ := os.ReadFile(base + ".rpi")

	mkStore := func(t *testing.T, mutate func(data, idx []byte) ([]byte, []byte)) (string, error) {
		t.Helper()
		dir := tmpdb(t)
		d, i := mutate(append([]byte(nil), healthy...), append([]byte(nil), hidx...))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "c.rpk"), d, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "c.rpi"), i, 0o644))
		db, err := Open(filepath.Join(dir, "c"), Options{})
		if err != nil {
			return "", err
		}
		db.Close()
		return dir, nil
	}

	// 1. Unknown major version -> ErrVersionUnsupported, file unmodified.
	dir, err := mkStore(t, func(d, i []byte) ([]byte, []byte) {
		d[8] = 99 // VersionMajor
		return d, i
	})
	require.ErrorIs(t, err, ErrVersionUnsupported, "unknown major: %v", err)
	_ = dir

	// 2. Bad data magic -> open fails cleanly.
	_, err = mkStore(t, func(d, i []byte) ([]byte, []byte) {
		d[0] ^= 0xFF
		return d, i
	})
	require.Error(t, err, "bad data magic accepted")

	// 3. Corrupt a ROWS block payload byte -> opens, Verify fails.
	dir, err = mkStore(t, func(d, i []byte) ([]byte, []byte) {
		// block 1 (metadata) header at 224; skip it to find block 2 (rows).
		rowsBlock := 224 + 64 + int(le32(d[224+44:]))
		d[rowsBlock+64+50] ^= 0xFF
		return d, i
	})
	require.NoError(t, err, "payload corruption should open: %v", err)
	db2, _ := Open(filepath.Join(dir, "c"), Options{})
	_, err = db2.Verify(context.Background(), VerifyFull)
	require.Error(t, err, "payload corruption not caught by verify")
	db2.Close()

	// 4. Corrupt an index entry -> index tail ignored, data authoritative.
	dir, err = mkStore(t, func(d, i []byte) ([]byte, []byte) {
		// flip a byte in the second txn's body (after first txn).
		i[128+firstTxnLen(i)+10] ^= 0xFF
		return d, i
	})
	require.NoError(t, err, "index corruption should recover: %v", err)
	db3, err := Open(filepath.Join(dir, "c"), Options{})
	require.NoError(t, err)
	defer db3.Close()
	snaps, _ := db3.ListSnapshots(context.Background())
	require.Len(t, snaps, 2, "after index corruption, snapshots = %d, want 2", len(snaps))
}

// firstTxnLen walks the index to find the length of the first transaction.
func firstTxnLen(idx []byte) int {
	// Header is 128; read IndexTxnHeader at 128.
	if len(idx) < 128+80 {
		return 0
	}
	body := int(le32(idx[128+64:]))
	return 80 + body + 80
}
