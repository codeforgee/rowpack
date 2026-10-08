package rowpack

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// TestMetaRoundTrip pins the whole contract: one opaque block per snapshot,
// published with the transaction and read back byte for byte, while rows and
// schemas keep behaving exactly as before.
func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	meta1 := []byte("app=rowpack-exporter\nversion=1.2.3\nbatch=2026-09-28T01:02:03Z\n")

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(10)}))
	require.NoError(t, tx.SetMeta(meta1))
	snap1, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap1)
	require.NoError(t, err)
	require.Equal(t, meta1, got)

	// Rows unaffected: the meta block is not a rows block and not a
	// metadata (schema) block.
	row, err := db.Get(ctx, snap1, "t", 1, nil)
	require.NoError(t, err)
	v, ok := row[0].Uint64()
	require.True(t, ok)
	require.Equal(t, uint64(10), v)
	blocks, err := db.Blocks(snap1, "t")
	require.NoError(t, err)
	require.Len(t, blocks, 1)
}

// TestMetaStoredCompressed pins the on-disk contract: SetMeta values are
// always block-compressed (no expansion fallback), and Meta() returns the
// decompressed bytes verbatim — both for a highly compressible value (stored
// shrinks) and an incompressible one (stored may exceed raw; still committed
// within the stored limit and round-trips byte for byte).
func TestMetaStoredCompressed(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	// Compressible: 420 KB of repetition must shrink on disk.
	big := bytes.Repeat([]byte("rowpack-meta-payload-"), 20000)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.SetMeta(big))
	snap1, err := tx.Commit(ctx)
	require.NoError(t, err)
	got, err := db.Meta(ctx, snap1)
	require.NoError(t, err)
	require.Equal(t, big, got)

	st, err := db.captureState()
	require.NoError(t, err)
	loc := metaBlockLoc(st.view, snap1)
	require.NotNil(t, loc)
	blk, err := db.loader.reader.ReadAtBlock(int64(loc.DataOffset))
	require.NoError(t, err)
	require.Equal(t, format.CompressionZstd, blk.Header.Compression, "meta must be stored compressed")
	require.Less(t, blk.Header.StoredSize, blk.Header.RawSize, "compressible meta must shrink on disk")

	// Incompressible: forced compression may expand, but must still commit
	// (within MaxStoredBlockBytes) and round-trip verbatim.
	raw := make([]byte, 1<<16)
	_, err = rand.Read(raw)
	require.NoError(t, err)
	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, tx2.SetMeta(raw))
	snap2, err := tx2.Commit(ctx)
	require.NoError(t, err)
	got2, err := db.Meta(ctx, snap2)
	require.NoError(t, err)
	require.Equal(t, raw, got2)
	st2, err := db.captureState()
	require.NoError(t, err)
	loc2 := metaBlockLoc(st2.view, snap2)
	blk2, err := db.loader.reader.ReadAtBlock(int64(loc2.DataOffset))
	require.NoError(t, err)
	require.Equal(t, format.CompressionZstd, blk2.Header.Compression,
		"incompressible meta must stay compressed, never fall back to plain")
}

// TestMetaParentChainResolution pins the per-snapshot inheritance rule: the
// nearest layer at or above the requested snapshot decides, so a DELTA that
// sets nothing keeps its parent's value visible and one that sets something
// replaces it for itself and its descendants.
func TestMetaParentChainResolution(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	metaV1 := []byte("v1")

	full, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, full.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, full.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, full.SetMeta(metaV1))
	snap1, err := full.Commit(ctx)
	require.NoError(t, err)

	// No SetMeta: inherits.
	delta, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, delta.Insert(ctx, "t", 2, Row{Uint64(2)}))
	snap2, err := delta.Commit(ctx)
	require.NoError(t, err)
	got, err := db.Meta(ctx, snap2)
	require.NoError(t, err)
	require.Equal(t, metaV1, got)

	// Overridden for this snapshot and everything below it.
	metaV3 := []byte("v3")
	delta2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, delta2.SetMeta(metaV3))
	snap3, err := delta2.Commit(ctx)
	require.NoError(t, err)
	got, err = db.Meta(ctx, snap3)
	require.NoError(t, err)
	require.Equal(t, metaV3, got)

	delta3, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, delta3.Delete(ctx, "t", 1))
	snap4, err := delta3.Commit(ctx)
	require.NoError(t, err)
	got, err = db.Meta(ctx, snap4)
	require.NoError(t, err)
	require.Equal(t, metaV3, got)

	// The older snapshot still resolves to its own value: nothing merged.
	got, err = db.Meta(ctx, snap1)
	require.NoError(t, err)
	require.Equal(t, metaV1, got)

	// A FULL resets the chain: with no meta of its own there is none.
	checkpoint, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, checkpoint.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, checkpoint.Insert(ctx, "t", 9, Row{Uint64(9)}))
	snap5, err := checkpoint.Commit(ctx)
	require.NoError(t, err)
	got, err = db.Meta(ctx, snap5)
	require.NoError(t, err)
	require.Nil(t, got, "a FULL snapshot does not inherit its predecessor's meta")
}

