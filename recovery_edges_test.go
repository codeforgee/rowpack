package rowpack

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// TestSnapshotWithoutIndexTxn：一个"块 + Footer 完整但 IndexTxn 段缺失"的
// 快照——walk 以 txnStart==0 完成（blocksEnd 直接取 footer 前），recover
// 走完整 in-memory 重建（覆盖 walkSnapshot 空 txn 分支与 rebuildIndex 的
// 块扫描路径）。
func TestSnapshotWithoutIndexTxn(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "notxn")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)

	// 记录 snapshot2（DELTA）的头与 footer 偏移，以及 snapshot1 的长度。
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	snap1, snap2 := committed[0], committed[1]
	require.NoError(t, db.Close())

	raw, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	// 新布局：原 FileHeader + snapshot1 全部 + snapshot2 头与块 + snapshot2
	// footer,去掉 snapshot2 的 IndexTxn 段。
	var out []byte
	out = append(out, raw[:format.DataFileHeaderSize]...)
	out = append(out, raw[format.DataFileHeaderSize:snap1.end]...)
	out = append(out, raw[snap2.start:snap2.txnStart]...)  // header + blocks（无 txn）
	out = append(out, raw[snap2.footerOff:snap2.end]...)   // footer

	rebuilt := filepath.Join(tmpdb(t), "notxn2")
	require.NoError(t, os.WriteFile(rebuilt+".rpk", out, 0o644))

	db2, err := Open(rebuilt, Options{BlockSize: 1024})
	require.NoError(t, err, "footer-authoritative snapshot without txn must rebuild")
	t.Cleanup(func() { db2.Close() })
	rec := db2.Stats().Recovery
	require.True(t, rec.Performed)
	require.Equal(t, uint64(1), rec.SnapshotsRebuilt)
	// snapshot2 未被 DELTA 触及的行读回旧值；被删除的行走 tombstone。
	row, err := db2.Get(ctx, 2, "users", 1, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-1", name)
	_, err = db2.Get(ctx, 2, "users", 5, nil)
	require.ErrorIs(t, err, ErrNotFound, "rebuilt DELTA tombstone must hide row 5")
	// 重建后整体结构仍通过完整性校验。
	_, err = db2.Verify(ctx, VerifyFull)
	require.NoError(t, err)
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 2)
}

// TestHasValidFooterAfterSkipsFakeFooterMagic：损坏尾区中嵌入
// "MagicSnapshotFtr + 坏 CRC" 伪结构时，hasValidFooterAfter 必须忽略它
// （Unmarshal 失败 continue），不能把伪结构误判为已提交提交标记。
func TestHasValidFooterAfterSkipsFakeFooterMagic(t *testing.T) {
	base := filepath.Join(tmpdb(t), "fakeftr")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	require.NoError(t, db.Close())

	// 尾部:合法 SnapshotHeader;随后是坏块头(触发 tail 判定);再放
	// "MagicSnapshotFtr + 144 字节垃圾"——伪 footer magic。
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	fi, err := f.Stat()
	require.NoError(t, err)
	var sh [format.SnapshotHeaderSize]byte
	committed, _, err := scanCommitted(t, base)
	require.NoError(t, err)
	last := committed[len(committed)-1]
	_, err = f.ReadAt(sh[:], last.start)
	require.NoError(t, err)
	binary.LittleEndian.PutUint64(sh[16:], 7777)
	binary.LittleEndian.PutUint32(sh[88:], 0)
	crc9 := format.CRC32C(sh[:])
	binary.LittleEndian.PutUint32(sh[88:], crc9)
	tail := append([]byte{}, sh[:]...)
	tail = append(tail, []byte(format.MagicBlockHdr)...)
	tail = append(tail, make([]byte, 200)...) // 坏块头(垃圾 CRC)
	tail = append(tail, []byte(format.MagicSnapshotFtr)...)
	tail = append(tail, make([]byte, format.SnapshotFooterSize-8)...) // 伪 footer,CRC 必然坏
	_, err = f.WriteAt(tail, fi.Size())
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err, "fake footer magic plus broken block header = truncatable tail")
	t.Cleanup(func() { db2.Close() })
	rep := db2.Stats().Recovery
	require.True(t, rep.Performed)
	require.Greater(t, rep.DataTailIgnored, uint64(0))
	require.Equal(t, uint64(0), rep.SnapshotsRebuilt)
}
