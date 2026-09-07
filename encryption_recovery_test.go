package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// TestEncryptedRebuildIndex: explicit RebuildIndex requires the key and, with
// it, re-derives the full index from sealed blocks.
func TestEncryptedRebuildIndex(t *testing.T) {
	base := filepath.Join(t.TempDir(), "enc-rebuild")
	keyID := "rk"
	enc := func() Options { return encOptions(keyID) }
	db, _ := buildConcurrentStore(t, base, enc())
	db.Close()
	if err := os.Remove(base + ".rpi"); err != nil {
		t.Fatal(err)
	}

	// Without a key: reject.
	if err := RebuildIndex(context.Background(), base, RebuildOptions{Durability: SyncCommit}); !isErr(err, ErrKeyRequired) {
		t.Fatalf("rebuild without key = %v, want ErrKeyRequired", err)
	}
	// With the key: succeeds.
	if err := RebuildIndex(context.Background(), base, RebuildOptions{
		Durability: SyncCommit,
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
			KeyID:       keyID,
		},
	}); err != nil {
		t.Fatalf("rebuild with key: %v", err)
	}

	db2, err := Open(base, enc())
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	row, err := db2.Get(context.Background(), 1, 1, 42, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := row[1].String(); v != "n-42" {
		t.Fatalf("row 42 = %q, want n-42", v)
	}
}

// TestEncryptedVerify: full verify on an encrypted store passes when intact;
// after a single ciphertext byte is flipped, both quick and full verify fail
// with ErrAuthFailed (the loader reads/authenticates every block in both
// modes; quick skips only the per-row decoding).
func TestEncryptedVerify(t *testing.T) {
	base := filepath.Join(t.TempDir(), "enc-verify")
	keyID := "vk"
	enc := func() Options { return encOptions(keyID) }
	db, _ := buildConcurrentStore(t, base, enc())
	if _, err := db.Verify(context.Background(), VerifyQuick); err != nil {
		t.Fatalf("quick verify: %v", err)
	}
	if _, err := db.Verify(context.Background(), VerifyFull); err != nil {
		t.Fatalf("full verify: %v", err)
	}
	db.Close()

	tamperFirstRowsPayload(t, base+".rpk")

	db2, err := Open(base, enc())
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	for _, mode := range []VerifyMode{VerifyQuick, VerifyFull} {
		_, err := db2.Verify(context.Background(), mode)
		if err == nil {
			t.Fatalf("%v missed ciphertext tampering", mode)
		}
		if !isErr(err, ErrAuthFailed) {
			t.Fatalf("%v err = %v, want ErrAuthFailed on the chain", mode, err)
		}
	}
}

// TestEncryptedRecoveryFromTruncatedIndex simulates a truncated .rpi tail
// (data ahead of index): read-write Open rebuilds the tail from sealed blocks
// — recovery.buildIndexTxnFromData authenticates and decrypts every block —
// so the store recovers only when opened with its key.
func TestEncryptedRecoveryFromTruncatedIndex(t *testing.T) {
	base := filepath.Join(t.TempDir(), "enc-recover")
	keyID := "ck"
	enc := func() Options { return encOptions(keyID) }
	db, fullID := buildConcurrentStore(t, base, enc())
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: fullID})
	if err := w.Insert(context.Background(), 1, 9999, 1, Row{Uint64(9999), String("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Truncate the index mid-second-txn.
	idxPath := base + ".rpi"
	fi, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(idxPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(fi.Size() - 40); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Opening without the key fails at the key contract, before recovery.
	if _, err := Open(base, Options{}); !isErr(err, ErrKeyRequired) {
		t.Fatalf("open without key = %v, want ErrKeyRequired", err)
	}

	db2, err := Open(base, enc())
	if err != nil {
		t.Fatalf("open with key (auto recovery): %v", err)
	}
	defer db2.Close()
	snaps, err := db2.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d, want 2 (index tail rebuilt from sealed data)", len(snaps))
	}
	if r, err := db2.Get(context.Background(), snaps[1].ID, 1, 9999, nil); err != nil {
		t.Fatalf("row 9999 after recovery: %v", err)
	} else if v, _ := r[1].String(); v != "x" {
		t.Fatalf("row 9999 = %q", v)
	}
}

// tamperFirstRowsPayload flips one byte in the middle of the first Rows
// block's payload in the .rpk file.
func tamperFirstRowsPayload(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	for {
		var bh fileformat.BlockHeader
		var bhBuf [fileformat.BlockHeaderSize]byte
		if _, err := f.ReadAt(bhBuf[:], off); err != nil {
			t.Fatal(err)
		}
		if err := bh.Unmarshal(bhBuf[:]); err != nil {
			t.Fatal(err)
		}
		if bh.BlockKind == fileformat.BlockKindRows {
			flip := off + fileformat.BlockHeaderSize + int64(bh.StoredSize)/2
			b := make([]byte, 1)
			if _, err := f.ReadAt(b, flip); err != nil {
				t.Fatal(err)
			}
			b[0] ^= 0xFF
			if _, err := f.WriteAt(b, flip); err != nil {
				t.Fatal(err)
			}
			return
		}
		if string(bhBuf[0:8]) == "RPKSNAPF" {
			t.Fatal("no rows block found")
		}
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
}