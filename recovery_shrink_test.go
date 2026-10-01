package rowpack

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// recovery_shrink_test.go 覆盖恢复遍历里所有「被守卫保护着的读」:文件长度只在
// 打开时读一次并由 Appender 缓存,遍历的长度守卫拿的是这个缓存值,而读本身落到文件
// 上。因此在一个已打开的 store 下把文件截短,是唯一能让「守卫通过、读失败」的办
// 法——每一处都必须变成错误,而不是「这里没有快照」。同一手法也覆盖尾部扫描:读不到
// 的尾部是「可恢复的尾巴」,而不是「文件中间损坏」。
//
// 剩下 13 个块里,rebuildIndex 的四处读(422/426/491/495)读的是遍历刚刚校验过的同
// 一批字节,遍历通过则它们必然通过;readIndexTxn 的三处(88/182/189)同理:txn 的
// 长度与头部都由遍历先校验过;AddMetadata/AddRow(521/526)与 Build(534)需要块内
// 数据自相矛盾(重复 RowID、不可打包的 change type),而 SetSnapshot(512)需要的非
// 法快照类型在遍历解析快照头时就被拒了;Truncate(129)无法在已打开的读写 fd 上稳定
// 制造失败。

// shrinkUnder truncates the data file behind the store's back: the appender
// keeps its cached length, so the next read past the new end fails while every
// length guard still passes.
func shrinkUnder(t *testing.T, db *Store, keep int64) {
	t.Helper()
	require.NoError(t, os.Truncate(db.dataPath, keep))
}

// patchMetaBlockAt rewrites one metadata block through patch, restamping the
// block CRC so the file's integrity shell stays valid.
func patchMetaBlockAt(t *testing.T, path string, off int64, patch func(mb *metaBlock)) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	mb := loadPatchableMetaBlock(t, f, off)
	patch(mb)
	mb.write(t)
}

// armCommittedPlainStore returns a store whose blocks are stored uncompressed,
// so the metadata block can be rewritten in place.
func armCommittedPlainStore(t *testing.T) *Store {
	t.Helper()
	db, err := Create(tmpdb(t), Options{Compression: CompressionNone})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(context.Background())
	require.NoError(t, err)
	return db
}

// firstCommitted walks the first snapshot of an intact store to learn where its
// structures sit.
func firstCommitted(t *testing.T, db *Store) committedSnapshot {
	t.Helper()
	c, complete, _, err := db.walkSnapshot(int64(format.DataFileHeaderSize))
	require.NoError(t, err)
	require.True(t, complete, "the fixture store owns one committed snapshot")
	return c
}

// TestRecoverRejectsShrunkFile pins one property for every guarded read of the
// recovery walk: a read that cannot be satisfied is an error, never a silent
// "no snapshot here".
func TestRecoverRejectsShrunkFile(t *testing.T) {
	cases := []struct {
		name string
		keep func(c committedSnapshot) int64
	}{
		{"snapshot header", func(c committedSnapshot) int64 { return c.start + 1 }},
		{"block magic", func(c committedSnapshot) int64 { return c.blocksStart + 4 }},
		{"block header", func(c committedSnapshot) int64 { return c.blocksStart + format.BlockHeaderSize/2 }},
		{"txn header", func(c committedSnapshot) int64 { return c.txnStart + format.IndexTxnHeaderSize/2 }},
		{"txn footer", func(c committedSnapshot) int64 { return c.txnEnd - format.IndexTxnFooterSize + 4 }},
		// Past the magic but short of the whole footer: the magic read must
		// succeed so the footer read itself is the one that fails.
		{"snapshot footer", func(c committedSnapshot) int64 { return c.footerOff + format.SnapshotFooterSize/2 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := armCommittedStore(t)
			c := firstCommitted(t, db)
			shrinkUnder(t, db, tc.keep(c))
			require.Error(t, db.recover(), "a read past the end of the file must not be read as absent data")
		})
	}
}

