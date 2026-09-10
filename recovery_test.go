package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/fault"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// crashCommit runs Commit with a crash injected at the fault point; the
// injected panic simulates process death at exactly that position.
func crashCommit(ctx context.Context, tx *Tx, point string) (crashed bool, err error) {
	fault.Inject(point, func() { panic("injected crash at " + point) })
	defer fault.Clear()
	defer func() {
		if r := recover(); r != nil {
			crashed = true
		}
	}()
	_, err = tx.Commit(ctx)
	return crashed, err
}

// scratchStore builds a fresh store under testdata/tmpdb with a small block
// size and no determinism hooks (real random UUID/nonce).
func scratchStore(t *testing.T) *Store {
	t.Helper()
	db, err := Create(filepath.Join(tmpdb(t), "store"), Options{BlockSize: 1024})
	require.NoError(t, err)
	return db
}

// buildTwoSnapshots commits a FULL (rows 1..20) then a DELTA (delete row 5)
// and returns the delta snapshot ID.
func buildTwoSnapshots(t *testing.T, db *Store) SnapshotID {
	t.Helper()
	ctx := context.Background()
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 20)
	full, err := w.Commit(ctx)
	require.NoError(t, err)
	d, _ := db.Begin(ctx, full)
	require.NoError(t, d.Delete("users", 5))
	delta, err := d.Commit(ctx)
	require.NoError(t, err)
	return delta
}

// beginThird opens a FULL writer for snapshot 3.
func beginThird(t *testing.T, db *Store) *Tx {
	t.Helper()
	w, err := db.Begin(context.Background(), NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 3)
	return w
}

// TestCrashAtCommitPoints drives a crash at every commit fault point and
// verifies the tornado-recovery contract point by point.
func TestCrashAtCommitPoints(t *testing.T) {
	type expect struct {
		durable   bool // snapshot has a valid footer in the file when the crash fires
		tailBytes bool // an uncommitted tail (>0 bytes) is left behind
		inMemory  bool // the crash fires after the in-memory publish
	}
	cases := map[string]expect{
		"commit.header.before":  {durable: false, tailBytes: false, inMemory: false},
		"commit.block.before":   {durable: false, tailBytes: true, inMemory: false},
		"commit.txn.before":     {durable: false, tailBytes: true, inMemory: false},
		"commit.footer.after":   {durable: true, tailBytes: false, inMemory: false},
		"commit.sync.before":    {durable: true, tailBytes: false, inMemory: false},
		"commit.sync.after":     {durable: true, tailBytes: false, inMemory: false},
		"commit.publish.before": {durable: true, tailBytes: false, inMemory: false},
		"commit.publish.after":  {durable: true, tailBytes: false, inMemory: true},
	}
	for point, ex := range cases {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			db := scratchStore(t)
			buildTwoSnapshots(t, db)
			w := beginThird(t, db)

			crashed, err := crashCommit(ctx, w, point)
			require.True(t, crashed, "fault point %q must interrupt commit (err=%v)", point, err)

			// In-memory view of the crashed instance: published only at
			// publish.after; everywhere else the crashed snapshot is absent.
			path := db.Path()
			_, gerr := db.Get(ctx, 3, "users", 1, nil)
			if ex.inMemory {
				require.NoError(t, gerr, "%s: state was published in memory", point)
			} else {
				require.ErrorIs(t, gerr, ErrNotFound, "%s: crashed snapshot invisible in memory", point)
			}
			require.NoError(t, db.Close())

			// Reopen: durable points recover the snapshot; torn points lose it.
			db2, err := Open(path, Options{BlockSize: 1024})
			require.NoError(t, err)
			t.Cleanup(func() { db2.Close() })
			snaps, err := db2.ListSnapshots(ctx)
			require.NoError(t, err)
			wantSnaps := 2
			if ex.durable {
				wantSnaps = 3
			}
			require.Len(t, snaps, wantSnaps, "%s: snapshot count after reopen", point)
			if ex.durable {
				_, gerr := db2.Get(ctx, 3, "users", 1, nil)
				require.NoError(t, gerr, "%s: durable snapshot readable after reopen", point)
			}
			rec := db2.Stats().Recovery
			require.Equal(t, ex.tailBytes, rec.DataTailIgnored > 0, "%s: tail bytes", point)
			require.Equal(t, ex.tailBytes, rec.Performed, "%s: recovery performed only for torn tails", point)

			// The two pre-crash snapshots are always intact.
			row, err := db2.Get(ctx, 2, "users", 1, nil)
			require.NoError(t, err)
			name, _ := row[1].String()
			require.Equal(t, "user-1", name)
			if ex.durable {
				_, gerr := db2.Get(ctx, 2, "users", 5, nil)
				require.ErrorIs(t, gerr, ErrNotFound, "delta tombstone survives the torn commit")
			}
		})
	}
}

