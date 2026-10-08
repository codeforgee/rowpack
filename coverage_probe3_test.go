package rowpack

// Round-3 probes: API error arms that survived the first two passes
// (PeekHeader/Create/Open path resolution, post-Close accessors, SealTable
// misuse, SetMeta stored-size overflow, a negative cache budget, Verify
// scope/snapshot filtering and addressOfTable's unknown-table arms).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
)

// ---- PeekHeader: path resolution and short-file read ----

func TestPeekHeaderInvalidPath(t *testing.T) {
	_, err := PeekHeader("")
	require.ErrorIs(t, err, ErrInvalidPath)
	_, err = PeekHeader(".rpk")
	require.ErrorIs(t, err, ErrInvalidPath)
}

func TestPeekHeaderShortFile(t *testing.T) {
	base := filepath.Join(tmpdb(t), "short")
	require.NoError(t, os.WriteFile(base+".rpk", []byte("rowpack"), 0o644))
	_, err := PeekHeader(base)
	require.ErrorContains(t, err, "read store header")
}

// PeekHeader open failure that is neither success nor ErrNotFound: a path
// component in front of the store name is a regular file (ENOTDIR).
func TestPeekHeaderOpenFailureNotMissing(t *testing.T) {
	dir := tmpdb(t)
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))
	_, err := PeekHeader(filepath.Join(blocker, "store"))
	require.ErrorContains(t, err, "open store file")
	require.NotErrorIs(t, err, ErrNotFound)
}

// ---- Store.KeyID ----

func TestStoreKeyIDAccessor(t *testing.T) {
	ctx := context.Background()
	dir := tmpdb(t)

	enc := Options{Encryption: &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "kid-1", key: testKey("kid-1")}, KeyID: "kid-1"}}
	db, err := Create(filepath.Join(dir, "enc"), enc)
	require.NoError(t, err)
	require.Equal(t, "kid-1", db.KeyID())
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	_, err = w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(filepath.Join(dir, "enc"), Options{Encryption: &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "kid-1", key: testKey("kid-1")}, KeyID: "kid-1"}})
	require.NoError(t, err)
	require.Equal(t, "kid-1", reopened.KeyID())
	require.NoError(t, reopened.Close())

	plain, err := Create(filepath.Join(dir, "plain"), Options{})
	require.NoError(t, err)
	require.Equal(t, "", plain.KeyID())
	require.NoError(t, plain.Close())
}

// ---- Create/Open reject an empty base path (with or without extension) ----

func TestCreateOpenInvalidPath(t *testing.T) {
	for _, base := range []string{"", ".rpk", ".rpi"} {
		_, err := Create(base, Options{})
		require.ErrorIs(t, err, ErrInvalidPath, "Create(%q)", base)
		_, err = Open(base, Options{})
		require.ErrorIs(t, err, ErrInvalidPath, "Open(%q)", base)
	}
}

// ---- Post-Close accessors degrade to zero values / ErrClosed ----

func TestStatsAndMetaAfterClose(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.SetMeta([]byte("m")))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1)}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	st := db.Stats()
	require.Zero(t, st.Snapshots)
	require.Zero(t, st.Blocks)

	_, err = db.Meta(ctx, snap)
	require.ErrorIs(t, err, ErrClosed)
}

// ---- SealTable misuse ----

func TestSealTableArms(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))

	// Unknown table.
	require.ErrorIs(t, w.SealTable("nope"), ErrNotFound)
	// No pending rows: a no-op.
	require.NoError(t, w.SealTable("t"))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1)}))
	require.NoError(t, w.SealTable("t"))
	_, err = w.Commit(ctx)
	require.NoError(t, err)

	// The committed transaction rejects further sealing.
	require.ErrorIs(t, w.SealTable("t"), ErrSnapshotCommitted)
}

// ---- SetMeta whose stored (compressed) form exceeds the block limit ----

func TestSetMetaStoredOverflow(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{
		Compression: CompressionNone, // stored == raw: deterministic overflow
		Limits:      Limits{MaxStoredBlockBytes: 4096},
	})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	big := make([]byte, 8192)
	for i := range big {
		big[i] = byte(i)
	}
	require.NoError(t, w.SetMeta(big)) // raw within MaxRawBlockBytes
	_, err = w.Commit(ctx)
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorContains(t, err, "exceeds limit")
}

