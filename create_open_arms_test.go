package rowpack

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// create_open_arms_test.go 覆盖 Create/Open 的「还没开始就失败」臂:加密配置不可用、
// 选项不合法、以及数据文件存在但打不开。三者都必须在留下任何状态之前失败——Create
// 里加密是在建文件之前解析的,所以拿不到密钥时磁盘上不该出现 store 文件。

// TestCreateRejectsUnusableEncryption: an unusable encryption configuration is
// rejected before the store file exists, whether the configuration is
// incomplete or the key simply cannot be produced.
func TestCreateRejectsUnusableEncryption(t *testing.T) {
	_, err := Create(filepath.Join(tmpdb(t), "no-provider"), Options{
		Encryption: &EncryptionConfig{KeyID: "k1"},
	})
	require.ErrorIs(t, err, ErrInvalidArgument)
	require.ErrorContains(t, err, "key provider is nil")

	base := filepath.Join(tmpdb(t), "no-key")
	_, err = Create(base, Options{
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: "k1", fail: errors.New("vault down")},
			KeyID:       "k1",
		},
	})
	require.ErrorIs(t, err, ErrKeyUnavailable)
	require.NoFileExists(t, base+".rpk", "a Create that cannot resolve its key leaves no store behind")
}

// TestOpenRejectsInvalidOptions: options are resolved (and rejected) before the
// file is touched.
func TestOpenRejectsInvalidOptions(t *testing.T) {
	_, err := Open(filepath.Join(tmpdb(t), "never-opened"), Options{BlockSize: 1})
	require.ErrorIs(t, err, ErrInvalidArgument)
}

// TestOpenRejectsUnopenableDataFile: the store file is there but cannot be
// opened, so Open must say so instead of reporting a missing store.
func TestOpenRejectsUnopenableDataFile(t *testing.T) {
	base := filepath.Join(tmpdb(t), "store")
	require.NoError(t, os.Mkdir(base+".rpk", 0o755), "the data path exists but is not a file")

	_, err := Open(base, Options{})
	require.ErrorContains(t, err, "open store file")
	require.NotErrorIs(t, err, ErrNotFound)
}

// TestCreateOpenInvalidPath: an empty base path stays empty after suffix
// trimming (filepath.Clean("") is ".", which would silently become ".rpk"), so
// both entry points reject it before touching the filesystem.
func TestCreateOpenInvalidPath(t *testing.T) {
	for _, base := range []string{"", ".rpk", ".rpi"} {
		_, err := Create(base, Options{})
		require.ErrorIs(t, err, ErrInvalidPath, "Create(%q)", base)
		_, err = Open(base, Options{})
		require.ErrorIs(t, err, ErrInvalidPath, "Open(%q)", base)
	}
}
