package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

// 重建索引时 ItemOrdinal 必须是「记录在自身块内的序号」，不是快照内的累计计数。
// 写路径（writer.go 的 blk.rowsDir 下标）按此发布，读路径（RecordAt/PageFor、
// ReadBatch 的 (block, ordinal) 排序）按此解析；recovery.go 的块扫描重建曾跨块
// 累加计数，导致任何跨多个 rows 块的快照在 IndexTxn 损坏后重建出一张错位的索引：
// 打开「成功」，但行落在别的位置上——轻则 ErrNotFound/越界错误，重则静默读到
// 别的行的值。

// rebuildAfterTxnDamage 破坏快照 snap 自己的 IndexTxn 中段，然后重新打开（只读
// 参数与创建时一致，重建只在内存里发生）。返回重开后的 store。
func rebuildAfterTxnDamage(t *testing.T, base string, snap SnapshotID, opts Options) *Store {
	t.Helper()
	db, err := Open(base, opts)
	require.NoError(t, err)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	var found *committedSnapshot
	for i := range committed {
		if committed[i].snapshotID == uint64(snap) {
			found = &committed[i]
		}
	}
	require.NotNil(t, found, "snapshot %d must be committed", snap)
	require.Greater(t, found.txnEnd-found.txnStart, int64(16))
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	mid := found.txnStart + (found.txnEnd-found.txnStart)/2
	_, err = f.WriteAt([]byte{0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA, 0xAA}, mid)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	dbr, err := Open(base, opts)
	require.NoError(t, err, "a snapshot whose IndexTxn is damaged must still open")
	t.Cleanup(func() { dbr.Close() })
	require.True(t, dbr.Stats().Recovery.Performed, "open must report a rebuild")
	return dbr
}

// buildWriteOrderStore 按给定的 RowID 顺序写一个多块 FULL 快照。
func buildWriteOrderStore(t *testing.T, order []RowID) (string, SnapshotID) {
	t.Helper()
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{BlockSize: 512})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}))
	for _, id := range order {
		require.NoError(t, tx.Insert("t", id, Row{Uint64(uint64(id)), String("row-" + itoa(int(id)))}))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	// 必须真的跨多个 rows 块，否则本用例无意义。
	blks, err := db.Blocks(ctx, snap, "t")
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(blks), 3, "need a multi-block snapshot")
	total := 0
	for _, b := range blks {
		require.Less(t, int(b.ItemCount), len(order), "each block must hold fewer records than the snapshot")
		total += int(b.ItemCount)
	}
	require.Equal(t, len(order), total)
	require.NoError(t, db.Close())
	return base, snap
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// checkEveryRow 逐行 Get + 全表 Scan + ReadBatch，校验值、顺序与完整性。
func checkEveryRow(t *testing.T, db *Store, snap SnapshotID, want []RowID) {
	t.Helper()
	ctx := context.Background()
	// 逐行 Get 按升序检查，与写入顺序无关。
	for _, id := range want {
		row, err := db.Get(ctx, snap, "t", id, nil)
		require.NoError(t, err, "Get(%d) on a rebuilt index must resolve the right record", id)
		v, ok := row[0].Uint64()
		require.True(t, ok)
		require.Equal(t, uint64(id), v, "row %d must carry its own id", id)
		s, _ := row[1].String()
		require.Equal(t, "row-"+itoa(int(id)), s, "row %d must carry its own value", id)
	}
	it, err := db.Scan(ctx, snap, "t", ScanOptions{})
	require.NoError(t, err)
	var got []RowID
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, it.RowID())
	}
	require.NoError(t, it.Err())
	it.Close()
	wantSorted := slices.Clone(want)
	slices.SortFunc(wantSorted, func(a, b RowID) int { return int(a) - int(b) })
	require.Equal(t, wantSorted, got, "rebuilt scan must emit every row once in RowID order")

	rows, err := db.ReadBatch(ctx, snap, "t", want)
	require.NoError(t, err)
	for k, id := range want {
		v, _ := rows[k][0].Uint64()
		require.Equal(t, uint64(id), v, "ReadBatch slot %d", k)
	}
	_, err = db.Verify(ctx, VerifyFull)
	require.NoError(t, err, "a rebuilt store must verify clean")
}

