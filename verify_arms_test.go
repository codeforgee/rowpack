package rowpack

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
)

// verify_arms_test.go 覆盖校验器的「块/索引对不上」臂:已关闭、块自身的载荷解析不
// 了、以及块被标成 metadata 却装着 rows 载荷。后两条用白盒视图/补丁块构造:写入器
// 不会造出这种索引。scope/深度/地址解析三条契约也在本文件——它们共用同一批手工视图
// 构造器。
//
// 剩下四条不可达:「FULL 带着父快照」与「DELTA 的父快照不存在」在索引层 Apply 就被
// 拒了(快照进不了视图,校验器永远看不到);表名为空时的 continue(表记录一定有名字);
// 深度超过上限(正常路径由 Apply 挡住,手工视图可达);以及 Duration<=0 的兜底(只有时钟
// 粒度极粗的平台才会走到)。

// publishCraftedSnapshot 已移到 testutil_test.go（与 craftView / publishCraftedView
// 共用一份构造逻辑）。

// TestVerifyReportsClosed: Verify is a read path like any other.
func TestVerifyReportsClosed(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	require.NoError(t, db.Close())

	_, err := db.Verify(ctx, VerifyFull, VerifyScope{})
	require.ErrorIs(t, err, ErrClosed)
}

// TestVerifyRejectsUnloadableBlock: the block's own payload no longer parses,
// so VerifyFull fails on that block instead of reporting a healthy store.
func TestVerifyRejectsUnloadableBlock(t *testing.T) {
	ctx := context.Background()
	base, _ := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		// The block CRC is restamped, so only the container itself is wrong.
		pb.container[0] ^= 0xFF // RowsBlockHeader magic
	})

	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	_, err = db.Verify(ctx, VerifyFull, VerifyScope{})
	require.ErrorIs(t, err, ErrCorruptData)
}

// TestVerifyRejectsUnparsableMetadataPayload: the block itself is intact by CRC
// but its payload is not metadata. Patched after Open: Open would have refused
// the store, and Verify is the entry point that re-reads blocks from disk.
func TestVerifyRejectsUnparsableMetadataPayload(t *testing.T) {
	ctx := context.Background()
	base, _ := setupPlainMultiPageStore(t, 60)
	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	moff := firstMetadataBlock(t, db)
	mb := loadPatchableMetaBlock(t, f, moff)
	require.NotZero(t, mb.payload[0], "the payload opens with a magic")
	mb.payload[0] ^= 0xFF // block CRCs are restamped: only Parse can notice
	mb.write(t)

	// Re-point a metadata block entry at the patched bytes under a fresh block
	// id: the loader caches by block id, so the copy Open already validated
	// would otherwise be served again.
	publishCraftedSnapshot(t, db, 2, 1, format.SnapshotDelta, func(b *index.Builder) {
		require.NoError(t, b.AddBlock(format.BlockIndexEntry{
			BlockID:    999999,
			SnapshotID: 2,
			TableID:    1,
			BlockKind:  format.BlockKindMetadata,
			DataOffset: uint64(moff),
			RawSize:    64,
			StoredSize: 64,
		}))
	})

	_, err = db.Verify(ctx, VerifyFull, VerifyScope{})
	require.ErrorIs(t, err, ErrCorruptData)
}

// TestVerifyRejectsForeignMetadataPayload: a block registered as metadata that
// carries a rows payload parses as neither. VerifyFull reads metadata blocks,
// so it must reject the mismatch.
func TestVerifyRejectsForeignMetadataPayload(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	st, err := db.captureState()
	require.NoError(t, err)

	var rowsOffset uint64
	for _, bl := range st.view.Blocks() {
		if bl.Kind == format.BlockKindRows {
			rowsOffset = bl.DataOffset
			break
		}
	}
	require.NotZero(t, rowsOffset, "the fixture store has a rows block")

	publishCraftedSnapshot(t, db, 2, 1, format.SnapshotDelta, func(b *index.Builder) {
		require.NoError(t, b.AddBlock(format.BlockIndexEntry{
			BlockID:    123456,
			SnapshotID: 2,
			TableID:    1,
			BlockKind:  format.BlockKindMetadata, // claims metadata...
			DataOffset: rowsOffset,               // ...but points at rows bytes
			RawSize:    64,
			StoredSize: 64,
		}))
	})

	_, err = db.Verify(ctx, VerifyFull, VerifyScope{})
	require.ErrorIs(t, err, ErrCorruptData)
}

// ---- scope, chain depth and address resolution ----

// NOTE: the "FULL snapshot has a parent" and "DELTA parent missing" arms in
// Verify are unreachable through any supported path: View.beginApply rejects
// both at construction, and the crafted-view helpers funnel through Apply.
// They stay as defense in depth for views built by future recovery paths.

// TestVerifyRejectsChainBeyondDepthLimit: a chain deeper than the store's
// configured limit is corruption, even though the view itself applied under the
// format default.
func TestVerifyRejectsChainBeyondDepthLimit(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	publishCraftedView(t, db, 2, 1, nil)
	publishCraftedView(t, db, 3, 2, nil)
	// Shrink the store's limit after the fact; Limits are reader policy.
	db.opts.Limits.MaxSnapshotDepth = 1

	_, err := db.Verify(ctx, VerifyQuick, VerifyScope{})
	require.Error(t, err)
	var ce *CorruptionError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, ErrCorruptIndex, ce.Kind)
	require.Equal(t, "snapshot chain too deep", ce.Reason)
}

// TestVerifyScopeFilters: a snapshot-filtered verify skips other snapshots'
// blocks, and a table-filtered verify skips blocks of other tables. Both must
// still succeed on a healthy store.
func TestVerifyScopeFilters(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("kept", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.DefineTable("skipped", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.Insert(ctx, "kept", 1, Row{Int64(1)}))
	require.NoError(t, w.Insert(ctx, "skipped", 1, Row{Int64(1)}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)

	// Whole store: both tables checked.
	full, err := db.Verify(ctx, VerifyFull, VerifyScope{})
	require.NoError(t, err)
	blocksAll := full.BlocksChecked

	// Table filter drops the other table's rows blocks.
	byTable, err := db.Verify(ctx, VerifyFull, VerifyScope{Tables: []string{Qualify(NSUser, "kept")}})
	require.NoError(t, err)
	require.Less(t, byTable.BlocksChecked, blocksAll)

	// Snapshot filter on the only snapshot still checks it.
	bySnap, err := db.Verify(ctx, VerifyFull, VerifyScope{Snapshot: snap})
	require.NoError(t, err)
	require.Equal(t, blocksAll, bySnap.BlocksChecked)

	// A verify of a tiny store must still report a positive duration.
	require.Greater(t, full.Duration, time.Duration(0))
}

// TestVerifyScopeSkipsOtherSnapshotBlocks: the blocks a scoped verify counts
// partition the whole-store count — snapshots outside the scope contribute
// nothing and nothing is counted twice.
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

// TestVerifyAcceptsDistinctAddresses: a healthy store must not trigger the
// duplicate-address arm.
func TestVerifyAcceptsDistinctAddresses(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	_, err := db.Verify(ctx, VerifyQuick, VerifyScope{})
	require.NoError(t, err)
}

// TestVerifyAddressResolvesThroughOlderSnapshot: a FULL checkpoint that omits a
// table resets the chain — the newer snapshot does not name the older table any
// more, yet its rows block (visible through the older snapshot) must still
// resolve an address by walking back.
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

// TestVerifyUnknownTableAddress: a crafted index entry claiming a rows block
// for an unknown table id must not crash Verify — the address resolves to ""
// and the structural checks pass on the (real) block body.
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
