package rowpack

import (
	"bytes"
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

// crashChildOpts returns the options plain and encrypted crash scenarios use
// to build their stores (the encrypted child and its reopen share the same
// fixed key).
func crashChildOpts(enc bool) Options {
	if !enc {
		return Options{}
	}
	keyID := "cck"
	return Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
		KeyID:       keyID,
	}}
}

// runCrashChild re-invokes the test binary to create a store (plain or
// encrypted) and commit a snapshot, dying via os.Exit at the given fault
// point. The files are left in the crashed state for the caller to reopen.
func runCrashChild(t *testing.T, base string, faultPoint string, enc bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashChildHelper")
	cmd.Env = append(os.Environ(), "ROWCRASH_BASE="+base, "ROWCRASH_FAULT="+faultPoint)
	if enc {
		cmd.Env = append(cmd.Env, "ROWCRASH_ENC=1")
	}
	out, err := cmd.CombinedOutput()
	_ = out
	if err != nil {
		// The child exits non-zero by design (os.Exit(0) inside a test);
		// failures in setup also surface here.
		t.Logf("crash child output: %s", out)
	}
}

// TestCrashChildHelper is the crash simulation entry: it creates a store
// (plain, or encrypted when ROWCRASH_ENC is set), writes a FULL snapshot of
// 100 rows, and commits. A fault at the requested point terminates the
// process mid-commit.
func TestCrashChildHelper(t *testing.T) {
	base := os.Getenv("ROWCRASH_BASE")
	if base == "" {
		t.Skip("not a crash child")
	}
	fp := os.Getenv("ROWCRASH_FAULT")
	fault.Inject(fp, func() { os.Exit(0) })
	t.Cleanup(fault.Clear)

	db, err := Create(base, crashChildOpts(os.Getenv("ROWCRASH_ENC") == "1"))
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

// verifyCrashRecovery reopens the crashed store with the provided options and
// asserts the committed state (which snapshot count and row visibility are
// expected).
func verifyCrashRecovery(t *testing.T, base string, wantSnapshots int, opts Options) *Store {
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
	// The committed snapshot's rows must be readable.
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

// TestM8CrashFaultPoints crashes a store at every injection point (plain and
// encrypted) and verifies the recovery outcome is deterministic and
// idempotent (AC-008) and that the crypto contract holds for keyed stores.
func TestM8CrashFaultPoints(t *testing.T) {
	cases := []struct {
		point         string
		wantSnapshots int
	}{
		{"commit.header.before", 0}, // nothing written
		{"commit.block.before", 0},  // header only, no footer
		{"commit.txn.before", 0},    // header+blocks, no footer: uncommitted tail
		{"commit.footer.after", 1},  // fully committed (footer + txn)
		{"commit.sync.after", 1},    // committed and synced
		{"commit.publish.after", 1}, // committed and published
	}

	for _, enc := range []bool{false, true} {
		name := "plain"
		if enc {
			name = "encrypted"
		}
		opts := crashChildOpts(enc)
		t.Run(name, func(t *testing.T) {
			for _, c := range cases {
				t.Run(c.point, func(t *testing.T) {
					base := filepath.Join(tmpdb(t), "store")
					runCrashChild(t, base, c.point, enc)
					// First reopen: recovery happens.
					db := verifyCrashRecovery(t, base, c.wantSnapshots, opts)
					db.Close()
					// Second reopen: idempotent, no further changes.
					db2 := verifyCrashRecovery(t, base, c.wantSnapshots, opts)
					db2.Close()
					// Third reopen read-only: never modifies files.
					before := fileSizes(t, base)
					ro, err := Open(base, opts)
					require.NoError(t, err, "read-only reopen: %v", err)
					ro.Close()
					after := fileSizes(t, base)
					for _, ext := range []string{".rpk", ".lock"} {
						require.Equal(t, before[ext], after[ext], "read-only open modified %s: %d -> %d", ext, before[ext], after[ext])
					}
				})
			}
		})
	}
}

func fileSizes(t *testing.T, base string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, ext := range []string{".rpk", ".lock"} {
		if fi, err := os.Stat(base + ext); err == nil {
			out[ext] = fi.Size()
		}
	}
	return out
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	return fi.Size()
}

// appendFile appends raw bytes to path (store must be closed).
func appendFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.Write(raw)
	require.NoError(t, err)
}

// craftedSnapshotPair renders a committed header+footer pair for snapshot id
// with no blocks: valid to the walk, but carrying no rows.
func craftedSnapshotPair(t *testing.T, id uint64) []byte {
	t.Helper()
	var hdr [fileformat.SnapshotHeaderSize]byte
	var ftr [fileformat.SnapshotFooterSize]byte
	sh := fileformat.SnapshotHeader{SnapshotType: fileformat.SnapshotFull, SnapshotID: id}
	require.NoError(t, sh.MarshalTo(hdr[:]))
	sf := fileformat.SnapshotFooter{SnapshotType: fileformat.SnapshotFull, SnapshotID: id}
	require.NoError(t, sf.MarshalTo(ftr[:]))
	return append(hdr[:], ftr[:]...)
}

// TestM8TailTruncated simulates a crash mid-transaction by truncating the
// single file inside the last snapshot: without a complete SnapshotFooter the
// last snapshot is an uncommitted tail and is dropped on read-write Open.
// The encrypted variant additionally pins the key contract: a keyless open
// fails with ErrKeyRequired before any recovery.
func TestM8TailTruncated(t *testing.T) {
	for _, enc := range []bool{false, true} {
		name := "plain"
		opts := Options{}
		if enc {
			name = "encrypted"
			opts = encOptions("ck")
		}
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(tmpdb(t), "it")
			db, fullID := buildConcurrentStore(t, base, opts)
			// Commit a second snapshot so the file holds two txns.
			w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: fullID})
			require.NoError(t, w.Insert(context.Background(), 1, 9999, 1, Row{Uint64(9999), String("x")}))
			_, err := w.Commit(context.Background())
			require.NoError(t, err)
			db.Close()

			// Truncate mid-last-txn-footer: the 2nd snapshot loses its footer.
			fi, _ := os.Stat(base + ".rpk")
			require.NoError(t, os.Truncate(base+".rpk", fi.Size()-40))

			if enc {
				// The key contract is enforced before recovery runs.
				_, err = Open(base, Options{})
				require.ErrorIs(t, err, ErrKeyRequired, "open without key = %v, want ErrKeyRequired", err)
			}

			db2, err := Open(base, opts)
			require.NoError(t, err, "reopen: %v", err)
			defer db2.Close()
			snaps, _ := db2.ListSnapshots(context.Background())
			require.Len(t, snaps, 1, "snapshots = %d, want 1 (truncated tail dropped)", len(snaps))
			_, err = db2.Get(context.Background(), snaps[0].ID, 1, 9999, nil)
			require.Error(t, err, "row 9999 must be gone with the uncommitted tail: %v", err)
		})
	}
}

