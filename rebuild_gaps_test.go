package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// TestRebuildFailsOnCorruptBlock：快照的 IndexTxn 与块载荷同时损坏时，
// Open 的重建路径必须失败（不能带着半残索引静默打开）。
func TestRebuildFailsOnCorruptBlock(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "dualcorrupt")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	require.Len(t, committed, 2)
	snap1 := committed[0]

	// 取 snapshot1 中真正含行数据的块（rows block），先记下偏移。
	st, err := db.captureState()
	require.NoError(t, err)
	loc, ok := st.view.ResolveRow(1, 1, 1)
	require.True(t, ok)
	rowsBlk := st.view.Block(loc.BlockID)
	require.NotNil(t, rowsBlk)
	blkPayloadOff := int64(rowsBlk.DataOffset) + format.BlockHeaderSize
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	// 1) txn 中部填 0xAA（触发 txn CRC 失败 -> rebuild）。
	mid := snap1.txnStart + (snap1.txnEnd-snap1.txnStart)/2
	_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
	require.NoError(t, err)
	// 2) rows 块首个 payload 字节翻转（rebuild 读块时 CRC/auth 失败）。
	payload := make([]byte, 32)
	_, err = f.ReadAt(payload, blkPayloadOff)
	require.NoError(t, err)
	payload[0] ^= 0xFF
	_, err = f.WriteAt(payload, blkPayloadOff)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	db2, err := Open(base, Options{BlockSize: 1024})
	require.Error(t, err, "dual corruption must fail the open")
	t.Cleanup(func() { _ = db2 })
	_ = ctx
}

// TestOpenTinyFile：数据文件小于固定头长度时 Open 必须报错。
func TestOpenTinyFile(t *testing.T) {
	base := filepath.Join(tmpdb(t), "tiny")
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.WriteFile(base+".rpk", make([]byte, 60), 0o644))
	_, err := Open(base, Options{})
	require.Error(t, err, "sub-header file must be rejected")
	// 恰好等于头长度但内容为垃圾：同样是拒绝（头穿插 CRC 校验）。
	require.NoError(t, os.WriteFile(base+".rpk", make([]byte, format.DataFileHeaderSize), 0o644))
	_, err = Open(base, Options{})
	require.Error(t, err, "garbage header must be rejected")
}
