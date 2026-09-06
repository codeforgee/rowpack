package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestM10CorruptSamples exercises opening deliberately corrupted stores and
// asserting the structured error types (AC-009 / AC-011). Samples are built
// from a healthy store; none of them may cause a panic.
func TestM10CorruptSamples(t *testing.T) {
	base := filepath.Join(t.TempDir(), "src")
	db, _ := buildConcurrentStore(t, base, DefaultOptions())
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	_ = w.Insert(context.Background(), 1, 7001, 1, Row{Uint64(7001), String("x")})
	if _, err := w.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	healthy, _ := os.ReadFile(base + ".rpk")
	hidx, _ := os.ReadFile(base + ".rpi")

	mkStore := func(t *testing.T, mutate func(data, idx []byte) ([]byte, []byte)) (string, error) {
		t.Helper()
		dir := t.TempDir()
		d, i := mutate(append([]byte(nil), healthy...), append([]byte(nil), hidx...))
		os.WriteFile(filepath.Join(dir, "c.rpk"), d, 0o644)
		os.WriteFile(filepath.Join(dir, "c.rpi"), i, 0o644)
		db, err := Open(filepath.Join(dir, "c"), DefaultOptions())
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
	if err == nil || !is(err, ErrVersionUnsupported) {
		t.Fatalf("unknown major: %v", err)
	}
	_ = dir

	// 2. Bad data magic -> open fails cleanly.
	if _, err := mkStore(t, func(d, i []byte) ([]byte, []byte) {
		d[0] ^= 0xFF
		return d, i
	}); err == nil {
		t.Fatal("bad data magic accepted")
	}

	// 3. Corrupt a ROWS block payload byte -> opens, Verify fails.
	dir, err = mkStore(t, func(d, i []byte) ([]byte, []byte) {
		// block 1 (metadata) header at 224; skip it to find block 2 (rows).
		rowsBlock := 224 + 64 + int(le32(d[224+44:]))
		d[rowsBlock+64+50] ^= 0xFF
		return d, i
	})
	if err != nil {
		t.Fatalf("payload corruption should open: %v", err)
	}
	db2, _ := Open(filepath.Join(dir, "c"), DefaultOptions())
	if _, err := db2.Verify(context.Background(), VerifyFull); err == nil {
		t.Fatal("payload corruption not caught by verify")
	}
	db2.Close()

	// 4. Corrupt an index entry -> index tail ignored, data authoritative.
	dir, err = mkStore(t, func(d, i []byte) ([]byte, []byte) {
		// flip a byte in the second txn's body (after first txn).
		i[128+firstTxnLen(i)+10] ^= 0xFF
		return d, i
	})
	if err != nil {
		t.Fatalf("index corruption should recover: %v", err)
	}
	db3, err := Open(filepath.Join(dir, "c"), DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db3.Close()
	snaps, _ := db3.ListSnapshots(context.Background())
	if len(snaps) != 2 {
		t.Fatalf("after index corruption, snapshots = %d, want 2", len(snaps))
	}
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

func is(err error, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
