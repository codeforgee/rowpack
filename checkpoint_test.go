package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fault"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// ---- V2-M3: DELTA 与恢复深化 ----

// TestFullCheckpoint verifies the v2 checkpoint semantics (R7): a later FULL
// takes the next global ID, resets Depth to 1, and its visibility no longer
// follows any ancestor chain; older snapshots stay readable; the chain
// continues normally after the new FULL.
func TestFullCheckpoint(t *testing.T) {
	base := filepath.Join(tmpdb(t), "ckpt")
	db, err := Create(base, Options{})
	require.NoError(t, err)

	// FULL(1): table 1 with rows 1..10.
	w1, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w1.DefineSchema(schema1()))
	sch := schema1()
	sch.TableID = 1
	require.NoError(t, w1.DefineSchema(sch))
	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, w1.Insert(context.Background(), 1, i, 1, row1(i)))
	}
	f1, err := w1.Commit(context.Background())
	require.NoError(t, err)
	require.Equal(t, SnapshotID(1), f1.ID, "first FULL id")

	// DELTA(2): update row 2.
	w2, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: f1.ID})
	require.NoError(t, err)
	require.NoError(t, w2.Update(context.Background(), 1, 2, 1, Row{Uint64(2), String("delta-2")}))
	f2, err := w2.Commit(context.Background())
	require.NoError(t, err)
	require.Equal(t, SnapshotID(2), f2.ID)

	// FULL(3): a checkpoint that reintroduces schema and rows 1..5.
	w3, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w3.DefineSchema(sch)) // checkpoint carries its own schema
	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, w3.Insert(context.Background(), 1, i, 1, Row{Uint64(i), String("ckpt-3")}))
	}
	f3, err := w3.Commit(context.Background())
	require.NoError(t, err)
	require.Equal(t, SnapshotID(3), f3.ID, "checkpoint FULL must keep the global counter")

	// DELTA(4) on the checkpoint.
	w4, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: f3.ID})
	require.NoError(t, err)
	require.NoError(t, w4.Insert(context.Background(), 1, 11, 1, row1(11)))
	f4, err := w4.Commit(context.Background())
	require.NoError(t, err)
	require.Equal(t, SnapshotID(4), f4.ID)

	st := db.state.Load()
	require.Equal(t, uint32(1), st.view.Snapshot(1).Depth)
	require.Equal(t, uint32(2), st.view.Snapshot(2).Depth)
	require.Equal(t, uint32(1), st.view.Snapshot(3).Depth, "checkpoint FULL must reset depth")
	require.Equal(t, uint32(2), st.view.Snapshot(4).Depth)

	// FULL(3) visibility is independent of 1/2.
	_, err = db.Get(context.Background(), 3, 1, 1, nil)
	require.NoError(t, err, "snapshot 3 row 1")
	r, err := db.Get(context.Background(), 3, 1, 2, nil)
	require.NoError(t, err)
	n, _ := r[1].String()
	require.Equal(t, "ckpt-3", n, "snapshot 3 row 2 = %q, want checkpooint value (not delta-2)", n)
	_, err = db.Get(context.Background(), 3, 1, 6, nil)
	require.ErrorIs(t, err, ErrNotFound, "snapshot 3 must not see ancestor row 6")
	_, err = db.Get(context.Background(), 3, 1, 11, nil)
	require.ErrorIs(t, err, ErrNotFound, "snapshot 3 must not see descendant row 11")

	// DELTA(4) sees checkpoint rows + own.
	_, err = db.Get(context.Background(), 4, 1, 11, nil)
	require.NoError(t, err)
	_, err = db.Get(context.Background(), 4, 1, 10, nil)
	require.ErrorIs(t, err, ErrNotFound, "snapshot 4 must not fall through past the checkpoint")

	// Older snapshots stay readable.
	r2, err := db.Get(context.Background(), 2, 1, 2, nil)
	require.NoError(t, err)
	n2, _ := r2[1].String()
	require.Equal(t, "delta-2", n2, "snapshot 2 row 2 = %q", n2)
	require.NoError(t, db.Close())

	// Reopen: the checkpoint chain replays and stays consistent.
	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer db2.Close()
	r3, err := db2.Get(context.Background(), 4, 1, 11, nil)
	require.NoError(t, err)
	n3, _ := r3[1].String()
	exp, _ := row1(11)[1].String()
	require.Equal(t, exp, n3, "row 11 after reopen = %q", n3)
	_, err = db2.Get(context.Background(), 2, 1, 2, nil)
	require.NoError(t, err, "snapshot 2 after reopen")
}

// TestFooterChainLinks verifies the PreviousFooterOffset chain recorded by
// every committed footer exactly links the previous footer position.
func TestFooterChainLinks(t *testing.T) {
	base := filepath.Join(tmpdb(t), "chain")
	db, _ := buildConcurrentStore(t, base, Options{})
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, w.Insert(context.Background(), 1, 9998, 1, Row{Uint64(9998), String("x")}))
	_, err := w.Commit(context.Background())
	require.NoError(t, err)
	w2, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 2})
	require.NoError(t, w2.Insert(context.Background(), 1, 9999, 1, Row{Uint64(9999), String("y")}))
	_, err = w2.Commit(context.Background())
	require.NoError(t, err)

	committed, tailStart, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 3, "committed snapshots")
	size, _ := db.data.Size()
	require.Equal(t, size, tailStart, "no uncommitted tail expected")
	require.Zero(t, committed[0].prevFooter, "first footer prev must be 0")
	for i := 1; i < len(committed); i++ {
		require.Equal(t, uint64(committed[i-1].footerOff), committed[i].prevFooter,
			"footer[%d].prevFooter must point at footer[%d]", i, i-1)
	}
}