// TestTornTailTruncation appends uncommitted garbage to a clean store: a
// read-write open truncates it; a read-only open reports it untouched.
func TestTornTailTruncation(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "torn")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 5)
	full, _ := w.Commit(ctx)
	sizeBefore, err := db.data.Size()
	require.NoError(t, err)
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR|os.O_APPEND, 0)
	require.NoError(t, err)
	var garbage [512]byte
	for i := range garbage {
		garbage[i] = byte(i)
	}
	_, err = f.Write(garbage[:])
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Read-only open: reports the tail, does not truncate.
	ro, err := Open(base, Options{ReadOnly: true, BlockSize: 1024})
	require.NoError(t, err)
	snaps, err := ro.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	rec := ro.Stats().Recovery
	require.True(t, rec.Performed)
	require.Equal(t, uint64(len(garbage)), rec.DataTailIgnored)
	sz, _ := ro.data.Size()
	require.Equal(t, sizeBefore+int64(len(garbage)), sz, "read-only open must not truncate")
	require.NoError(t, ro.Close())

	// Read-write open truncates the tail.
	dbrw, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { dbrw.Close() })
	sz, _ = dbrw.data.Size()
	require.Equal(t, sizeBefore, sz, "read-write open truncates the uncommitted tail")
	snaps, err = dbrw.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1)
	row, err := dbrw.Get(ctx, full, "users", 3, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-3", name)
}

// TestCorruptIndexTxnRebuild corrupts snapshot 1's IndexTxn while snapshot 2
// stays valid: the open must succeed, rebuild the damaged snapshot's index
// from its own blocks in memory, and keep both snapshots fully readable.
func TestCorruptIndexTxnRebuild(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "rebuilt")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	delta := buildTwoSnapshots(t, db)

	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	snap1 := committed[0]
	require.Greater(t, snap1.txnEnd-snap1.txnStart, int64(16))
	require.NoError(t, db.Close())

	// Corrupt bytes in the middle of snapshot 1's IndexTxn extent.
	mid := snap1.txnStart + (snap1.txnEnd-snap1.txnStart)/2
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 2, "corrupt txn must not drop the snapshot")
	rec := db2.Stats().Recovery
	require.True(t, rec.Performed)
	require.Equal(t, uint64(1), rec.SnapshotsRebuilt, "snapshot 1 index rebuilt in memory")
	require.Greater(t, rec.IndexTailIgnored, uint64(0))

	// Both snapshots read correctly despite the rebuilt index.
	row, err := db2.Get(ctx, 1, "users", 7, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-7", name)
	_, err = db2.Get(ctx, delta, "users", 5, nil)
	require.ErrorIs(t, err, ErrNotFound, "delta tombstone still resolved over the rebuilt index")
}

// TestCorruptBlockHeaderMidFile corrupts a block header between two valid
// footers: because a later valid footer exists this is mid-file corruption,
// never an uncommitted tail, and Open must fail.
func TestCorruptBlockHeaderMidFile(t *testing.T) {
	base := filepath.Join(tmpdb(t), "midcorrupt")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)

	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	blkOff := committed[0].blocksStart // first BlockHeader of snapshot 1
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("GARBAGE!"), blkOff)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = Open(base, Options{BlockSize: 1024})
	require.Error(t, err, "mid-file corruption must fail the open")
	require.Contains(t, err.Error(), "mid-file corruption")
}

