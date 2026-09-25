package inspect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
)

// inspect_damaged_arms_test.go 覆盖「库能打开、但读下去就坏」这组臂:块体被改了一个
// 字节,索引与块头都还自洽,所以 Open 与 recovery 都不碰它——只有真的去读数据的命令
// (verify 的逐块校验、dump 的迭代)会撞上。
//
// 到不了的四条:
//   - list 的 ListSnapshots(133)与 Tables(148)失败:表目录是打开时派生的,元数据块
//     读不出来时 Open 就失败了(见 TestUnreadableCatalogIsRejectedAtOpen),命令根本跑
//     不到那两行。
//   - dump 的 Scan 失败(204):地址是刚刚从 Tables 里确认存在的,Scan 不会立刻失败,
//     损坏只在迭代中冒出来,也就是 220 那条。
//   - Value 的默认分支(284):所有 rowpack.Type 都列在 switch 里,而 Value 的 type 字段
//     是私有的,构造不出一个类型不在其中的值。

// damagedCopy copies the golden store and flips one payload byte in the first
// (or last) block of kind, leaving every structure (header CRCs, index) intact.
// tableID selects rows blocks of one table; 0 means any.
func damagedCopy(t *testing.T, kind format.BlockKind, tableID uint32, last bool) string {
	t.Helper()
	raw, err := os.ReadFile(goldenBase(t, "full-delta-store") + ".rpk")
	require.NoError(t, err)

	var hits []int
	for off := format.DataFileHeaderSize; off+format.BlockHeaderSize <= len(raw); {
		if string(raw[off:off+8]) != format.MagicBlockHdr {
			off++
			continue
		}
		var bh format.BlockHeader
		if err := bh.Unmarshal(raw[off : off+format.BlockHeaderSize]); err != nil {
			off++
			continue
		}
		if bh.BlockKind == kind && (tableID == 0 || bh.TableID == tableID) {
			hits = append(hits, off)
		}
		off += format.BlockHeaderSize + int(bh.StoredSize)
	}
	require.NotEmpty(t, hits, "the golden store must own a %v block", kind)

	target := hits[0]
	if last {
		target = hits[len(hits)-1]
	}
	raw[target+format.BlockHeaderSize] ^= 0xFF // first payload byte

	base := filepath.Join(t.TempDir(), "damaged")
	require.NoError(t, os.WriteFile(base+".rpk", raw, 0o644))
	return base
}

// TestVerifyReportsDamagedBlock: verify reads every block, so a store that
// opens cleanly can still fail verification — and that is a failure exit, not
// an empty OK line.
func TestVerifyReportsDamagedBlock(t *testing.T) {
	base := damagedCopy(t, format.BlockKindRows, 1, false)

	out, errb, code := run(t, "verify", base)
	require.Equal(t, ExitFailure, code)
	require.NotContains(t, out, "OK ")
	require.Contains(t, errb, "verify:")
}

// TestDumpReportsDamagedBlock: dump streams rows, so a damaged block surfaces
// while iterating and must be reported instead of ending the scan early.
func TestDumpReportsDamagedBlock(t *testing.T) {
	base := damagedCopy(t, format.BlockKindRows, 1, false)

	out, errb, code := run(t, "dump", base, "1", "1")
	require.Equal(t, ExitFailure, code)
	require.Empty(t, out, "no row from a damaged block may be printed as valid")
	require.Contains(t, errb, "dump:")
}

// TestListReportsUnreadableCatalog: list prints each snapshot's tables. A
// snapshot whose catalog block no longer decodes is reported per snapshot — an
// inspect tool must not hide a broken store behind a shorter list.
// TestUnreadableCatalogIsRejectedAtOpen: a metadata block that no longer
// decodes is not something list can work around. The catalog is derived when
// the store opens, so the tool fails at open instead of listing a store whose
// tables it cannot name.
func TestUnreadableCatalogIsRejectedAtOpen(t *testing.T) {
	base := damagedCopy(t, format.BlockKindMetadata, 0, false)

	out, errb, code := run(t, "list", base)
	require.Equal(t, ExitFailure, code)
	require.Empty(t, out, "nothing is listed when the store cannot be opened")
	require.Contains(t, errb, "open:")
}