// TestFooterCorruptionLast flips a byte in the LAST SnapshotFooter: the last
// snapshot loses its commit evidence, so read-write Open treats everything
// from its header as an uncommitted tail and truncates it away.
func TestFooterCorruptionLast(t *testing.T) {
	base := filepath.Join(tmpdb(t), "fc-last")
	db, _ := buildConcurrentStore(t, base, Options{})
	db.Close()

	committed, _, err := openForScan(t, base)
	require.NoError(t, err)
	require.Len(t, committed, 1, "expect one committed snapshot")
	off := committed[len(committed)-1].footerOff + fileformat.SnapshotFooterCRC32COffset
	flipByte(t, base+".rpk", off)

	db2, err := Open(base, Options{})
	require.NoError(t, err, "last footer corruption must open (tail dropped): %v", err)
	defer db2.Close()
	snaps, _ := db2.ListSnapshots(context.Background())
	require.Len(t, snaps, 0, "last snapshot must be dropped as uncommitted tail")
}

// TestFooterCorruptionMid flips a byte in a MIDDLE SnapshotFooter: a valid
// footer exists later, so the broken region is mid-file corruption and Open
// fails hard (a committed snapshot must never be silently lost).
func TestFooterCorruptionMid(t *testing.T) {
	base := filepath.Join(tmpdb(t), "fc-mid")
	db, _ := buildConcurrentStore(t, base, Options{})
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, w.Insert(context.Background(), 1, 8888, 1, Row{Uint64(8888), String("z")}))
	_, err := w.Commit(context.Background())
	require.NoError(t, err)
	db.Close()

	committed, _, err := openForScan(t, base)
	require.NoError(t, err)
	require.Len(t, committed, 2, "expect two committed snapshots")
	off := committed[0].footerOff + fileformat.SnapshotFooterCRC32COffset
	flipByte(t, base+".rpk", off)

	_, err = Open(base, Options{})
	require.Error(t, err, "middle footer corruption must fail open")
	require.Contains(t, err.Error(), "mid-file corruption", "err = %v", err)
}

// TestCommitErrorUnknownOnSyncFailure verifies that a failure during the
// single commit sync reports CommitError with Unknown:true: every byte of the
// transaction is already appended when the sync point is reached, so the
// outcome cannot be determined without a snapshot query (writer.go
// commitLocked, BINARY_FORMAT_V2 §8).
func TestCommitErrorUnknownOnSyncFailure(t *testing.T) {
	db := newEmptyStore(t)
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	require.NoError(t, w.DefineSchema(testSchema()))
	require.NoError(t, w.Insert(context.Background(), 1, 1, 1, Row{Uint64(1), String("a")}))
	// Break the file handle exactly at the sync point: header, blocks, txn
	// and footer are appended, the single Sync fails.
	fault.Inject("commit.sync.before", func() { _ = db.data.Close() })
	t.Cleanup(fault.Clear)
	_, err = w.Commit(context.Background())
	require.Error(t, err, "commit must fail")
	var ce *CommitError
	require.ErrorAs(t, err, &ce)
	require.True(t, ce.Unknown, "sync failure must be Unknown: %+v", ce)
	_ = db.Close() // the handle is already closed; ignore double-close errors
}

// TestEmptyDeltaReopen commits an empty DELTA and verifies the zero-block
// snapshot (IndexTxnHeader directly after SnapshotHeader) walks, replays and
// reopens correctly.
func TestEmptyDeltaReopen(t *testing.T) {
	base := filepath.Join(tmpdb(t), "empty-delta")
	db := newEmptyStoreAt(t, base)
	f := commitOneFull(t, db, 5)
	w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: f})
	require.NoError(t, err)
	_, err = w.Commit(context.Background())
	require.NoError(t, err, "empty delta commit")
	require.NoError(t, db.Close())

	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer db2.Close()
	snaps, err := db2.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Len(t, snaps, 2, "snapshots = %d, want 2", len(snaps))
	require.Equal(t, uint64(2), snaps[1].ID)
	_, err = db2.Get(context.Background(), 2, 1, 5, nil)
	require.NoError(t, err, "empty delta must still see the full row")
}

// openForScan opens the store read-write and returns the committed-snapshot
// list plus tail start (test helper).
func openForScan(t *testing.T, base string) ([]committedSnapshot, int64, error) {
	t.Helper()
	db, err := Open(base, Options{})
	require.NoError(t, err)
	defer db.Close()
	return db.scanDataFile()
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	b := make([]byte, 1)
	_, err = f.ReadAt(b, off)
	require.NoError(t, err)
	b[0] ^= 0xFF
	_, err = f.WriteAt(b, off)
	require.NoError(t, err)
}