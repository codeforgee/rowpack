package iofile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 生命周期错误路径：句柄失效（Close 之后）或文件被截短时，每个写入口都必须把
// 错误返回给调用方——静默成功会让上层 store 在失效句柄上继续记账，最终写坏文件。
// 这些分支此前全部未执行（0% 的部分正是失败返回）。

func TestOperationsAfterCloseReportErrors(t *testing.T) {
	_, a := newTestAppender(t)
	if _, err := a.Append([]byte("0123456789")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	err := a.Close()
	require.NoError(t, err, "Close")
	off, size := a.Offset(), a.Size()

	if _, err := a.Append([]byte("more")); err == nil {
		t.Fatal("Append after Close must fail")
	}
	if _, err := a.Append(make([]byte, 3*8192)); err == nil {
		t.Fatal("zero Append after Close must fail")
	}
	require.Error(t, a.Truncate(0), "Truncate after Close must fail")
	require.Error(t, a.Sync(), "Sync after Close must fail")
	if _, _, err := a.View(0, 4); err == nil {
		t.Fatal("View after Close must fail")
	} else {
		require.Contains(t, err.Error(), "stat file for view", "View error = %v, want the stat-for-view diagnosis", err)
	}
	if _, err := a.ReadAt(make([]byte, 4), 0); err == nil {
		t.Fatal("ReadAt after Close must fail")
	}
	// 失败的写不得改动记账，否则后续逻辑会以为数据已经落盘。
	if a.Offset() != off || a.Size() != size {
		t.Fatalf("failed writes moved counters: offset %d->%d size %d->%d", off, a.Offset(), size, a.Size())
	}
	a.Close() // 幂等
}

// TestTruncateDropsStaleMapping: Truncate 必须丢弃旧映射。留着它的话，一个覆盖
// 已删除字节的视图会直接踩到 EOF 之外（SIGBUS），而不是报错。
func TestTruncateDropsStaleMapping(t *testing.T) {
	path, a := newTestAppender(t)
	defer a.Close()
	ForceReadAt(false)
	defer ForceReadAt(false)

	if _, err := a.Append([]byte("0123456789abcdef")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	v, done, err := a.View(0, 16) // 建立覆盖 16 字节的映射
	require.NoError(t, err, "View")
	if string(v) != "0123456789abcdef" {
		done()
		t.Fatalf("view %q", v)
	}
	done()

	err = a.Truncate(4)
	require.NoError(t, err, "Truncate")
	if fi, err := os.Stat(path); err != nil || fi.Size() != 4 {
		t.Fatalf("file size after truncate = %v %v, want 4", fi, err)
	}
	if _, _, err := a.View(0, 16); err == nil || !strings.Contains(err.Error(), "beyond file size") {
		t.Fatalf("stale mapping survived Truncate: %v", err)
	}
	v2, done2, err := a.View(0, 4)
	require.NoError(t, err, "View after truncate")
	defer done2()
	if string(v2) != "0123" {
		t.Fatalf("view after truncate %q, want %q", v2, "0123")
	}
}

// TestViewRemapsAfterGrowth 覆盖「已有映射 + 文件增长」的重映射分支：旧映射必须先
// munmap，否则每次增长都泄漏一段地址空间。
func TestViewRemapsAfterGrowth(t *testing.T) {
	_, a := newTestAppender(t)
	defer a.Close()
	ForceReadAt(false)
	defer ForceReadAt(false)

	if _, err := a.Append([]byte("aaaa")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	v1, done1, err := a.View(0, 4) // 建立 4 字节映射
	require.NoError(t, err, "first View")
	if string(v1) != "aaaa" {
		done1()
		t.Fatalf("first view %q, want %q", v1, "aaaa")
	}
	done1()

	// 增长超过当前映射长度：下一次 View 必须重映射（并释放旧映射）。
	if _, err := a.Append([]byte(strings.Repeat("b", 4096))); err != nil {
		t.Fatalf("Append growth: %v", err)
	}
	v2, done2, err := a.View(0, a.Offset())
	require.NoError(t, err, "second View")
	defer done2()
	if len(v2) != 4100 {
		t.Fatalf("remapped view length %d, want 4100", len(v2))
	}
	if string(v2[:4]) != "aaaa" || string(v2[4:]) != strings.Repeat("b", 4096) {
		t.Fatalf("remapped view content mismatch: %q", v2[:8])
	}
	// 中段视图仍需命中新映射（不再触发重映射）。
	v3, done3, err := a.View(100, 8)
	require.NoError(t, err, "third View")
	defer done3()
	if string(v3) != strings.Repeat("b", 8) {
		t.Fatalf("mid-file view %q", v3)
	}
}

// TestCreateSingleNeverDestroysExistingFile 钉住失败清理的边界：CreateSingle 只在
// 「自己创建成功、后续步骤失败」时才删文件；O_EXCL 让已存在的目标在打开阶段就
// 失败，此时既有 store 必须原样保留。
func TestCreateSingleNeverDestroysExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.rpk")
	err := CreateSingle(path, []byte("HEADER"), false)
	require.NoError(t, err, "CreateSingle")
	err = CreateSingle(path, []byte("X"), false)
	require.Error(t, err, "CreateSingle over an existing file must fail")
	require.Contains(t, err.Error(), "create store file")
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "HEADER" {
		t.Fatalf("failed create destroyed the existing store: %q (%v)", b, err)
	}

	// 目标已经是目录：同样只能在打开阶段失败，且绝不能把目录删掉。
	dpath := filepath.Join(dir, "adir")
	err = os.Mkdir(dpath, 0o755)
	require.NoError(t, err, "mkdir")
	require.Error(t, CreateSingle(dpath, []byte("X"), false), "CreateSingle onto a directory must fail")
	if fi, err := os.Stat(dpath); err != nil || !fi.IsDir() {
		t.Fatalf("directory damaged by a failed create: %v %v", fi, err)
	}
}
