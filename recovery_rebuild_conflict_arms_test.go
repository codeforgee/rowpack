package rowpack

import (
	"encoding/binary"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// recovery_rebuild_conflict_arms_test.go 覆盖重建的「拼装出来的索引说不通」这组臂:一个
// 已提交快照的索引(IndexTxn)被破坏后要从它自己的块重建,重建喂给 index.Builder 的东西
// 必须自洽——快照条目的类型来自快照头(SetSnapshot 拒绝未知类型),同一个 (表,行) 在一份
// 索引里只能出现一次(AddRow 拒绝重复)。索引与块一起提交,真实文件里不会出现这两种
// 形状,只能手工拼装:改字段后重算结构自身的 CRC,让文件层面完全自洽,矛盾只由重建发现。
//
// 重建里剩下的臂都到不了:
//   - AddMetadata 的快照不匹配(521):条目与块条目的 SnapshotID 同源于块头,而 AddBlock
//     先跑,任何块头与快照头的不一致都在那里先报错,到不了 AddMetadata。
//   - Build 失败(534):重建不加密、不压缩(level 0),所有 MarshalTo 都写在精确大小的
//     buffer 上、AssembleTxn 只在有 key epoch 时才可能失败——没有失败点。
//   - 文件扫描比重建更早也更严:块头/快照头改坏或 stored size 越出文件尾时扫描先判
//     mid-file corruption(TestRebuildScanCatchesDamageFirst),txn 头坏了同理
//     (TestBrokenTxnHeaderIsMidFileCorruption)。
//   - 读 txn 的 ReadAt(182/88)与截断尾部(129)都是纯 I/O 失败:需要另一个进程在扫描
//     之后动文件,单进程测试造不出来。

// blockOffsetsOfKind walks a snapshot's block region and returns the offsets of
// every block of kind.
func blockOffsetsOfKind(t *testing.T, f *os.File, c committedSnapshot, kind format.BlockKind) []int64 {
	t.Helper()
	var offs []int64
	cur := c.blocksStart
	for cur+format.BlockHeaderSize <= c.blocksEnd {
		var hb [format.BlockHeaderSize]byte
		_, err := f.ReadAt(hb[:], cur)
		require.NoError(t, err)
		if format.BlockKind(hb[12]) == kind {
			offs = append(offs, cur)
		}
		cur += format.BlockHeaderSize + int64(binary.LittleEndian.Uint32(hb[44:]))
	}
	return offs
}

// patchBlockHeaderSnapshotID rewrites a block header's SnapshotID and restamps
// the header CRC, so the file stays internally consistent while the block now
// claims to belong to another snapshot.
func patchBlockHeaderSnapshotID(t *testing.T, f *os.File, off int64, snapshotID uint64) {
	t.Helper()
	var hb [format.BlockHeaderSize]byte
	_, err := f.ReadAt(hb[:], off)
	require.NoError(t, err)
	binary.LittleEndian.PutUint64(hb[24:], snapshotID)
	binary.LittleEndian.PutUint32(hb[52:], 0)
	binary.LittleEndian.PutUint32(hb[52:], format.CRC32C(hb[:]))
	_, err = f.WriteAt(hb[:], off)
	require.NoError(t, err)
}

// storedSizeAt returns a block header's StoredSize.
func storedSizeAt(t *testing.T, f *os.File, off int64) uint32 {
	t.Helper()
	var hb [format.BlockHeaderSize]byte
	_, err := f.ReadAt(hb[:], off)
	require.NoError(t, err)
	return binary.LittleEndian.Uint32(hb[44:])
}

// equalSizedRowsBlocks returns two rows blocks of the same stored size; cloning
// one over the other therefore keeps every later offset in the file.
func equalSizedRowsBlocks(t *testing.T, f *os.File, c committedSnapshot) (src, dst int64) {
	t.Helper()
	offs := blockOffsetsOfKind(t, f, c, format.BlockKindRows)
	require.GreaterOrEqual(t, len(offs), 2, "the snapshot must own two rows blocks")
	for i := 0; i+1 < len(offs); i++ {
		if storedSizeAt(t, f, offs[i]) == storedSizeAt(t, f, offs[i+1]) {
			return offs[i], offs[i+1]
		}
	}
	t.Fatal("no two rows blocks share a stored size")
	return 0, 0
}

// cloneBlockOver copies the whole block at src over dst, keeping dst's own
// BlockID: two blocks may carry the same rows — that is what a forged duplicate
// looks like — but never the same identity.
func cloneBlockOver(t *testing.T, f *os.File, src, dst int64) {
	t.Helper()
	size := int64(storedSizeAt(t, f, src))
	blk := make([]byte, format.BlockHeaderSize+size)
	_, err := f.ReadAt(blk, src)
	require.NoError(t, err)

	var dh [format.BlockHeaderSize]byte
	_, err = f.ReadAt(dh[:], dst)
	require.NoError(t, err)
	copy(blk[16:24], dh[16:24]) // dst keeps its own BlockID
	binary.LittleEndian.PutUint32(blk[52:], 0)
	binary.LittleEndian.PutUint32(blk[52:], format.CRC32C(blk[:format.BlockHeaderSize]))

	_, err = f.WriteAt(blk, dst)
	require.NoError(t, err)
}

// patchSnapshotHeaderType rewrites a snapshot header's type and restamps its CRC.
func patchSnapshotHeaderType(t *testing.T, f *os.File, off int64, typ uint8) {
	t.Helper()
	var hb [format.SnapshotHeaderSize]byte
	_, err := f.ReadAt(hb[:], off)
	require.NoError(t, err)
	hb[12] = typ
	binary.LittleEndian.PutUint32(hb[88:], 0)
	binary.LittleEndian.PutUint32(hb[88:], format.CRC32C(hb[:]))
	_, err = f.WriteAt(hb[:], off)
	require.NoError(t, err)
}

// TestRebuildRejectsBlockFromAnotherSnapshot: every entry a block contributes
// carries the block header's snapshot id, and the block entry itself is added
// first — so a block that claims another snapshot is refused before any of its
// rows or metadata could be. (This is also why the row/metadata "snapshot
// mismatch" arms have no input of their own.)
func TestRebuildRejectsBlockFromAnotherSnapshot(t *testing.T) {
	base, snap := setupPlainMultiPageStore(t, 60)

	_, err := reopenAfterRebuildDamage(t, base, snap, Options{Compression: CompressionNone},
		func(f *os.File, c committedSnapshot) {
			offs := blockOffsetsOfKind(t, f, c, format.BlockKindRows)
			require.NotEmpty(t, offs, "the snapshot must own a rows block")
			patchBlockHeaderSnapshotID(t, f, offs[0], c.snapshotID+1000)
		})
	require.ErrorContains(t, err, "rebuild snapshot")
	require.ErrorContains(t, err, "block entry snapshot mismatch")
}

// TestRebuildRejectsDuplicateRowAcrossBlocks: one snapshot, one (table, row).
// A second block carrying the same rows is not a second version of them — the
// rebuild refuses to publish an index that names one row twice, instead of
// letting the read path pick an arbitrary one.
func TestRebuildRejectsDuplicateRowAcrossBlocks(t *testing.T) {
	base, snap := setupPlainMultiPageStore(t, 200)

	_, err := reopenAfterRebuildDamage(t, base, snap, Options{Compression: CompressionNone},
		func(f *os.File, c committedSnapshot) {
			src, dst := equalSizedRowsBlocks(t, f, c)
			cloneBlockOver(t, f, src, dst)
		})
	require.ErrorContains(t, err, "rebuild snapshot")
	require.ErrorContains(t, err, "duplicate (table")
}

// TestRebuildRejectsUnusableSnapshotType: the snapshot entry is rebuilt from the
// snapshot header. A type the engine does not know (neither FULL nor DELTA)
// cannot be published, even though the file around it is consistent.
func TestRebuildRejectsUnusableSnapshotType(t *testing.T) {
	base, snap := setupPlainMultiPageStore(t, 60)

	_, err := reopenAfterRebuildDamage(t, base, snap, Options{Compression: CompressionNone},
		func(f *os.File, c committedSnapshot) {
			patchSnapshotHeaderType(t, f, c.start, 9)
		})
	require.ErrorContains(t, err, "rebuild snapshot")
	require.ErrorContains(t, err, "bad type")
}