// TestMetaAbsentAndUnknownSnapshot separates the two "nothing" answers: an
// unknown snapshot is an error, a known snapshot without a meta is (nil, nil).
func TestMetaAbsentAndUnknownSnapshot(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Nil(t, got, "no meta is not an error")

	_, err = db.Meta(ctx, snap+4242)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestMetaOnlyFullSnapshotCommits pins that a meta alone makes a legal FULL
// snapshot: an annotation checkpoint is exactly what "one block per snapshot"
// is for, and it must not need a row.
func TestMetaOnlyFullSnapshotCommits(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	meta := []byte("checkpoint: nothing to report this cycle")

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.SetMeta(meta))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, meta, got)

	// The checkpoint is a real snapshot: a DELTA can extend it.
	next, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	snap2, err := next.Commit(ctx)
	require.NoError(t, err)
	got, err = db.Meta(ctx, snap2)
	require.NoError(t, err)
	require.Equal(t, meta, got)

	info, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, info, 2)
}

// TestMetaEmptyClears pins the clearing rule: nil or an empty slice undoes an
// earlier SetMeta, so a snapshot never carries an empty block and "cleared"
// and "never set" are the same observable state.
func TestMetaEmptyClears(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta([]byte("gone")))
	require.NoError(t, tx.SetMeta(nil))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Nil(t, got)

	// Same for an explicit empty slice.
	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, tx2.SetMeta([]byte("gone")))
	require.NoError(t, tx2.SetMeta([]byte{}))
	snap2, err := tx2.Commit(ctx)
	require.NoError(t, err)
	got, err = db.Meta(ctx, snap2)
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestMetaLastWriteWinsAndCopiesInput pins both taken-ownership rules: the
// last call before Commit publishes, and the value is snapshotted so the
// caller may reuse its buffer immediately.
func TestMetaLastWriteWinsAndCopiesInput(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	buf := []byte("first value")
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta(buf))
	copy(buf, "secondXXXXXX") // caller reuses the slice: SetMeta copied
	require.NoError(t, tx.SetMeta([]byte("second value")))
	require.NoError(t, tx.SetMeta([]byte("third value")))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, []byte("third value"), got)
}

// TestMetaOpaqueBytes pins that the block carries arbitrary bytes verbatim:
// NULs, invalid UTF-8 and 0xFF are all fine because nothing is parsed.
func TestMetaOpaqueBytes(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	want := []byte{0x00, 0xff, 0xfe, 0x00, 0x80, 'a', 0x00, 0x01, 0x02, 0x00}
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta(want))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, got), "got %x want %x", got, want)

	// The returned slice is owned by the caller: poisoning it must not
	// corrupt the decoded block cache for a later read.
	for i := range got {
		got[i] = 0xCC
	}
	again, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.True(t, bytes.Equal(want, again), "second read damaged: %x", again)
}

// TestMetaSurvivesReopen pins the persistence half of atomic publishing: the
// block is located by the recovered/ replayed index, not by the writing
// session.
func TestMetaSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "meta")
	meta := []byte(repeatString("log line\n", 8))

	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta(meta))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	defer reopened.Close()

	got, err := reopened.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, meta, got)
}

// TestMetaEncryptedRoundTrip pins that the meta block goes through the same
// sealing policy as every other whole-payload block: unreadable without the
// key, byte-exact with it.
func TestMetaEncryptedRoundTrip(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "meta-enc")
	meta := []byte("encrypted snapshot meta")

	db, err := Create(base, encOptions("meta-key"))
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta(meta))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, meta, got)
	require.NoError(t, db.Close())

	reopened, err := Open(base, encOptions("meta-key"))
	require.NoError(t, err)
	defer reopened.Close()
	got, err = reopened.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, meta, got)
}

