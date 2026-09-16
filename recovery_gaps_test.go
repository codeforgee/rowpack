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

// 这些测试逐一触发 walkSnapshot 的中止判定分支（tail vs mid-file
// corruption），保证"可丢弃尾部"与"必须拒绝"的边界不会被误判。

// appendTail 在文件末尾追加字节，返回追加前长度。
func appendTail(t *testing.T, base string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(b, mustFileSize(t, f))
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func mustFileSize(t *testing.T, f *os.File) int64 {
	t.Helper()
	fi, err := f.Stat()
	require.NoError(t, err)
	return fi.Size()
}

// patchBlockHeaderStoredSize 修改一个块头的 StoredSize 并重算块头 CRC。
func patchBlockHeaderStoredSize(t *testing.T, base string, off int64, newStored uint32) {
	t.Helper()
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	var hb [format.BlockHeaderSize]byte
	_, err = f.ReadAt(hb[:], off)
	require.NoError(t, err)
	binary.LittleEndian.PutUint32(hb[44:], newStored)
	binary.LittleEndian.PutUint32(hb[52:], 0)
	c := format.CRC32C(hb[:])
	binary.LittleEndian.PutUint32(hb[52:], c)
	_, err = f.WriteAt(hb[:], off)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// TestWalkTailBranches：文件末尾追加各种"半截结构"，都必须被当作
// 可丢弃尾部静默截断，不影响已提交数据。
func TestWalkTailBranches(t *testing.T) {
	cases := []struct {
		name string
		tail []byte
	}{
		// size-cur < 8：魔法都读不全。
		{"truncated-magic", []byte{0x01, 0x02, 0x03, 0x04, 0x05}},
		// 未知结构 magic -> break。
		{"unknown-structure", append([]byte("XXXXXXXX"), make([]byte, 200)...)},
		// 块头 magic 正确但 CRC 损坏 -> broken block header。
		{"broken-block-header", append([]byte(format.MagicBlockHdr), make([]byte, 200)...)},
		// txn 头 magic 正确但 CRC 损坏。
		{"broken-txn-header", append([]byte(format.MagicIndexTxnHdr), make([]byte, 200)...)},
		// 合法 txn 头 + BodyBytes 巨大 -> 截断 txn。
		{"txn-body-overrun", nil}, // 特殊构造，见下
		// 合法 txn 头 + 过短的 footer。
		{"txn-footer-short", nil},  // 特殊构造，见下
		// 合法块头 + StoredSize 超文件 -> 截断 payload。
		{"block-payload-overrun", nil}, // 特殊构造，见下
	}
	_ = cases
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			base := filepath.Join(tmpdb(t), "walktail")
			db, err := Create(base, Options{BlockSize: 1024})
			require.NoError(t, err)
			buildTwoSnapshots(t, db)
			require.NoError(t, db.Close())

			// 每个尾部都以一个合法 SnapshotHeader 前缀开始，让 walk 进入
			// 块循环后再撞上畸形结构——否则会在"非 SnapshotHeader"处提前
			// 以 tail 退出，覆盖不到块/Txn/Footer 的分类分支。
			committed, _, err := scanCommitted(t, base)
			require.NoError(t, err)
			last := committed[len(committed)-1]
			var sh [format.SnapshotHeaderSize]byte
			fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
			require.NoError(t, err)
			_, err = fr.ReadAt(sh[:], last.start)
			require.NoError(t, err)
			require.NoError(t, fr.Close())
			// 换一个 ID，避免与已提交快照混淆（tail 内无 footer 跟随）。
			binary.LittleEndian.PutUint64(sh[16:], 9999)
			binary.LittleEndian.PutUint32(sh[88:], 0)
			crc9 := format.CRC32C(sh[:])
			binary.LittleEndian.PutUint32(sh[88:], crc9)

			switch c.name {
			case "block-payload-overrun":
				// 合法块头模板 + 巨大 StoredSize。
				var tmpl [format.BlockHeaderSize]byte
				fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
				require.NoError(t, err)
				_, err = fr.ReadAt(tmpl[:], int64(last.blocksStart))
				require.NoError(t, err)
				require.NoError(t, fr.Close())
				binary.LittleEndian.PutUint32(tmpl[44:], 1<<30)
				binary.LittleEndian.PutUint32(tmpl[52:], 0)
				crcc := format.CRC32C(tmpl[:])
				binary.LittleEndian.PutUint32(tmpl[52:], crcc)
				appendTail(t, base, append(sh[:], tmpl[:]...))
			case "txn-body-overrun", "txn-footer-short":
				// 合法 txn 头模板；BodyBytes 分别置巨大/0。
				var hdr [format.IndexTxnHeaderSize]byte
				fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
				require.NoError(t, err)
				_, err = fr.ReadAt(hdr[:], last.txnStart)
				require.NoError(t, err)
				require.NoError(t, fr.Close())
				if c.name == "txn-body-overrun" {
					binary.LittleEndian.PutUint64(hdr[64:], 1<<40)
				} else {
					binary.LittleEndian.PutUint64(hdr[64:], 0)
				}
				binary.LittleEndian.PutUint32(hdr[72:], 0)
				crc := format.CRC32C(hdr[:])
				binary.LittleEndian.PutUint32(hdr[72:], crc)
				tail := append(append([]byte{}, sh[:]...), hdr[:]...)
				if c.name == "txn-footer-short" {
					tail = append(tail, make([]byte, 40)...) // footer 短于 80B
				}
				appendTail(t, base, tail)
			default:
				// truncated-magic / unknown-structure / broken-* 都只需
				// 前缀 + 原始尾部。
				appendTail(t, base, append(sh[:], c.tail...))
			}

			db2, err := Open(base, Options{BlockSize: 1024})
			require.NoError(t, err)
			t.Cleanup(func() { db2.Close() })
			rep := db2.Stats().Recovery
			require.True(t, rep.Performed, "tail must be reported as repaired")
			require.Greater(t, rep.DataTailIgnored, uint64(0))
			// 两个快照仍完好。
			snaps, err := db2.ListSnapshots(ctx)
			require.NoError(t, err)
			require.Len(t, snaps, 2)
			_, err = db2.Get(ctx, 1, "users", 7, nil)
			require.NoError(t, err, "read after tail truncation")
		})
	}
}