// TestRebuildMultiBlockAscending: 升序写入、跨多块的 FULL 快照，IndexTxn 损坏后
// 重建，每一行都必须按自己的块内序号解析。
func TestRebuildMultiBlockAscending(t *testing.T) {
	ids := make([]RowID, 0, 200)
	for i := RowID(1); i <= 200; i++ {
		ids = append(ids, i)
	}
	base, snap := buildWriteOrderStore(t, ids)
	db := rebuildAfterTxnDamage(t, base, snap, Options{BlockSize: 512})
	checkEveryRow(t, db, snap, ids)
}

// TestRebuildMultiBlockDescending: 降序写入（块的物理记录序与 RowID 序相反）时，
// 重建出的索引既要对得上块内序号，也要恢复出升序的行迭代。
func TestRebuildMultiBlockDescending(t *testing.T) {
	ids := make([]RowID, 0, 200)
	for i := RowID(200); i >= 1; i-- {
		ids = append(ids, i)
	}
	base, snap := buildWriteOrderStore(t, ids)
	db := rebuildAfterTxnDamage(t, base, snap, Options{BlockSize: 512})
	checkEveryRow(t, db, snap, ids)
}

// TestRebuildMultiBlockInterleaved: 打散写入顺序（含重复访问同一 RowID 的
// Update/Delete），重建后 DELTA 的墓碑与更新值都必须解析到正确位置。
func TestRebuildMultiBlockInterleaved(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{BlockSize: 512})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}))
	for i := 1; i <= 120; i++ {
		require.NoError(t, tx.Insert("t", RowID(i), Row{Uint64(uint64(i)), String("row-" + itoa(i))}))
	}
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	tx2, err := db.Begin(ctx, full)
	require.NoError(t, err)
	var shuffled []RowID
	for i := 115; i >= 1; i -= 3 { // 降序 + 步长：物理顺序与 RowID 顺序无关
		shuffled = append(shuffled, RowID(i))
	}
	for _, id := range shuffled {
		require.NoError(t, tx2.Update("t", id, Row{Uint64(uint64(id)), String("upd-" + itoa(int(id)))}))
	}
	// v1 禁止同一快照内重复 RowKey，因此删除集与更新集必须不相交：更新的是
	// ≡1 (mod 3) 的 id，这里删 ≡2 (mod 3) 的 id。
	for id := 110; id >= 2; id -= 6 {
		require.NoError(t, tx2.Delete("t", RowID(id)))
	}
	delta, err := tx2.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db2 := rebuildAfterTxnDamage(t, base, delta, Options{BlockSize: 512})
	deleted := make(map[RowID]bool)
	for id := 110; id >= 2; id -= 6 {
		deleted[RowID(id)] = true
	}
	updated := make(map[RowID]bool)
	for _, id := range shuffled {
		updated[id] = true
	}
	for id := RowID(1); id <= 120; id++ {
		row, err := db2.Get(ctx, delta, "t", id, nil)
		if deleted[id] {
			require.ErrorIs(t, err, ErrNotFound, "row %d was deleted in the delta", id)
			continue
		}
		require.NoError(t, err, "row %d", id)
		s, _ := row[1].String()
		if updated[id] {
			require.Equal(t, "upd-"+itoa(int(id)), s, "row %d must read the delta's value", id)
		} else {
			require.Equal(t, "row-"+itoa(int(id)), s, "row %d must read the parent's value", id)
		}
	}
	_, err = db2.Verify(ctx, VerifyFull)
	require.NoError(t, err)
}
