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
	"github.com/stretchr/testify/require"
)

// TestEncryptedVerify: full verify on an encrypted store passes when intact;
// after a single ciphertext byte is flipped, both quick and full verify fail
// with ErrAuthFailed (the loader reads/authenticates every block in both
// modes; quick skips only the per-row decoding).
func TestEncryptedVerify(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc-verify")
	keyID := "vk"
	enc := func() Options { return encOptions(keyID) }
	db, _ := buildConcurrentStore(t, base, enc())
	_, err := db.Verify(context.Background(), VerifyQuick)
	require.NoError(t, err, "quick verify: %v", err)
	_, err = db.Verify(context.Background(), VerifyFull)
	require.NoError(t, err, "full verify: %v", err)
	db.Close()

	tamperFirstRowsPayload(t, base+".rpk")

	db2, err := Open(base, enc())
	require.NoError(t, err)
	defer db2.Close()
	for _, mode := range []VerifyMode{VerifyQuick, VerifyFull} {
		_, err := db2.Verify(context.Background(), mode)
		require.Error(t, err, "%v missed ciphertext tampering", mode)
		require.ErrorIs(t, err, ErrAuthFailed, "%v err = %v, want ErrAuthFailed on the chain", mode, err)
	}
}

// TestEncryptedRecoveryFromTruncatedTail truncates the single file inside
// the last snapshot transaction: without a complete SnapshotFooter the last
// snapshot is an uncommitted tail, so read-write Open truncates it away. The
// crypto contract is unchanged: an encrypted store opens only with its key.
func TestEncryptedRecoveryFromTruncatedTail(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc-recover")
	keyID := "ck"
	enc := func() Options { return encOptions(keyID) }
	db, fullID := buildConcurrentStore(t, base, enc())
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: fullID})
	require.NoError(t, w.Insert(context.Background(), 1, 9999, 1, Row{Uint64(9999), String("x")}))
	_, err := w.Commit(context.Background())
	require.NoError(t, err)
	db.Close()

	// Truncate mid-last-txn: the 2nd snapshot loses its footer.
	fi, err := os.Stat(base + ".rpk")
	require.NoError(t, err)
	require.NoError(t, os.Truncate(base+".rpk", fi.Size()-40))

	// Opening without the key fails at the key contract, before recovery.
	_, err = Open(base, Options{})
	require.ErrorIs(t, err, ErrKeyRequired, "open without key = %v, want ErrKeyRequired", err)

	db2, err := Open(base, enc())
	require.NoError(t, err, "open with key (auto recovery): %v", err)
	defer db2.Close()
	snaps, err := db2.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Len(t, snaps, 1, "snapshots = %d, want 1 (truncated tail dropped)", len(snaps))
	_, err = db2.Get(context.Background(), snaps[0].ID, 1, 9999, nil)
	require.Error(t, err, "row 9999 must be gone with the uncommitted tail: %v", err)
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
	require.NoError(t, err)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	if err := w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString},
	}}); err != nil {
		require.NoError(t, err)
	}
	for i := uint64(1); i <= 100; i++ {
		require.NoError(t, w.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("c-%d", i))}))
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
		{"commit.header.before", 0}, // nothing written
		{"commit.block.before", 0},  // header only, no footer
		{"commit.txn.before", 0},    // header+blocks, no footer
		{"commit.footer.after", 1},  // fully committed (footer + txn)
		{"commit.sync.after", 1},    // committed and synced
		{"commit.publish.after", 1}, // committed and published
	}
	for _, c := range cases {
		t.Run(c.point, func(t *testing.T) {
			base := filepath.Join(tmpdb(t), "estore")
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
			require.NoError(t, err, "read-only reopen: %v", err)
			ro.Close()
			after := fileSizes(t, base)
			for _, ext := range []string{".rpk", ".lock"} {
				require.Equal(t, before[ext], after[ext], "read-only open modified %s: %d -> %d", ext, before[ext], after[ext])
			}
		})
	}
}

func verifyEncryptedCrashRecovery(t *testing.T, base string, wantSnapshots int, opts Options) *Store {
	t.Helper()
	db, err := Open(base, opts)
	if err != nil {
		require.Fail(t, "reopen: %v", err)
	}
	snaps, err := db.ListSnapshots(context.Background())
	require.NoError(t, err)
	if len(snaps) != wantSnapshots {
		db.Close()
		require.Fail(t, "snapshots = %d, want %d", len(snaps), wantSnapshots)
	}
	for _, sn := range snaps {
		for i := uint64(1); i <= 100; i++ {
			r, err := db.Get(context.Background(), sn.ID, 1, i, nil)
			if err != nil {
				db.Close()
				require.Fail(t, "snapshot %d row %d: %v", sn.ID, i, err)
			}
			if v, _ := r[0].Uint64(); v != i {
				db.Close()
				require.Fail(t, "snapshot %d row %d id = %d", sn.ID, i, v)
			}
		}
	}
	return db
}
func tamperFirstRowsPayload(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	for {
		var bh fileformat.BlockHeader
		var bhBuf [fileformat.BlockHeaderSize]byte
		if _, err := f.ReadAt(bhBuf[:], off); err != nil {
			require.NoError(t, err)
		}
		require.NoError(t, bh.Unmarshal(bhBuf[:]))
		if bh.BlockKind == fileformat.BlockKindRows {
			flip := off + fileformat.BlockHeaderSize + int64(bh.StoredSize)/2
			b := make([]byte, 1)
			if _, err := f.ReadAt(b, flip); err != nil {
				require.NoError(t, err)
			}
			b[0] ^= 0xFF
			if _, err := f.WriteAt(b, flip); err != nil {
				require.NoError(t, err)
			}
			return
		}
		if string(bhBuf[0:8]) == "RPKSNAPF" {
			require.Fail(t, "no rows block found")
		}
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
}
