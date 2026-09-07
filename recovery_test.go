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

// runCrashChild re-invokes the test binary to create a store and commit a
// snapshot, dying via os.Exit at the given fault point. The files are left in
// the crashed state for the caller to reopen.
func runCrashChild(t *testing.T, base string, faultPoint string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestCrashChildHelper")
	cmd.Env = append(os.Environ(), "ROWCRASH_BASE="+base, "ROWCRASH_FAULT="+faultPoint)
	out, err := cmd.CombinedOutput()
	_ = out
	if err != nil {
		// The child exits non-zero by design (os.Exit(0) inside a test);
		// failures in setup also surface here.
		t.Logf("crash child output: %s", out)
	}
}

// TestCrashChildHelper is the crash simulation entry: it creates a store,
// writes a FULL snapshot of 100 rows, and commits. A fault at the requested
// point terminates the process mid-commit.
func TestCrashChildHelper(t *testing.T) {
	base := os.Getenv("ROWCRASH_BASE")
	if base == "" {
		t.Skip("not a crash child")
	}
	fp := os.Getenv("ROWCRASH_FAULT")
	fault.Inject(fp, func() { os.Exit(0) })
	t.Cleanup(fault.Clear)

	db, err := Create(base, Options{})
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

// verifyCrashRecovery reopens the crashed store and asserts the committed
// state (which snapshot count and row visibility are expected).
func verifyCrashRecovery(t *testing.T, base string, wantSnapshots int) *Store {
	t.Helper()
	db, err := Open(base, Options{})
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

// TestM8CrashFaultPoints crashes at every injection point and verifies the
// recovery outcome is deterministic and idempotent (AC-008).
func TestM8CrashFaultPoints(t *testing.T) {
	cases := []struct {
		point         string
		wantSnapshots int
	}{
		{"commit.data-header.before", 0}, // nothing written
		{"commit.block.before", 0},       // header only, no footer
		{"commit.data-footer.after", 1},  // data committed, no index
		{"commit.data-sync.after", 1},    // data synced, no index
		{"commit.index.before", 1},       // data committed, no index
		{"commit.index-sync.before", 1},  // index written but not synced
		{"commit.publish.after", 1},      // fully committed
	}
	for _, c := range cases {
		t.Run(c.point, func(t *testing.T) {
			base := filepath.Join(tmpdb(t), "store")
			runCrashChild(t, base, c.point)
			// First reopen: recovery happens.
			db := verifyCrashRecovery(t, base, c.wantSnapshots)
			db.Close()
			// Second reopen: idempotent, no further changes.
			db2 := verifyCrashRecovery(t, base, c.wantSnapshots)
			db2.Close()
			// Third reopen read-only: never modifies files.
			before := fileSizes(t, base)
			ro, err := Open(base, Options{ReadOnly: true})
			require.NoError(t, err, "read-only reopen: %v", err)
			ro.Close()
			after := fileSizes(t, base)
			for _, ext := range []string{".rpk", ".rpi"} {
				require.Equal(t, before[ext], after[ext], "read-only open modified %s: %d -> %d", ext, before[ext], after[ext])
			}
		})
	}
}

func fileSizes(t *testing.T, base string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, ext := range []string{".rpk", ".rpi", ".lock"} {
		if fi, err := os.Stat(base + ext); err == nil {
			out[ext] = fi.Size()
		}
	}
	return out
}

// TestM8IndexTruncated simulates a crash mid-index-write by truncating the
// index file to a partial transaction.
func TestM8IndexTruncated(t *testing.T) {
	base := filepath.Join(tmpdb(t), "it")
	db, fullID := buildConcurrentStore(t, base, Options{})
	// Commit a second snapshot so the index has two txns.
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: fullID})
	require.NoError(t, w.Insert(context.Background(), 1, 9999, 1, Row{Uint64(9999), String("x")}))
	_, err := w.Commit(context.Background())
	require.NoError(t, err)
	db.Close()

	// Truncate the index file mid-second-txn.
	idxPath := base + ".rpi"
	fi, _ := os.Stat(idxPath)
	f, _ := os.OpenFile(idxPath, os.O_RDWR, 0o644)
	truncLen := fi.Size() - 40 // cut into the second txn footer
	f.Truncate(truncLen)
	f.Close()

	db2, err := Open(base, Options{})
	require.NoError(t, err, "reopen: %v", err)
	defer db2.Close()
	snaps, _ := db2.ListSnapshots(context.Background())
	require.Len(t, snaps, 2, "snapshots = %d, want 2 (index tail rebuilt from data)", len(snaps))
	r, err := db2.Get(context.Background(), snaps[1].ID, 1, 9999, nil)
	require.NoError(t, err, "row 9999: %v", err)
	v, _ := r[1].String()
	require.Equal(t, "x", v, "row 9999 = %q", v)
}

// TestM8IndexDeleted verifies that a completely missing index requires
// explicit RebuildIndex (Open never opens without both files), and that the
// rebuilt store is fully readable.
func TestM8IndexDeleted(t *testing.T) {
	base := filepath.Join(tmpdb(t), "noidx")
	db, _ := buildConcurrentStore(t, base, Options{})
	db.Close()
	require.NoError(t, os.Remove(base+".rpi"))
	_, err := Open(base, Options{})
	require.Error(t, err, "Open succeeded with a missing index file")
	require.NoError(t, RebuildIndex(context.Background(), base, RebuildOptions{Durability: SyncCommit}), "rebuild: %v", err)
	db2, err := Open(base, Options{})
	require.NoError(t, err, "reopen after rebuild: %v", err)
	defer db2.Close()
	snaps, _ := db2.ListSnapshots(context.Background())
	require.Len(t, snaps, 1, "snapshots = %d, want 1", len(snaps))
	_, err = db2.Get(context.Background(), 1, 1, 42, nil)
	require.NoError(t, err, "row 42: %v", err)
}

// TestM8MidFileCorruption verifies structural corruption between two valid
// snapshots is a hard error, never silently skipped.
func TestM8MidFileCorruption(t *testing.T) {
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
}

// TestM8Verify covers VerifyQuick and VerifyFull on a healthy store and on a
// store with payload corruption (AC-009).
func TestM8Verify(t *testing.T) {
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
	require.NotZero(t, rep.RowsChecked, "full verify checked no rows: %+v", rep)
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
}

// TestM8RebuildIndex rebuilds a deleted index through the explicit API.
func TestM8RebuildIndex(t *testing.T) {
	base := filepath.Join(tmpdb(t), "rebuild")
	db, _ := buildConcurrentStore(t, base, Options{})
	db.Close()
	require.NoError(t, os.Remove(base+".rpi"))
	require.NoError(t, RebuildIndex(context.Background(), base, RebuildOptions{Durability: SyncCommit}))
	db2, err := Open(base, Options{})
	require.NoError(t, err, "reopen after rebuild: %v", err)
	defer db2.Close()
	r, err := db2.Get(context.Background(), 1, 1, 42, nil)
	require.NoError(t, err, "row 42: %v", err)
	v, _ := r[1].String()
	require.Equal(t, "n-42", v, "row 42 = %q", v)
}

// TestM8DataTailTruncated simulates a partial snapshot appended after the
// last committed one; read-write open truncates it, read-only ignores it.
func TestM8DataTailTruncated(t *testing.T) {
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
}