// TestRecoverTailScanRejectsShrunkFile: the tail/mid-file discriminator scans
// for a later footer. If that scan cannot read, the region is a recoverable
// tail — the opposite answer (mid-file corruption) would refuse to open a store
// whose only fault is that the file ended.
func TestRecoverTailScanRejectsShrunkFile(t *testing.T) {
	t.Run("no footer magic reachable", func(t *testing.T) {
		db := armCommittedStore(t)
		c := firstCommitted(t, db)
		// Break the snapshot header so the walk stops there without an error,
		// then end the file a little later: the footer scan runs off the end.
		writeFileSpan(t, db.Path(), c.start, []byte("not-a-snapshot-header"))
		shrinkUnder(t, db, c.start+int64(format.SnapshotHeaderSize)+8)

		require.NoError(t, db.recover())
		stats, ok := db.recoveryStats.Load().(recoveryReport)
		require.True(t, ok)
		require.True(t, stats.performed)
		require.NotZero(t, stats.dataTailIgnored, "the unreadable region is dropped as a tail")
	})

	t.Run("footer magic cut short", func(t *testing.T) {
		db := armCommittedStore(t)
		c := firstCommitted(t, db)
		// Stop the walk at the snapshot header, plant the footer magic just
		// behind it and end the file inside that footer: the scan finds the
		// magic but cannot read the footer behind it.
		writeFileSpan(t, db.Path(), c.start, []byte("not-a-snapshot-header"))
		at := c.start + int64(format.SnapshotHeaderSize) + 8
		writeFileSpan(t, db.Path(), at, []byte(format.MagicSnapshotFtr))
		shrinkUnder(t, db, at+20)

		require.NoError(t, db.recover())
		stats, ok := db.recoveryStats.Load().(recoveryReport)
		require.True(t, ok)
		require.True(t, stats.performed)
		require.NotZero(t, stats.dataTailIgnored, "a footer that cannot be read is not a commit point")
	})
}

// TestRecoverRejectsUnparsableMetadataOnRebuild: with the IndexTxn unusable,
// recovery rebuilds from the blocks — and a metadata block that does not parse
// is then a hard failure, not a table that quietly disappears.
func TestRecoverRejectsUnparsableMetadataOnRebuild(t *testing.T) {
	db := armCommittedPlainStore(t)
	c := firstCommitted(t, db)
	path, base := db.dataPath, db.Path()
	require.NoError(t, db.Close())

	// 1. Break the metadata payload, keeping the block CRC-consistent.
	patchMetaBlockAt(t, path, c.blocksStart, func(mb *metaBlock) {
		mb.payload[0] ^= 0xFF
	})
	// 2. Break the IndexTxn: one flipped body byte fails the footer-bound CRC,
	// so the txn is rejected and the rebuild path runs.
	writeFileSpan(t, base, c.txnStart+int64(format.IndexTxnHeaderSize)+8, []byte{0xAA})

	// Reopened: the block cache is cold, so the rebuild reads the broken block.
	_, err := Open(base, Options{Compression: CompressionNone})
	require.ErrorContains(t, err, "rebuild snapshot")
}

// TestRecoverRejectsForeignBlockOnRebuild: a rebuilt index is validated by the
// same builder the commit path uses, so a block claiming another snapshot cannot
// be folded into this snapshot's index.
func TestRecoverRejectsForeignBlockOnRebuild(t *testing.T) {
	db := armCommittedPlainStore(t)
	c := firstCommitted(t, db)
	path, base := db.dataPath, db.Path()
	require.NoError(t, db.Close())

	patchMetaBlockAt(t, path, c.blocksStart, func(mb *metaBlock) {
		mb.hdr.SnapshotID = mb.hdr.SnapshotID + 1000
	})
	writeFileSpan(t, base, c.txnStart+int64(format.IndexTxnHeaderSize)+8, []byte{0xAA})

	_, err := Open(base, Options{Compression: CompressionNone})
	require.ErrorContains(t, err, "rebuild snapshot")
	require.ErrorContains(t, err, "snapshot mismatch")
}

// TestRecoverIgnoresFooterTxnOffsets: the footer's txn offsets are advisory —
// the walk derives the txn extent from the txn header itself (its BodyBytes),
// so a forged footer range cannot push recovery into reading outside the file.
func TestRecoverIgnoresFooterTxnOffsets(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	c := firstCommitted(t, db)

	f, err := os.OpenFile(db.dataPath, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	var fb [format.SnapshotFooterSize]byte
	_, err = f.ReadAt(fb[:], c.footerOff)
	require.NoError(t, err)
	var ftr format.SnapshotFooter
	require.NoError(t, ftr.Unmarshal(fb[:]))
	require.Greater(t, ftr.IndexTxnEndOffset, ftr.IndexTxnStartOffset)
	ftr.IndexTxnEndOffset += uint64(db.data.Size()) // far past the end of the file
	ftr.IndexTxnStartOffset = 0
	require.NoError(t, ftr.MarshalTo(fb[:])) // recomputes the footer CRC
	_, err = f.WriteAt(fb[:], c.footerOff)
	require.NoError(t, err)

	require.NoError(t, db.recover())
	snaps, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1, "the snapshot is still committed; the footer range did not move it")
}
