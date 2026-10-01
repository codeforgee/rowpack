package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Scan 的 ctx 契约：中途取消后 Next 必须返回 false 且 Err() 报告取消原因，
// Close 幂等；提前取消的 ctx 在首次 Next 即生效。
func TestScanCtxCancel(t *testing.T) {
	ctx := context.Background()
	s, err := Create(t.TempDir()+"/s", Options{})
	require.NoError(t, err)
	defer s.Close()
	tx, err := s.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	for i := 1; i <= 100; i++ {
		require.NoError(t, tx.Insert(ctx, "t", RowID(i), Row{Int64(int64(i))}))
	}
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	// 中途取消。
	cctx, cancel := context.WithCancel(ctx)
	it, err := s.Scan(cctx, 1, "t", ScanOptions{})
	require.NoError(t, err)
	n := 0
	for n < 10 {
		_, ok := it.Next()
		require.True(t, ok)
		n++
	}
	cancel()
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		n++
		require.Less(t, n, 1000, "cancel must terminate the scan")
	}
	require.Equal(t, context.Canceled, it.Err(), "cancel must surface via Err")
	require.NoError(t, it.Close())
	require.NoError(t, it.Close(), "Close idempotent")
	require.Equal(t, context.Canceled, it.Err())

	// 创建前已取消：首次 Next 即失败。
	done := make(chan struct{})
	close(done)
	cctx2, cancel2 := context.WithCancel(ctx)
	cancel2()
	it2, err := s.Scan(cctx2, 1, "t", ScanOptions{})
	require.NoError(t, err)
	_, ok := it2.Next()
	require.False(t, ok)
	require.Equal(t, context.Canceled, it2.Err())
	require.NoError(t, it2.Close())
	_ = done
}

// Scan 对不存在的快照必须报 ErrNotFound，而不是空结果。
func TestScanUnknownSnapshot(t *testing.T) {
	ctx := context.Background()
	s, err := Create(t.TempDir()+"/s", Options{})
	require.NoError(t, err)
	defer s.Close()
	tx, err := s.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	_, err = s.Scan(ctx, 999, "t", ScanOptions{})
	require.ErrorIs(t, err, ErrNotFound)
	// 不存在的表同理。
	_, err = s.Scan(ctx, 1, "missing-table", ScanOptions{})
	require.ErrorIs(t, err, ErrNotFound)
	// 非法范围。
	_, err = s.Scan(ctx, 1, "t", ScanOptions{Start: 5, End: 5})
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = s.Scan(ctx, 1, "t", ScanOptions{Start: 9, End: 5})
	require.ErrorIs(t, err, ErrInvalidArgument)
}
