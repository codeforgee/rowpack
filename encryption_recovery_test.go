package rowpack

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fault"
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

// TestEncryptedCrashChildHelper is the crash child for encrypted stores: it
// creates an encrypted store, writes a FULL snapshot of 100 rows and commits;
// the requested fault exits the process mid-commit.
func TestEncryptedCrashChildHelper(t *testing.T) {
	base := os.Getenv("ROWCRASH_BASE")
	if base == "" {
		t.Skip("not a crash child")
	}
	fp := os.Getenv("ROWCRASH_FAULT")
	fault.Inject(fp, func() { os.Exit(0) })
	t.Cleanup(fault.Clear)

	keyID := "cck"
	db, err := Create(base, Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
		KeyID:       keyID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString},
	}}); err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 100; i++ {
		if err := w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("c-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = w.Commit(context.Background())
	_ = db.Close()
}

// TestEncryptedCrashFaultPoints crashes an encrypted store at every injection
// point and verifies the recovery outcome is deterministic, idempotent and
// never modifies files on a read-only reopen.
func TestEncryptedCrashFaultPoints(t *testing.T) {
	cases := []struct {
		point         string
		wantSnapshots int
	}{
		{"commit.data-header.before", 0},
		{"commit.block.before", 0},
		{"commit.data-footer.after", 1},
		{"commit.data-sync.after", 1},
		{"commit.index.before", 1},
		{"commit.index-sync.before", 1},
		{"commit.publish.after", 1},
	}
	for _, c := range cases {
		t.Run(c.point, func(t *testing.T) {
			base := filepath.Join(t.TempDir(), "estore")
			cmd := exec.Command(os.Args[0], "-test.run=TestEncryptedCrashChildHelper")
			cmd.Env = append(os.Environ(), "ROWCRASH_BASE="+base, "ROWCRASH_FAULT="+c.point)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Logf("crash child output: %s", out)
			}

			keyID := "cck"
			enc := func() Options {
				return Options{Encryption: &EncryptionConfig{
					KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
					KeyID:       keyID,
				}}
			}
			// First reopen: recovery happens.
			db := verifyEncryptedCrashRecovery(t, base, c.wantSnapshots, enc())
			db.Close()
			// Second reopen: idempotent.
			db2 := verifyEncryptedCrashRecovery(t, base, c.wantSnapshots, enc())
			db2.Close()
			// Read-only reopen with key: never modifies files.
			before := fileSizes(t, base)
			ro, err := Open(base, enc())
			if err != nil {
				t.Fatalf("read-only reopen: %v", err)
			}
			ro.Close()
			after := fileSizes(t, base)
			for _, ext := range []string{".rpk", ".rpi"} {
				if before[ext] != after[ext] {
					t.Fatalf("read-only open modified %s: %d -> %d", ext, before[ext], after[ext])
				}
			}
		})
	}
}

func verifyEncryptedCrashRecovery(t *testing.T, base string, wantSnapshots int, opts Options) *Store {
	t.Helper()
	db, err := Open(base, opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	snaps, err := db.ListSnapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != wantSnapshots {
		db.Close()
		t.Fatalf("snapshots = %d, want %d", len(snaps), wantSnapshots)
	}
	for _, sn := range snaps {
		for i := uint64(1); i <= 100; i++ {
			r, err := db.Get(context.Background(), sn.ID, 1, i, nil)
			if err != nil {
				db.Close()
				t.Fatalf("snapshot %d row %d: %v", sn.ID, i, err)
			}
			if v, _ := r[0].Uint64(); v != i {
				db.Close()
				t.Fatalf("snapshot %d row %d id = %d", sn.ID, i, v)
			}
		}
	}
	return db
}
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