// TestM8IndexDeleted was removed with the index file (v2): there is no
// separate .rpi to delete, and a committed snapshot whose IndexTxn is
// missing or corrupt recovers automatically in memory (TestM8RebuildSnapshotFromBlocks).

// TestM8MidFileCorruption verifies structural corruption between two valid
// snapshots is a hard error, never silently skipped. Two flavors: a block
// header CRC flipped inside a committed snapshot, and a malformed snapshot
// region followed by a complete one.
func TestM8MidFileCorruption(t *testing.T) {
	t.Run("block-header-crc", func(t *testing.T) {
		base := filepath.Join(tmpdb(t), "mid")
		opts := Options{}
		opts.BlockSize = 2048
		db, fullID := buildConcurrentStore(t, base, opts)
		w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: fullID})
		require.NoError(t, w.Insert(context.Background(), 1, 3001, 1, Row{Uint64(3001), String("d1")}))
		_, err := w.Commit(context.Background())
		require.NoError(t, err)
		db.Close()

		// Corrupt a block header CRC in the FIRST snapshot (in the middle).
		data, err := os.ReadFile(base + ".rpk")
		require.NoError(t, err)
		// Find the second block header in snapshot 1: walk from offset 128.
		pos := 128 + 96
		pos += 64 + int(le32(data[pos+44:])) // skip block 1
		// block 2 header CRC at pos+52
		data[pos+52] ^= 0xFF
		require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))
		_, err = Open(base, Options{})
		require.Error(t, err, "mid-file corruption opened successfully")
	})

	t.Run("broken-snapshot-region", func(t *testing.T) {
		base := filepath.Join(tmpdb(t), "mid-region")
		db := newEmptyStoreAt(t, base)
		commitOneFull(t, db, 5)
		db.Close()

		// Truncated snapshot at offset X, followed by a complete one: the
		// incomplete region is mid-file corruption, never silently skipped.
		raw := craftedSnapshotPair(t, 5) // valid header (walk starts), 96-byte footer acts as garbage
		broken := raw[:fileformat.SnapshotHeaderSize]
		broken = append(broken, bytes.Repeat([]byte{0xA5}, 256)...)
		broken = append(broken, craftedSnapshotPair(t, 99)...)
		appendFile(t, base+".rpk", broken)

		_, err := Open(base, Options{})
		require.Error(t, err, "Open error = %v, want mid-file corruption", err)
		require.Contains(t, err.Error(), "mid-file corruption", "Open error = %v, want mid-file corruption", err)
	})
}

