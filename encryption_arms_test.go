package rowpack

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/seal"
)

// encryption_arms_test.go 覆盖解密器「拿不到密钥」的三条臂:块、页、索引分片三条解密
// 入口都必须把密钥解析失败原样报成 ErrKeyUnavailable——它不是认证失败(ErrAuthFailed),
// 因为根本没到认证那一步。以及一条「密钥长度不对」臂:provider 给了不能用的密钥,同样
// 不能被当成数据损坏。
//
// 白盒直接构造解密器:写入路径只写 epoch 0,而打开时 epoch 0 的密钥已经解析并缓存,
// 通过文件走不到这些臂。

func TestDecrypterReportsUnavailableKey(t *testing.T) {
	d := newDecrypter(&staticKeyProvider{keyID: "k1", fail: errors.New("vault down")}, "k1", [16]byte{})

	_, err := d.Decrypt(format.BlockHeader{}, nil)
	require.ErrorIs(t, err, ErrKeyUnavailable)
	require.NotErrorIs(t, err, ErrAuthFailed)

	_, err = d.OpenPage(format.BlockHeader{}, format.RowsPageDirEntry{}, nil)
	require.ErrorIs(t, err, ErrKeyUnavailable)

	_, err = d.OpenIndexChunk(seal.ChunkContext{}, nil)
	require.ErrorIs(t, err, ErrKeyUnavailable)
}

func TestDecrypterRejectsUnusableKey(t *testing.T) {
	d := newDecrypter(&staticKeyProvider{keyID: "k1", key: []byte("too short")}, "k1", [16]byte{})

	_, err := d.Decrypt(format.BlockHeader{}, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrKeyUnavailable, "the provider answered, the key is simply unusable")
	require.NotErrorIs(t, err, ErrCorruptData)
}
