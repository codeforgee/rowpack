package rowpack

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// recovery_rebuild_arms_test.go 覆盖「索引丢了,从块重建」这条路的拒绝臂:块体里的某个
// 页坏了,重建枚举记录时读不下去——索引(IndexTxn)与块一起提交,所以这只能在破坏索引
// 之后、重建过程中暴露。
//
// 重建里其余的臂都到不了:文件扫描比重建更早、也更严格。把块头/快照头改坏、或让块的
// stored size 越出文件尾,扫描直接判成 mid-file corruption,重建根本不会跑(见
// TestRebuildScanCatchesDamageFirst);而 txn 越出文件尾、截断失败、txn CRC 与内容自相
// 矛盾、重建时行重复等,都只能靠手工拼装的文件构造,测试里不做。

// offsetsOfSnapshot returns the committed snapshot's byte ranges by scanning the
// data file through the real recovery code.
func offsetsOfSnapshot(t *testing.T, base string, snap SnapshotID, opts Options) committedSnapshot {
	t.Helper()
	db, err := Open(base, opts)
	require.NoError(t, err)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.NoError(t, db.Close())
	for _, c := range committed {
		if c.snapshotID == uint64(snap) {
			return c
		}
	}
	t.Fatalf("snapshot %d is not committed", snap)
	return committedSnapshot{}
}

// reopenAfterRebuildDamage damages the snapshot's IndexTxn (so a rebuild is
// forced) plus whatever damage adds, then reopens the store.
func reopenAfterRebuildDamage(t *testing.T, base string, snap SnapshotID, opts Options, damage func(f *os.File, c committedSnapshot)) (*Store, error) {
	t.Helper()
	c := offsetsOfSnapshot(t, base, snap, opts)

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	mid := c.txnStart + (c.txnEnd-c.txnStart)/2
	_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
	require.NoError(t, err)
	if damage != nil {
		damage(f, c)
	}
	require.NoError(t, f.Close())

	db, err := Open(base, opts)
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
	}
	return db, err
}

// TestRebuildRejectsCorruptRowsPage: the container is intact by CRC but one
// record in it is not, so the rebuild cannot enumerate the block's records — it
// must not publish a half-built index.
func TestRebuildRejectsCorruptRowsPage(t *testing.T) {
	base, snap := setupPlainMultiPageStore(t, 60)
	patchFirstBlock(t, base, func(t *testing.T, pb *patchableBlock) {
		rh := pageHeaderOf(t, pb, 0)
		streams := pb.pageStreams(0)
		cbStart := int(rh.RowIDsBytes) + int(rh.OffsetsBytes) + int(rh.SchemaRLEBytes)
		require.Less(t, cbStart, len(streams))
		streams[cbStart] |= 3 // record 0: an illegal packed change type
	})

	_, err := reopenAfterRebuildDamage(t, base, snap, Options{Compression: CompressionNone}, nil)
	require.ErrorContains(t, err, "rebuild snapshot")
}

// TestRebuildScanCatchesDamageFirst: damage the rebuild would trip over is
// reported by the file scan before any rebuild runs — a block header that no
// longer parses is mid-file corruption, not a failed rebuild.
func TestRebuildScanCatchesDamageFirst(t *testing.T) {
	base, snap := setupPlainMultiPageStore(t, 60)
	_, err := reopenAfterRebuildDamage(t, base, snap, Options{Compression: CompressionNone},
		func(f *os.File, c committedSnapshot) {
			_, err := f.WriteAt([]byte{0, 0, 0, 0}, c.blocksStart) // no block magic
			require.NoError(t, err)
		})
	require.ErrorContains(t, err, "mid-file corruption", "the scan rejects the file, so no rebuild runs")
}
