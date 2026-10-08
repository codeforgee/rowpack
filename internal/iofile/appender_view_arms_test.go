//go:build unix

package iofile

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// appender_view_arms_test.go 覆盖 readMapper 的两条兜底臂:
//
//   - 映射建不出来时永久降级(mmap_unix.go 73):mmap 失败不是致命错误——把 disabled
//     置上,改用 ReadAt 副本,后续视图都走这条路,而不是每次读都失败一次。
//   - 两个视图撞上同一次重映射(mmap_unix.go 53):先拿到写锁的那个完成重映射,另一个
//     拿到锁时范围已经被新映射覆盖,于是回到读路径重取,而不是再映射一遍。
//
// 其余的臂都是纯 I/O 失败,包内造不出来:OpenAppender 打开成功之后 Stat 失败、WriteAt
// 短写(只有出错才会短写)、CreateSingle 的写头/Sync/Close 失败、以及 fallbackView 的
// ReadAt 失败(61 行已经先挡住了越界范围)。

// TestViewDegradesWhenMmapIsImpossible: a file the platform refuses to map is
// not a read error — the mapper latches the ReadAt fallback and keeps serving.
func TestViewDegradesWhenMmapIsImpossible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unmappable.bin")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	require.NoError(t, err)
	// Sparse and far past what the platform will map in one go: the mapping
	// reservation fails, the read itself does not.
	const unmappable = int64(1) << 47
	if err := f.Truncate(unmappable); err != nil {
		f.Close()
		t.Skipf("this filesystem cannot hold a %d-byte sparse file: %v", unmappable, err)
	}
	require.NoError(t, f.Close())
	t.Cleanup(func() { os.Remove(path) })

	a, err := OpenAppender(path, false)
	require.NoError(t, err)
	t.Cleanup(func() { a.Close() })

	b, done, err := a.View(0, 8)
	require.NoError(t, err, "a file that cannot be mapped falls back to ReadAt")
	require.Equal(t, make([]byte, 8), b)
	done()
	require.True(t, a.mapper.disabled, "the fallback latches: mmap is not retried per view")

	// Every later view on this appender is a copy, and still reads correctly.
	// (The write lands at the sparse file's end, far past offset 0.)
	_, err = a.Append([]byte("hello"))
	require.NoError(t, err)
	b2, done2, err := a.View(a.Offset()-5, 5)
	require.NoError(t, err, "the latched fallback keeps serving")
	require.Equal(t, []byte("hello"), b2)
	done2()
}

// TestViewRaceSharesTheFreshMapping: concurrent views past the end of the
// current mapping — one of them remaps, the rest must see the mapping is
// already large enough and re-read instead of mapping a second time. Every
// view still observes the appended bytes.
func TestViewRaceSharesTheFreshMapping(t *testing.T) {
	_, a := newTestAppender(t)
	t.Cleanup(func() { a.Close() })

	payload := []byte("0123456789abcdef")
	_, err := a.Append(payload)
	require.NoError(t, err)

	for range 200 {
		// A view of the whole file establishes the current mapping.
		_, done, err := a.View(0, a.Size())
		require.NoError(t, err)
		done()

		// Grow past it: the next views must remap.
		_, err = a.Append(make([]byte, 32))
		require.NoError(t, err)
		size := a.Size()

		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {
				<-start
				v, d, verr := a.View(0, size)
				if verr != nil {
					t.Errorf("View: %v", verr)
					return
				}
				if !bytes.Equal(v[:len(payload)], payload) {
					t.Errorf("view lost the appended bytes: %q", v[:len(payload)])
				}
				d()
			})
		}
		close(start)
		wg.Wait()

		// Drop the mapping so the next round races on a fresh one.
		require.NoError(t, a.Truncate(int64(len(payload))))
	}
}
