package rowpack

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 块扫描重建与 IndexTxn 重放是同一张索引的两份独立实现，二者必须产出逐位一致的
// 可读结果。这个差分测试把「所有可读出口」压成指纹，比对原始索引与重建索引：
// 序号错位、块 run 归属、墓碑解析、沿父链取值、多 ns 目录、FULL checkpoint 等
// 分歧都会在第一行差异上立刻显形。

// fpValue 把一个 Value 渲染成确定性文本（与 rowpack-inspect 的展示口径一致）。
func fpValue(v Value) string {
	if v.IsNull() {
		return "NULL"
	}
	switch v.Type() {
	case TypeBool:
		x, _ := v.Bool()
		return fmt.Sprintf("b:%v", x)
	case TypeInt8:
		x, _ := v.Int8()
		return fmt.Sprintf("i:%d", x)
	case TypeInt16:
		x, _ := v.Int16()
		return fmt.Sprintf("i:%d", x)
	case TypeInt32:
		x, _ := v.Int32()
		return fmt.Sprintf("i:%d", x)
	case TypeInt64:
		x, _ := v.Int64()
		return fmt.Sprintf("i:%d", x)
	case TypeUint8:
		x, _ := v.Uint8()
		return fmt.Sprintf("u:%d", x)
	case TypeUint16:
		x, _ := v.Uint16()
		return fmt.Sprintf("u:%d", x)
	case TypeUint32:
		x, _ := v.Uint32()
		return fmt.Sprintf("u:%d", x)
	case TypeUint64:
		x, _ := v.Uint64()
		return fmt.Sprintf("u:%d", x)
	case TypeFloat32:
		x, _ := v.Float32()
		return fmt.Sprintf("f:%v", x)
	case TypeFloat64:
		x, _ := v.Float64()
		return fmt.Sprintf("f:%v", x)
	case TypeString:
		x, _ := v.String()
		return "s:" + x
	case TypeBytes:
		x, _ := v.Bytes()
		return fmt.Sprintf("x:%x", x)
	case TypeDate:
		x, _ := v.Date()
		return "D:" + x.Time(time.UTC).Format("2006-01-02")
	case TypeTime:
		x, _ := v.Time()
		return "T:" + time.Unix(0, int64(x)).UTC().Format("15:04:05.000000000")
	case TypeDateTime:
		x, _ := v.DateTimeValue()
		return "F:" + x.UTC().Format(time.RFC3339Nano)
	case TypeDecimal:
		d, _ := v.Decimal()
		return fmt.Sprintf("d:%s/%d", d.Unscaled.String(), d.Scale)
	}
	return fmt.Sprintf("?:%d", v.Type())
}

// richColumns 覆盖全部值类型（含可空列），行宽保证快照必然跨多个 rows 块。
func richColumns() []Column {
	return []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "flag", Type: TypeBool, Nullable: true},
		{Name: "cnt", Type: TypeInt64, Nullable: true},
		{Name: "score", Type: TypeFloat64, Nullable: true},
		{Name: "name", Type: TypeString, Nullable: true},
		{Name: "blob", Type: TypeBytes, Nullable: true},
		{Name: "day", Type: TypeDate, Nullable: true},
		{Name: "clock", Type: TypeTime, Nullable: true},
		{Name: "seen", Type: TypeDateTime, Nullable: true},
		{Name: "amount", Type: TypeDecimal, Nullable: true, Scale: 2},
	}
}

// richRow 构造第 i 行的完整值。
func richRow(i int, tag string) Row {
	tod, err := NewTimeOfDay(i%24, i%60, i%60, i%1000)
	if err != nil {
		panic(err)
	}
	payload := strings.Repeat(fmt.Sprintf("%s%04d", tag, i), 4)
	return Row{
		Uint64(uint64(i)),
		Bool(i%2 == 0),
		Int64(int64(i) * -7),
		Float64(float64(i)/4 + 0.25),
		String(payload),
		Bytes([]byte(payload)),
		DateValue(NewDate(time.Date(2026, time.March, 1+(i%27), 0, 0, 0, 0, time.UTC))),
		TimeValue(tod),
		DateTime(time.Date(2026, time.January, 1+(i%28), i%24, i%60, 0, 0, time.UTC)),
		DecimalValue(Decimal{Unscaled: big.NewInt(int64(i)*1000 + 7), Scale: 2}),
	}
}

