package rowpack

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/index"
)

// verify_arms_test.go 覆盖校验器的「块/索引对不上」臂:已关闭、块自身的载荷解析不
// 了、以及块被标成 metadata 却装着 rows 载荷。后两条用白盒视图/补丁块构造:写入器
// 不会造出这种索引。
//
// 剩下五条不可达:「FULL 带着父快照」与「DELTA 的父快照不存在」在索引层 Apply 就被
// 拒了(快照进不了视图,校验器永远看不到);表名为空时的 continue(表记录一定有名字);
// 深度超过上限(同样由 Apply 挡住);以及 Duration<=0 的兜底(只有时钟粒度极粗的平台
// 才会走到)。

// publishCraftedSnapshot 已移到 testutil_test.go（与 craftView / publishCraftedView
// 共用一份构造逻辑）。

// TestVerifyReportsClosed: Verify is a read path like any other.
func TestVerifyReportsClosed(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	require.NoError(t, db.Close())

	_, err := db.Verify(ctx, VerifyFull)
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

	_, err = db.Verify(ctx, VerifyFull)
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

	_, err = db.Verify(ctx, VerifyFull)
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

	_, err = db.Verify(ctx, VerifyFull)
	require.ErrorIs(t, err, ErrCorruptData)
}
