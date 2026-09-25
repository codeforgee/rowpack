package rowpack

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// rowpack_arms_test.go 处理 rowpack.go 的六条零覆盖臂。逐条判定之后,六条都是没有
// 输入能走到的,下面每个测试钉住一条让它们到不了的语义:
//
//   - rand/UUID 失败(65、162):crypto/rand.Read 自 Go 1.24 起不再返回错误——它只从
//     不会失败的源读取,因此 effectiveUUID 的错误臂和 Create 里包装它的那条没有输入。
//     testUUIDOverride 只是让 golden 字节确定,不模拟失败。UUID 只在 Create 生成一次
//     并写进文件头,Open 从头里读回(见 TestReopenReadsUUIDFromHeader)。
//   - 文件头编码失败(188):MarshalTo 只因目标太短或 KeyID 过长而失败;目标是定长数组,
//     而 KeyID 已被 EncryptionConfig.validate 用同一个「> FileHeaderKeyIDMaxLen」判据
//     挡在 Create 的最前面。
//   - Create 的双重故障(196):openStore 失败后要求 os.Remove 也失败。文件是本进程刚在
//     同一个目录里创建成功的——目录可写、文件存在,Remove 唯一还可能报的就是
//     os.ErrNotExist,而它已被显式忽略;要让 Remove 因权限失败,就必须让目录不可写,
//     那样 iofile.CreateSingle 会先失败,根本走不到这里。清理本身有效,见
//     TestCreateCleansUpWhenOpenFails。
//   - abort 失败(310):Writer.abort 只有两种返回值——nil 和 ErrSnapshotCommitted,后者
//     被 Close 显式排除。见 TestCloseIgnoresAbortOfCommittedWriter。
//   - 锁释放失败(320):Unix 上的 Release 是 Flock(LOCK_UN) 加 Close,持有锁的文件是
//     本进程打开且仍然有效的;只读打开根本不取锁(s.lock 为 nil)。见
//     TestReadOnlyCloseNeedsNoLock。

// oneShotKeyProvider serves the key once and then fails: Create resolves the key
// twice — once to stamp the header, once again inside openStore when the write
// path resolves it up front — so the second call is what makes the open half of
// Create fail.
type oneShotKeyProvider struct {
	served int
}

func (p *oneShotKeyProvider) Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error) {
	p.served++
	if p.served > 1 {
		return nil, errors.New("vault closed")
	}
	return testKey(keyID), nil
}

// TestCreateCleansUpWhenOpenFails: a Create that fails after the file exists
// leaves no store behind. The clean-up itself cannot fail — the file was just
// created by this process in a directory it can write — which is why the
// "clean up failed too" arm has no input.
func TestCreateCleansUpWhenOpenFails(t *testing.T) {
	base := filepath.Join(tmpdb(t), "store")

	_, err := Create(base, Options{
		Encryption: &EncryptionConfig{
			KeyProvider: &oneShotKeyProvider{},
			KeyID:       "k1",
		},
	})
	require.ErrorContains(t, err, "vault closed", "the write path resolves the key again and fails")
	require.NoFileExists(t, base+".rpk", "a failed Create leaves no store behind")
}

// TestReopenReadsUUIDFromHeader: the UUID is generated once at Create and is a
// file property afterwards — Open never generates one.
func TestReopenReadsUUIDFromHeader(t *testing.T) {
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{})
	require.NoError(t, err)
	uuid := db.UUID()
	require.NotEqual(t, [16]byte{}, uuid, "Create stamps a UUID")
	require.NoError(t, db.Close())

	reopened, err := Open(base, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	require.Equal(t, uuid, reopened.UUID(), "the UUID is read back from the header, never regenerated")
}

// TestCloseIgnoresAbortOfCommittedWriter: Close aborts the active writer to
// release it, and a committed writer answers "already committed" — that is not
// a Close failure. It is in fact the only error that path can produce, which is
// why Close has no "abort failed" branch to take.
func TestCloseIgnoresAbortOfCommittedWriter(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	tx := armTx(t, db, 1)
	require.NoError(t, tx.Insert("t", 2, Row{Uint64(2)}))
	_, err := tx.Commit(ctx)
	require.NoError(t, err)

	require.NoError(t, db.Close(), "aborting a committed writer is not an error")
}

// TestCloseAbortsOpenWriter: an uncommitted writer is discarded by Close. Its
// rows never existed, and committing afterwards is refused by the closed store.
func TestCloseAbortsOpenWriter(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)

	tx := armTx(t, db, 1)
	require.NoError(t, tx.Insert("t", 3, Row{Uint64(3)}))
	require.NoError(t, db.Close(), "aborting an open writer is not an error")

	_, commitErr := tx.Commit(ctx)
	require.ErrorIs(t, commitErr, ErrSnapshotAborted, "the writer Close aborted cannot commit any more")
}

// TestReadOnlyCloseNeedsNoLock: a read-only open takes no writer lock, so Close
// has nothing to release — and Close stays idempotent.
func TestReadOnlyCloseNeedsNoLock(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{})
	require.NoError(t, err)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	ro, err := Open(base, Options{ReadOnly: true})
	require.NoError(t, err)
	require.True(t, ro.ReadOnly())
	require.Nil(t, ro.lock, "a read-only open holds no writer lock")

	require.NoError(t, ro.Close())
	require.NoError(t, ro.Close(), "Close is idempotent")
}
