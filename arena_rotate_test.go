package rowpack

import (
	"context"
	"math/rand"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// 超大字符串 (> iterArenaChunkSize=32KiB) 会强制 strArena 旋转 chunk：
// 旋转后先前视图必须仍有效(GC 保活)，且每个字符串内容正确、互不串扰。
func TestArenaRotateLargeStrings(t *testing.T) {
	ctx := context.Background()
	s, err := Create(t.TempDir()+"/s", Options{})
	require.NoError(t, err)
	defer s.Close()
	tx, err := s.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "a", Type: TypeString}}))

	big := make([]byte, 0, 128<<10)
	for i := range 128 << 10 {
		big = append(big, byte('a'+i%26))
	}
	want := map[RowID]string{}
	var ids []RowID
	// 大串夹在两个小串之间，迫使旋转后继续在同一 arena 写入。
	for i := 1; i <= 3; i++ {
		id := RowID(100 + i) // 与大串独占区间，避免与下面的小串冲突
		ids = append(ids, id)
		want[id] = string(big)
		if err := tx.Insert(ctx, "t", id, Row{String(want[id])}); err != nil {
			t.Fatal(err)
		}
	}
	small := "small-value"
	for i := 1; i <= 6; i++ {
		id := RowID(i)
		ids = append(ids, id)
		want[id] = small
		if err := tx.Insert(ctx, "t", id, Row{String(small)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Scan：每行字符串内容必须与写入一致（arena 旋转后视图仍有效）。
	it, err := s.Scan(ctx, 1, "t", ScanOptions{})
	require.NoError(t, err)
	n := 0
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		got, okv := row[0].String()
		require.True(t, okv)
		require.Equal(t, want[it.RowID()], got, "scan row %d (arena view must survive rotation)", it.RowID())
		// 保留视图，避免被 GC 提前回收掉测试对象本身。
		runtime.KeepAlive(row)
		n++
	}
	require.NoError(t, it.Err())
	require.Equal(t, len(want), n)
	it.Close()

	// ReadBatch 同场景（独立 arena）。
	rows, err := s.ReadBatch(ctx, 1, "t", ids)
	require.NoError(t, err)
	for k, id := range ids {
		got, _ := rows[k][0].String()
		require.Equal(t, want[id], got, "batch row %d", id)
	}

	// 随机内容交叉检验（扫描 vs 批量 vs 单行）。
	r := rand.New(rand.NewSource(1))
	strs := make([]string, 0, 20)
	rids := make([]RowID, 0, 20)
	tx2, err := s.Begin(ctx, 1)
	require.NoError(t, err)
	for i := range 20 {
		id := RowID(1000 + i)
		sz := 1 << uint(10+r.Intn(14)) // 1KiB..16MiB
		b := make([]byte, sz)
		r.Read(b)
		for j := range b {
			b[j] = byte('A' + b[j]%26)
		}
		rids = append(rids, id)
		strs = append(strs, string(b))
		require.NoError(t, tx2.Insert(ctx, "t", id, Row{String(strs[i])}))
	}
	if _, err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i, id := range rids {
		got, err := s.Get(ctx, 2, "t", id, nil)
		require.NoError(t, err)
		v, _ := got[0].String()
		require.Equal(t, strs[i], v, "get row %d (%d bytes)", id, len(strs[i]))
	}
}