// buildRichStore 建 FULL → DELTA（降序更新/删除/新增）→ DELTA（新表）→ FULL
// checkpoint（重写全表，深度重置）四个快照，跨多 ns、多表、多块、含墓碑与 NULL。
func buildRichStore(t *testing.T, base string) []SnapshotID {
	t.Helper()
	ctx := context.Background()
	db, err := Create(base, Options{BlockSize: 2048})
	require.NoError(t, err)
	cols := richColumns()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("main", cols))
	require.NoError(t, tx.DefineTableIn("public", "audit", cols))
	for i := 1; i <= 120; i++ {
		require.NoError(t, tx.Insert(ctx, "main", RowID(i), richRow(i, "m")))
	}
	for i := 1; i <= 40; i++ {
		row := richRow(i, "a")
		if i%5 == 0 {
			row[4] = Null() // 可空列打洞
		}
		require.NoError(t, tx.Insert(ctx, "public.audit", RowID(i), row))
	}
	s1, err := tx.Commit(ctx)
	require.NoError(t, err)

	// DELTA 1：降序更新 + 隔步删除 + 新 id 插入（物理顺序与 RowID 顺序无关）。
	tx2, err := db.Begin(ctx, s1)
	require.NoError(t, err)
	for i := 110; i >= 1; i -= 3 {
		require.NoError(t, tx2.Update(ctx, "main", RowID(i), richRow(i, "u1")))
	}
	// v1 禁止同一快照内重复 RowKey：更新集是 ≡2 (mod 3) 的 id，删除集取 ≡0 (mod 3)。
	for i := 105; i >= 1; i -= 9 {
		require.NoError(t, tx2.Delete(ctx, "main", RowID(i)))
	}
	for i := 121; i <= 140; i++ {
		require.NoError(t, tx2.Insert(ctx, "main", RowID(i), richRow(i, "n1")))
	}
	for i := 39; i >= 20; i -= 2 {
		require.NoError(t, tx2.Update(ctx, "public.audit", RowID(i), richRow(i, "a1")))
	}
	s2, err := tx2.Commit(ctx)
	require.NoError(t, err)

	// DELTA 2：新表（只有这个快照携带它的目录记录）+ 继续删除。
	tx3, err := db.Begin(ctx, s2)
	require.NoError(t, err)
	require.NoError(t, tx3.DefineTableIn("ops", "misc", cols))
	for i := 200; i <= 215; i++ {
		require.NoError(t, tx3.Insert(ctx, "ops.misc", RowID(i), richRow(i, "o")))
	}
	for i := 140; i >= 130; i-- {
		require.NoError(t, tx3.Delete(ctx, "main", RowID(i)))
	}
	s3, err := tx3.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// FULL checkpoint：重新定义同样的表（写自己的元数据层）并重灌全表。
	db, err = Open(base, Options{BlockSize: 2048})
	require.NoError(t, err)
	tx4, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx4.DefineTable("main", cols))
	require.NoError(t, tx4.DefineTableIn("public", "audit", cols))
	require.NoError(t, tx4.DefineTableIn("ops", "misc", cols))
	for i := 1; i <= 129; i++ {
		require.NoError(t, tx4.Insert(ctx, "main", RowID(i), richRow(i, "ck")))
	}
	for i := 1; i <= 40; i++ {
		require.NoError(t, tx4.Insert(ctx, "public.audit", RowID(i), richRow(i, "ca")))
	}
	for i := 200; i <= 215; i++ {
		require.NoError(t, tx4.Insert(ctx, "ops.misc", RowID(i), richRow(i, "co")))
	}
	s4, err := tx4.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return []SnapshotID{s1, s2, s3, s4}
}