// TestMetaRejectsOversized pins the only content rule the engine enforces:
// length. The value becomes one block payload, so the raw-block limit bounds
// it.
func TestMetaRejectsOversized(t *testing.T) {
	ctx := context.Background()
	const maxRaw = 1024
	opts := Options{
		BlockSize: 128,
		Limits: Limits{
			MaxRowBytes: 512, MaxRawBlockBytes: maxRaw, MaxStoredBlockBytes: maxRaw,
			MaxColumns: 4, MaxValueBytes: 256, MaxSnapshotDepth: 8,
		},
	}
	db := testDB(t, opts)

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.ErrorIs(t, tx.SetMeta(make([]byte, maxRaw+1)), ErrInvalidArgument)
	// Exactly at the limit is still accepted even though the bytes are
	// incompressible (the block falls back to uncompressed storage), and the
	// pending value survived the rejected attempt above.
	exact := make([]byte, maxRaw)
	for i := range exact {
		exact[i] = byte(i)
	}
	require.NoError(t, tx.SetMeta(exact))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, exact, got)
}

// TestMetaStateGuards pins that SetMeta follows the transaction lifecycle:
// only an open transaction accepts it.
func TestMetaStateGuards(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta([]byte("v")))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.ErrorIs(t, tx.SetMeta([]byte("late")), ErrSnapshotCommitted)

	rolled, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, rolled.Rollback())
	require.ErrorIs(t, rolled.SetMeta([]byte("late")), ErrSnapshotAborted)
}

// TestMetaDamagedBlockIsCorruption pins that the block is validated like any
// other: a flipped payload byte surfaces as ErrCorruptData, never as decoded
// garbage.
func TestMetaDamagedBlockIsCorruption(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "meta-corrupt")

	db, err := Create(base, Options{BlockSize: 1024, Compression: CompressionNone})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta([]byte("0123456789abcdef")))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	// Locate the block before closing, then damage it while no store holds
	// the file.
	loc := metaBlockLoc(db.state.Load().view, snap)
	require.NotNil(t, loc)
	require.Equal(t, format.BlockKindSnapshotMeta, loc.Kind)
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0o644)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{'X'}, int64(loc.DataOffset)+format.BlockHeaderSize+2)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	reopened, err := Open(base, Options{BlockSize: 1024, Compression: CompressionNone})
	require.NoError(t, err)
	defer reopened.Close()
	_, err = reopened.Meta(ctx, snap)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrCorruptData, "damaged meta block: %v", err)
}

// TestMetaSurvivesIndexTxnRebuild pins that the meta block is located through
// the index like every block: when recovery rebuilds a snapshot's IndexTxn in
// memory from its own blocks, the rebuilt index still finds it.
func TestMetaSurvivesIndexTxnRebuild(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "meta-rebuild")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)

	meta1 := []byte("meta of snapshot 1")
	full, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, full.DefineTable("users", []Column{{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}}))
	require.NoError(t, full.Insert(ctx, "users", 1, Row{Uint64(1), String("u1")}))
	require.NoError(t, full.SetMeta(meta1))
	snap1, err := full.Commit(ctx)
	require.NoError(t, err)

	meta2 := []byte("meta of snapshot 2")
	delta, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, delta.Insert(ctx, "users", 2, Row{Uint64(2), String("u2")}))
	require.NoError(t, delta.SetMeta(meta2))
	snap2, err := delta.Commit(ctx)
	require.NoError(t, err)

	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	require.NoError(t, db.Close())

	// Damage the middle of the first snapshot's IndexTxn: recovery must
	// rebuild that snapshot's index from its blocks alone.
	c := committed[0]
	mid := c.txnStart + (c.txnEnd-c.txnStart)/2
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	require.Equal(t, uint64(1), db2.Stats().Recovery.SnapshotsRebuilt)

	got, err := db2.Meta(ctx, snap1)
	require.NoError(t, err)
	require.Equal(t, meta1, got)
	got, err = db2.Meta(ctx, snap2)
	require.NoError(t, err)
	require.Equal(t, meta2, got)

	row, err := db2.Get(ctx, snap2, "users", 1, nil)
	require.NoError(t, err)
	name, ok := row[1].String()
	require.True(t, ok)
	require.Equal(t, "u1", name)
}

// TestMetaVerifyChecksTheBlock pins that the meta block enters the Verify walk:
// it is loaded and CRC-validated in quick mode too, with no special case.
func TestMetaVerifyChecksTheBlock(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeUint64}}))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))
	require.NoError(t, tx.SetMeta([]byte("verifiable")))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	for _, mode := range []VerifyMode{VerifyQuick, VerifyFull} {
		rep, err := db.Verify(ctx, mode, VerifyScope{})
		require.NoError(t, err)
		require.Equal(t, uint64(1), rep.SnapshotsChecked)
		// Rows block + schema metadata block + this snapshot's meta block.
		require.Equal(t, uint64(3), rep.BlocksChecked, "meta block must be verified too")
	}
	// The store is still readable afterwards.
	got, err := db.Meta(ctx, snap)
	require.NoError(t, err)
	require.Equal(t, []byte("verifiable"), got)
}

func repeatString(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}