func scanCommitted(t *testing.T, base string) ([]committedSnapshot, int64, error) {
	t.Helper()
	raw, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	db, err := Open(base, Options{BlockSize: 1024, ReadOnly: true})
	if err == nil {
		defer db.Close()
	}
	// 直接用文件字节打开的 store 不方便；改用手工读文件的方式：
	_ = raw
	// 为了复用 scanDataFile 的内部状态机，直接以读写方式 Open 一次。
	s, err := Open(base, Options{BlockSize: 1024, ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer s.Close()
	return s.scanDataFile()
}

// TestWalkTruncatedPayload：已提交快照内的块头 StoredSize 超出文件——该损坏
// 块之后存在合法 footer，Open 必须报 mid-file corruption，而不是把它当作
// 可丢弃尾部静默截断。
func TestWalkTruncatedPayload(t *testing.T) {
	base := filepath.Join(tmpdb(t), "walkpay")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	blkOff := committed[0].blocksStart
	require.NoError(t, db.Close())

	// StoredSize -> 2^30，远超文件；重算块头 CRC 使结构看似"合法"。
	patchBlockHeaderStoredSize(t, base, blkOff, 1<<30)

	_, err = Open(base, Options{BlockSize: 1024})
	require.Error(t, err, "mid-file corruption must fail the open")
	require.Contains(t, err.Error(), "mid-file corruption")
}

// TestWalkFooterIDMismatchRejected：篡改已提交 footer 的 SnapshotID 并重算
// footer CRC 后，Open 必须报 mid-file corruption——提交权威不能被当作
// 可丢弃尾部而静默截断。
func TestWalkFooterIDMismatchRejected(t *testing.T) {
	base := filepath.Join(tmpdb(t), "ftrmismatch")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	last := committed[len(committed)-1]
	ftrOff := last.footerOff
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	var fb [format.SnapshotFooterSize]byte
	_, err = f.ReadAt(fb[:], ftrOff)
	require.NoError(t, err)
	binary.LittleEndian.PutUint64(fb[16:], last.snapshotID+100) // SnapshotID
	binary.LittleEndian.PutUint32(fb[136:], 0)                   // FooterCRC32C 占位
	c := format.CRC32C(fb[:])
	binary.LittleEndian.PutUint32(fb[136:], c)
	_, err = f.WriteAt(fb[:], ftrOff)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = Open(base, Options{BlockSize: 1024})
	require.Error(t, err, "mismatched footer ID must fail, not truncate")
	require.Contains(t, err.Error(), "mid-file corruption")
}