// fingerprintStore 读取所有公开出口并压成确定性文本。Recovery 报告被排除（重建
// 与原始路径的元信息本就不同），其余必须逐位一致。
func fingerprintStore(t *testing.T, db *Store) string {
	t.Helper()
	ctx := context.Background()
	var sb strings.Builder
	snaps, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	for _, sn := range snaps {
		fmt.Fprintf(&sb, "snapshot %d type=%d parent=%d changes=%d blocks=%d raw=%d stored=%d\n",
			sn.ID, sn.Type, sn.Parent, sn.ChangeCount, sn.BlockCount, sn.RawBytes, sn.StoredBytes)
		tables, err := db.Tables(ctx, sn.ID)
		require.NoError(t, err)
		require.NotEmpty(t, tables)
		names := make([]string, 0, len(tables))
		for _, tb := range tables {
			names = append(names, fmt.Sprintf("%d|%s|%s|%d", tb.ID, tb.NS, tb.Name, tb.LatestVersion))
		}
		sort.Strings(names)
		for _, n := range names {
			sb.WriteString("  table " + n + "\n")
		}
		for _, tb := range tables {
			addr := Qualify(tb.NS, tb.Name)
			// Blocks 只列本快照自己事务写出的块：DELTA 未触碰的表没有自己的块。
			blks, err := db.Blocks(sn.ID, addr)
			require.NoError(t, err)
			for _, b := range blks {
				fmt.Fprintf(&sb, "  blocks %s #%d items=%d min=%d max=%d raw=%d stored=%d\n",
					addr, b.BlockID, b.ItemCount, b.MinRowID, b.MaxRowID, b.RawBytes, b.StoredBytes)
			}
			// 合并视图 Scan。
			it, err := db.Scan(ctx, sn.ID, addr, ScanOptions{})
			require.NoError(t, err)
			merged := 0
			for {
				row, ok := it.Next()
				if !ok {
					break
				}
				merged++
				fmt.Fprintf(&sb, "  scan %s %d/%d:%s\n", addr, it.RowID(), it.ChangeType(), fpRow(row))
			}
			require.NoError(t, it.Err())
			it.Close()
			fmt.Fprintf(&sb, "  scancount %s %d\n", addr, merged)
			// 逐 id Get（命中出行，未命中计数）。
			var hits, missing int
			for id := RowID(1); id <= 220; id++ {
				row, err := db.Get(ctx, sn.ID, addr, id, nil)
				if err != nil {
					require.Truef(t, errors.Is(err, ErrNotFound), "Get(%s,%d): %v", addr, id, err)
					missing++
					continue
				}
				hits++
				fmt.Fprintf(&sb, "  get %s %d:%s\n", addr, id, fpRow(row))
			}
			fmt.Fprintf(&sb, "  getstats %s hits=%d missing=%d\n", addr, hits, missing)
			// Exists 与 Get 必须一致。
			var exists int
			for id := RowID(1); id <= 220; id++ {
				ok, err := db.Exists(ctx, sn.ID, addr, id)
				require.NoError(t, err)
				if ok {
					exists++
				}
			}
			fmt.Fprintf(&sb, "  exists %s %d\n", addr, exists)
			// 批量读：一次读全部可见 id（乱序 + 重复，考验 (block,ordinal) 排序）。
			visible, err := visibleIDs(ctx, db, sn.ID, addr)
			require.NoError(t, err)
			ids := append([]RowID(nil), visible...)
			for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
				ids[i], ids[j] = ids[j], ids[i]
			}
			ids = append(ids, visible...) // 重复项
			rows, err := db.ReadBatch(ctx, sn.ID, addr, ids)
			require.NoError(t, err)
			for k, id := range ids {
				fmt.Fprintf(&sb, "  batch %s %d:%s\n", addr, id, fpRow(rows[k]))
			}
			// 原始块流：物理顺序与墓碑都必须一致。
			if len(blks) > 0 {
				bit, err := db.ScanBlocks(ctx, sn.ID, addr, blks[0].BlockID, blks[len(blks)-1].BlockID+1)
				require.NoError(t, err)
				raw := 0
				for {
					row, ok := bit.Next()
					if !ok {
						break
					}
					raw++
					body := "<tombstone>"
					if row != nil {
						body = fpRow(row)
					}
					fmt.Fprintf(&sb, "  raw %s %d/%d:%s\n", addr, bit.RowID(), bit.ChangeType(), body)
				}
				require.NoError(t, bit.Err())
				bit.Close()
				fmt.Fprintf(&sb, "  rawcount %s %d\n", addr, raw)
			}
			// Schema 目录。
			sch, err := db.Schema(ctx, sn.ID, addr, tb.LatestVersion)
			require.NoError(t, err)
			coltxt := make([]string, 0, len(sch.Columns))
			for _, c := range sch.Columns {
				coltxt = append(coltxt, fmt.Sprintf("%s:%d:%v", c.Name, c.Type, c.Nullable))
			}
			fmt.Fprintf(&sb, "  schema %s v%d %s\n", addr, sch.Version, strings.Join(coltxt, ","))
		}
		_, err = db.Verify(ctx, VerifyFull, VerifyScope{})
		require.NoErrorf(t, err, "verify store at snapshot %d", sn.ID)
	}
	return sb.String()
}

func fpRow(row []Value) string {
	parts := make([]string, 0, len(row))
	for _, v := range row {
		parts = append(parts, fpValue(v))
	}
	return strings.Join(parts, " ")
}

func visibleIDs(ctx context.Context, db *Store, snap SnapshotID, addr string) ([]RowID, error) {
	it, err := db.Scan(ctx, snap, addr, ScanOptions{})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var ids []RowID
	for {
		if _, ok := it.Next(); !ok {
			break
		}
		ids = append(ids, it.RowID())
	}
	return ids, it.Err()
}

// copyStore 把 <srcBase>.rpk 复制到 dstDir 下，返回新的 base 路径。
func copyStore(t *testing.T, srcBase, dstDir string) string {
	t.Helper()
	data, err := os.ReadFile(srcBase + ".rpk")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dstDir, 0o755))
	dst := filepath.Join(dstDir, filepath.Base(srcBase))
	require.NoError(t, os.WriteFile(dst+".rpk", data, 0o644))
	return dst
}