// TestCorruptBlockPayloadDamagesRead corrupts one byte of a rows block
// payload: the open survives (index replay is in-memory), reads of that
// block fail with ErrCorruptData, and Verify reports a structured
// CorruptionError.
func TestCorruptBlockPayloadDamagesRead(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "rowcorrupt")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)

	// Find the block that physically holds row 1 via the published view and
	// patch its stored payload (the block flush order vs. commit-time flush
	// varies, so address the block by the row's resolved location).
	st, err := db.captureState()
	require.NoError(t, err)
	loc, ok := st.view.ResolveRow(1, 1, 1)
	require.True(t, ok, "row 1 must resolve")
	rowsBlk := st.view.Block(loc.BlockID)
	require.NotNil(t, rowsBlk)
	require.Equal(t, fileformat.BlockKindRows, rowsBlk.Kind)
	payloadOff := int64(rowsBlk.DataOffset) + fileformat.BlockHeaderSize
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	payload := make([]byte, 32)
	_, err = f.ReadAt(payload, payloadOff)
	require.NoError(t, err)
	payload[0] ^= 0xFF
	_, err = f.WriteAt(payload, payloadOff)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	// A read touching the damaged block fails with ErrCorruptData.
	_, err = db2.Get(ctx, 1, "users", 1, nil)
	require.ErrorIs(t, err, ErrCorruptData)
	// VerifyFull and VerifyQuick both report a structured error.
	for _, mode := range []VerifyMode{VerifyFull, VerifyQuick} {
		_, err = db2.Verify(ctx, mode)
		var cerr *CorruptionError
		require.ErrorAs(t, err, &cerr, "verify mode %v", mode)
		require.ErrorIs(t, cerr, ErrCorruptData)
		require.Equal(t, db2.dataPath, cerr.File)
	}
}

// TestVerifyCleanStore checks the healthy-path verify counts.
func TestVerifyCleanStore(t *testing.T) {
	ctx := context.Background()
	db := scratchStore(t)
	buildTwoSnapshots(t, db)
	rep, err := db.Verify(ctx, VerifyFull)
	require.NoError(t, err)
	require.Equal(t, uint64(2), rep.SnapshotsChecked)
	require.Greater(t, rep.BlocksChecked, uint64(0))
	require.Greater(t, rep.RowsChecked, uint64(0))
	require.Greater(t, rep.DataBytesRead, uint64(0))
	require.Greater(t, rep.Duration, int64(0))
	quick, err := db.Verify(ctx, VerifyQuick)
	require.NoError(t, err)
	require.Equal(t, rep.SnapshotsChecked, quick.SnapshotsChecked)
}

// TestEncryptedRecovery drives the recovery contract on an encrypted store:
// a torn commit tail is truncated on reopen and data reads back with the key.
func TestEncryptedRecovery(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "enc-rec")
	db, err := Create(base, encOptions("k1"))
	require.NoError(t, err)
	w, _ := db.Begin(ctx, NoParent)
	require.NoError(t, w.DefineTable("users", usersSchema()))
	insertUsers(t, w, 8)
	full, _ := w.Commit(ctx)
	w2 := beginThird(t, db)

	crashed, _ := crashCommit(ctx, w2, "commit.txn.before")
	require.True(t, crashed)
	require.NoError(t, db.Close())

	db2, err := Open(base, encOptions("k1"))
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1, "encrypted torn commit counts only the committed full")
	require.True(t, db2.Stats().Recovery.Performed)
	row, err := db2.Get(ctx, full, "users", 3, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-3", name)
}

// TestEncryptedRebuildRebuildsWithKey: a corrupted IndexTxn on an encrypted
// store rebuilds from ciphertext blocks, requiring the key at rebuild time.
func TestEncryptedRebuildRebuildsWithKey(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "enc-rebuild")
	db, err := Create(base, encOptions("k1"))
	require.NoError(t, err)
	buildTwoSnapshots(t, db)

	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	mid := committed[0].txnStart + (committed[0].txnEnd-committed[0].txnStart)/2
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55}, mid)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db2, err := Open(base, encOptions("k1"))
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	rec := db2.Stats().Recovery
	require.Equal(t, uint64(1), rec.SnapshotsRebuilt)
	row, err := db2.Get(ctx, 1, "users", 3, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-3", name)
}

// TestEncryptedMidFileCorruption applies the same mid-file rule to ciphertext
// regions: a broken structure before a valid footer is corruption, not a tail.
func TestEncryptedMidFileCorruption(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc-mid")
	db, err := Create(base, encOptions("k1"))
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	blkOff := committed[0].blocksStart
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("GARBAGE!"), blkOff)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = Open(base, encOptions("k1"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "mid-file corruption")
}