// TestM8Verify covers VerifyQuick and VerifyFull on a healthy plain and
// encrypted store, and on a store with payload corruption (AC-009): plain
// payload tampering fails full verify, ciphertext tampering fails both modes
// with ErrAuthFailed.
func TestM8Verify(t *testing.T) {
	t.Run("plain", func(t *testing.T) {
		base := filepath.Join(tmpdb(t), "verify")
		db, fullID := buildConcurrentStore(t, base, Options{})
		w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: fullID})
		_ = w.Insert(context.Background(), 1, 5001, 1, Row{Uint64(5001), String("v")})
		_, err := w.Commit(context.Background())
		require.NoError(t, err)
		rep, err := db.Verify(context.Background(), VerifyQuick)
		require.NoError(t, err, "quick verify: %v", err)
		require.Equal(t, uint64(2), rep.SnapshotsChecked, "quick verify report: %+v", rep)
		require.NotZero(t, rep.BlocksChecked, "quick verify report: %+v", rep)
		rep, err = db.Verify(context.Background(), VerifyFull)
		require.NoError(t, err, "full verify: %v", err)
		require.Equal(t, uint64(2), rep.SnapshotsChecked, "full verify report: %+v", rep)
		require.Equal(t, uint64(2001), rep.RowsChecked, "full verify checked %d rows, want 2001 (2000 FULL + 1 DELTA)", rep.RowsChecked)
		db.Close()

		// Payload corruption must be caught by full verify.
		data, _ := os.ReadFile(base + ".rpk")
		// Corrupt a byte in the last block payload: find the second block header
		// of snapshot 1 and flip a payload byte.
		pos := 128 + 96
		pos += 64 + int(le32(data[pos+44:])) // block 1
		pos += 64                            // block 2 header
		data[pos+10] ^= 0xFF                 // inside payload
		os.WriteFile(base+".rpk", data, 0o644)

		db2, err := Open(base, Options{})
		require.NoError(t, err, "reopen with payload corruption: %v", err)
		defer db2.Close()
		_, err = db2.Verify(context.Background(), VerifyFull)
		require.Error(t, err, "full verify missed payload corruption")
	})

	// The encrypted store authenticates every block in both modes: quick
	// skips only the per-row decoding, so a flipped ciphertext byte fails
	// quick and full alike with ErrAuthFailed.
	t.Run("encrypted", func(t *testing.T) {
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
	})
}

// tamperFirstIndexTxn flips one byte inside the first IndexTxn body so the
// stored bytes no longer match the footer-bound IndexTxnCRC32C (plain) or the
// ciphertext no longer authenticates (encrypted): the first snapshot is still
// committed, but its txn must be rebuilt in memory on open.
func tamperFirstIndexTxn(tb testing.TB, path string) {
	tb.Helper()
	data, err := os.ReadFile(path)
	require.NoError(tb, err)
	first := bytes.Index(data, []byte(fileformat.MagicIndexTxnHdr))
	require.Greater(tb, first, 0, "no IndexTxnHeader found")
	body := first + fileformat.IndexTxnHeaderSize + 10 // inside SnapshotIndexEntry / ciphertext
	require.Less(tb, body, len(data), "txn body offset out of range")
	data[body] ^= 0xFF
	require.NoError(tb, os.WriteFile(path, data, 0o644))
}