// snapshotTxnExtents 返回每个已提交快照自己的 IndexTxn 区间。
func snapshotTxnExtents(t *testing.T, base string) map[SnapshotID][2]int64 {
	t.Helper()
	db, err := Open(base, Options{ReadOnly: true})
	require.NoError(t, err)
	defer db.Close()
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	out := make(map[SnapshotID][2]int64, len(committed))
	for _, c := range committed {
		out[SnapshotID(c.snapshotID)] = [2]int64{c.txnStart, c.txnEnd}
	}
	return out
}

// TestRebuiltIndexMatchesOriginalIndex：单快照重建、全链重建、首个快照重建三种
// 情况下，重建出的索引必须在每一个读出口上与原始索引完全一致。
func TestRebuiltIndexMatchesOriginalIndex(t *testing.T) {
	ctx := context.Background()
	dir := tmpdb(t)
	base := filepath.Join(dir, "rich")
	snapIDs := buildRichStore(t, base)
	require.Len(t, snapIDs, 4)

	db, err := Open(base, Options{ReadOnly: true})
	require.NoError(t, err)
	want := fingerprintStore(t, db)
	require.NotEmpty(t, want)
	// 前提：快照自己写出多个 rows 块（序号错位只在多块时显形）；未触碰的表在
	// DELTA 里没有自己的块，因此按「每个快照至少有一张表拥有块」+「全链至少有一张
	// 表跨多块」两条来钉。
	storeWideMaxBlocks := 0
	for _, sn := range snapIDs {
		tbls, err := db.Tables(ctx, sn)
		require.NoError(t, err)
		owning := 0
		for _, tb := range tbls {
			blks, err := db.Blocks(sn, Qualify(tb.NS, tb.Name))
			require.NoError(t, err)
			if len(blks) > 0 {
				owning++
			}
			if len(blks) > storeWideMaxBlocks {
				storeWideMaxBlocks = len(blks)
			}
		}
		require.Greater(t, owning, 0, "snapshot %d must own rows blocks for some table", sn)
	}
	require.Greater(t, storeWideMaxBlocks, 1, "some (snapshot,table) must span multiple rows blocks")
	require.False(t, db.Stats().Recovery.Performed, "pristine store must not rebuild")
	require.NoError(t, db.Close())

	extents := snapshotTxnExtents(t, base)
	sub := func(t *testing.T, name string, targets []SnapshotID) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			dst := copyStore(t, base, filepath.Join(dir, name))
			f, err := os.OpenFile(dst+".rpk", os.O_RDWR, 0)
			require.NoError(t, err)
			for _, sn := range targets {
				ex, ok := extents[sn]
				require.True(t, ok, "snapshot %d must be committed", sn)
				require.Greater(t, ex[1]-ex[0], int64(16))
				mid := ex[0] + (ex[1]-ex[0])/2
				_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
				require.NoError(t, err)
			}
			require.NoError(t, f.Close())

			dbr, err := Open(dst, Options{ReadOnly: true})
			require.NoError(t, err, "damaging the IndexTxn must still open (rebuild in memory)")
			defer dbr.Close()
			rec := dbr.Stats().Recovery
			require.True(t, rec.Performed, "open must report a rebuild")
			require.GreaterOrEqual(t, rec.SnapshotsRebuilt, uint64(len(targets)),
				"every damaged snapshot must be rebuilt")
			assertFingerprintEqual(t, want, fingerprintStore(t, dbr))
		})
	}
	sub(t, "last", []SnapshotID{snapIDs[len(snapIDs)-1]})
	sub(t, "first", []SnapshotID{snapIDs[0]})
	sub(t, "whole-chain", snapIDs)
}

// assertFingerprintEqual 只打印第一处差异，避免整段文本糊满输出。
func assertFingerprintEqual(t *testing.T, want, got string) {
	t.Helper()
	if want == got {
		return
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	n := min(len(wl), len(gl))
	for i := 0; i < n; i++ {
		if wl[i] != gl[i] {
			t.Fatalf("fingerprint differs at line %d (of %d vs %d):\n  want: %s\n  got:  %s\n  ctx:  %s",
				i+1, len(wl), len(gl), wl[i], gl[i], firstNonEmpty([]string{wl[i-1], ""}))
		}
	}
	t.Fatalf("fingerprint length differs at line %d (of %d vs %d): want %q got %q",
		n+1, len(wl), len(gl), trimLine(wl, n), trimLine(gl, n))
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func trimLine(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return "<eof>"
}