// ---- A negative cache budget disables both caches; reads must still work ----

func TestNegativeCacheBudget(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024, CacheBytes: -1, ScanCacheBytes: -1})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1)}))
	require.NoError(t, w.Insert(ctx, "t", 2, Row{Int64(2)}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	row, err := db.Get(ctx, snap, "t", 1, nil)
	require.NoError(t, err)
	v, ok := row[0].Int64()
	require.True(t, ok)
	require.Equal(t, int64(1), v)

	it, err := db.Scan(ctx, snap, "t", ScanOptions{})
	require.NoError(t, err)
	n := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		n++
	}
	require.NoError(t, it.Err())
	require.Equal(t, 2, n)
	require.NoError(t, it.Close())

	st := db.Stats()
	require.Zero(t, st.Cache.CapacityBytes)
	require.Zero(t, st.Cache.Hits)
	require.Zero(t, st.Cache.Loads)
}

// ---- Verify scope: blocks of snapshots outside the scope are skipped ----

func TestVerifyScopeSkipsOtherSnapshotBlocks(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})
	insertOne := func(parent SnapshotID, id uint64) SnapshotID {
		w, err := db.Begin(ctx, parent)
		require.NoError(t, err)
		if parent == NoParent {
			require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
		}
		require.NoError(t, w.Insert(ctx, "t", id, Row{Int64(1)}))
		snap, err := w.Commit(ctx)
		require.NoError(t, err)
		return snap
	}
	snap1 := insertOne(NoParent, 1)
	snap2 := insertOne(snap1, 2)

	blocksOf := func(scope SnapshotID) uint64 {
		rep, err := db.Verify(ctx, VerifyQuick, VerifyScope{Snapshot: scope})
		require.NoError(t, err)
		return rep.BlocksChecked
	}
	full := blocksOf(0)
	only1 := blocksOf(snap1)
	only2 := blocksOf(snap2)
	require.Greater(t, only1, uint64(0))
	require.Greater(t, only2, uint64(0))
	require.Equal(t, full, only1+only2)
}

// ---- addressOfTable walks back to a snapshot that still names the table ----

// A FULL checkpoint that omits a table resets the chain: the newer snapshot
// does not name the older table any more, yet its rows block (visible through
// the older snapshot) must still resolve an address by walking back.
func TestVerifyAddressResolvesThroughOlderSnapshot(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("kept", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.Insert(ctx, "kept", 1, Row{Int64(1)}))
	snap1, err := w.Commit(ctx)
	require.NoError(t, err)

	// FULL checkpoint (parent 0) that does not define "kept".
	cp, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, cp.SetMeta([]byte("checkpoint")))
	snap2, err := cp.Commit(ctx)
	require.NoError(t, err)
	require.NotEqual(t, snap1, snap2)

	rep, err := db.Verify(ctx, VerifyFull, VerifyScope{})
	require.NoError(t, err)
	require.Greater(t, rep.BlocksChecked, uint64(0))
}

// ---- addressOfTable: a rows block whose table no snapshot names ----

// A crafted index entry claiming a rows block for an unknown table id must not
// crash Verify: the address resolves to "" and the structural checks pass on
// the (real) block body.
func TestVerifyUnknownTableAddress(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	st, err := db.captureState()
	require.NoError(t, err)
	var real *index.BlockLoc
	for _, bl := range st.view.Blocks() {
		if bl.Kind == format.BlockKindRows {
			real = bl
			break
		}
	}
	require.NotNil(t, real, "the fixture store has a rows block")

	publishCraftedSnapshot(t, db, 2, 1, format.SnapshotDelta, func(b *index.Builder) {
		require.NoError(t, b.AddBlock(format.BlockIndexEntry{
			BlockID:    424242,
			SnapshotID: 2,
			TableID:    999, // no snapshot names this table
			BlockKind:  format.BlockKindRows,
			DataOffset: real.DataOffset,
			RawSize:    real.RawSize,
			StoredSize: real.StoredSize,
		}))
	})

	rep, err := db.Verify(ctx, VerifyQuick, VerifyScope{})
	require.NoError(t, err)
	require.Greater(t, rep.BlocksChecked, uint64(0))
}