// TestM8RebuildSnapshotFromBlocks corrupts the first snapshot's stored
// IndexTxn (plain and encrypted) and verifies open rebuilds it in memory from
// its own blocks while later snapshots replay normally (BINARY_FORMAT_V2
// §10.2, R2).
func TestM8RebuildSnapshotFromBlocks(t *testing.T) {
	for _, enc := range []bool{false, true} {
		name := "plain"
		opts := Options{}
		if enc {
			name = "encrypted"
			opts = encOptions("tx")
		}
		t.Run(name, func(t *testing.T) {
			base := filepath.Join(tmpdb(t), "rebuild")
			db, _ := buildConcurrentStore(t, base, opts)
			w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
			_ = w.Insert(context.Background(), 1, 5001, 1, Row{Uint64(5001), String("v")})
			_, err := w.Commit(context.Background())
			require.NoError(t, err)
			db.Close()

			// Flip one byte inside the FIRST IndexTxn body: the txn must still
			// parse structurally, but its CRC (also bound by the footer)
			// diverges from the stored bytes (or the ciphertext stops
			// authenticating), so the snapshot's index is rebuilt in memory.
			tamperFirstIndexTxn(t, base+".rpk")

			db2, err := Open(base, opts)
			require.NoError(t, err, "reopen with corrupt IndexTxn: %v", err)
			defer db2.Close()
			require.Equal(t, uint64(1), db2.Stats().Recovery.SnapshotsRebuilt, "SnapshotsRebuilt = %d, want 1", db2.Stats().Recovery.SnapshotsRebuilt)
			// The first snapshot's rows still resolve (rebuilt in memory) ...
			r, err := db2.Get(context.Background(), 1, 1, 42, nil)
			require.NoError(t, err, "row 42: %v", err)
			v, _ := r[1].String()
			require.Equal(t, "n-42", v, "row 42 = %q", v)
			// ... and the second snapshot's txn replayed normally.
			r2, err := db2.Get(context.Background(), 2, 1, 5001, nil)
			require.NoError(t, err, "row 5001: %v", err)
			v2, _ := r2[1].String()
			require.Equal(t, "v", v2, "row 5001 = %q", v2)
		})
	}
}

// TestM8DataTailTruncated simulates a partial snapshot (or plain garbage)
// appended after the last committed one; read-write open truncates it,
// read-only ignores it.
func TestM8DataTailTruncated(t *testing.T) {
	t.Run("partial-snapshot", func(t *testing.T) {
		base := filepath.Join(tmpdb(t), "tail")
		db, _ := buildConcurrentStore(t, base, Options{})
		db.Close()

		// Append a partial snapshot (header + one block, no footer).
		data, _ := os.ReadFile(base + ".rpk")
		partial := append([]byte(nil), data...)
		var sh fileformat.SnapshotHeader
		sh.SnapshotType = fileformat.SnapshotFull
		sh.SnapshotID = 99
		sh.CreatedUnixNano = 1700000000000000000
		var shBuf [96]byte
		_ = sh.MarshalTo(shBuf[:])
		partial = append(partial, shBuf[:]...)
		// one fake block: reuse a valid block header from the existing data.
		fake := append([]byte(nil), data[224:224+64]...) // block header bytes
		partial = append(partial, fake...)
		os.WriteFile(base+".rpk", partial, 0o644)

		// Read-only: ignores the tail, doesn't modify.
		ro, err := Open(base, Options{ReadOnly: true})
		require.NoError(t, err, "read-only open: %v", err)
		_, err = ro.Get(context.Background(), 1, 1, 1, nil)
		require.NoError(t, err)
		ro.Close()
		sz, _ := os.Stat(base + ".rpk")
		require.Equal(t, int64(len(partial)), sz.Size(), "read-only open modified the data file")

		// Read-write: truncates the tail; recovery reported.
		db2, err := Open(base, Options{})
		require.NoError(t, err, "read-write open: %v", err)
		st := db2.Stats()
		require.True(t, st.Recovery.Performed, "recovery not reported: %+v", st.Recovery)
		require.NotZero(t, st.Recovery.DataTailIgnored, "recovery not reported: %+v", st.Recovery)
		db2.Close()
		sz2, _ := os.Stat(base + ".rpk")
		require.Equal(t, int64(len(data)), sz2.Size(), "tail not truncated: %d -> %d", len(partial), sz2.Size())
	})

	t.Run("garbage", func(t *testing.T) {
		base := filepath.Join(tmpdb(t), "garbage")
		db := newEmptyStoreAt(t, base)
		commitOneFull(t, db, 5)
		db.Close()

		sizeBefore := fileSize(t, base+".rpk")
		appendFile(t, base+".rpk", bytes.Repeat([]byte{0xA5}, 512))

		db2, err := Open(base, Options{})
		require.NoError(t, err, "reopen with garbage tail: %v", err)
		defer db2.Close()
		st := db2.Stats()
		require.True(t, st.Recovery.Performed, "recovery stats = %+v", st.Recovery)
		require.Equal(t, uint64(512), st.Recovery.DataTailIgnored, "recovery stats = %+v", st.Recovery)
		require.Equal(t, sizeBefore, fileSize(t, base+".rpk"), "tail not truncated: %d -> %d", sizeBefore, fileSize(t, base+".rpk"))
		// Data is intact.
		_, err = db2.Get(context.Background(), 1, 1, 5, nil)
		require.NoError(t, err, "row 5 after recovery: %v", err)
	})
}
