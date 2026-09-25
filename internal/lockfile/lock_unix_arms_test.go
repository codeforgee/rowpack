//go:build unix

package lockfile

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// lock_unix_arms_test.go 覆盖 Release 的「解锁失败」臂(lock_unix.go 43):锁文件被别人
// 关掉之后,解锁系统调用必然失败,Release 必须把这个错误报出来,而不是假装锁已经放下。
//
// Acquire 的非 EWOULDBLOCK 分支(29)在 Unix 上到不了:flock 只对已经打开的、有效的
// 描述符生效——打不开的文件在 OpenFile 那一步就返回了(22),而能打开的描述符上的
// flock 只会因「已被别的进程持有」而失败,那正是 EWOULDBLOCK/EAGAIN。

// TestReleaseReportsUnlockFailure: a lock whose file is gone cannot be
// unlocked. Release reports the failure and still clears the file, so a second
// Release is a no-op instead of a second failing syscall.
func TestReleaseReportsUnlockFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	l, err := Acquire(path)
	require.NoError(t, err)

	// Someone closed the lock file behind the Lock's back.
	require.NoError(t, l.f.Close())

	err = l.Release()
	require.Error(t, err, "unlocking a closed file must not be reported as success")
	require.Nil(t, l.f, "Release clears the file even when the unlock failed")

	// The failed Release leaves no lock to release: the store can be locked
	// again by the next writer.
	require.NoError(t, l.Release(), "a second Release is a no-op")
	l2, err := Acquire(path)
	require.NoError(t, err, "the lock file is free again")
	require.NoError(t, l2.Release())
}
