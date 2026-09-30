package rowpack

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/metadata"
)

// schema_corrupt_arms_test.go 覆盖 metadata 块载荷自身说不通时的两条路:目录项数
// 与实际目录长度不符、以及载荷根本不是一个 metadata 载荷。两者都必须成为被分类的
// 结构化损坏,而不是被当成「这张表不存在」跳过去。
//
// 手法沿用 block_stream_corrupt_test.go 的补丁器:改写载荷后重算块级 CRC,使文件
// 的完整性外壳完全合法,损坏只能由 metadata 层自己发现。
//
// 未覆盖的三条删除臂是构造上不可达的:写入路径只产生 OperationUpsert(writer.go
// 的 addMetadata),恢复路径的 Operation 也是从载荷目录抄回来的,因此
// deriveTables 的 DELETE 跳过臂与 readMetadataCached 的「记录已删除」臂没有输入
// 能走到;而「目录比索引指到的条目少」需要索引与块互相矛盾,恢复重建不会造出这种
// 索引。

// openWithPatchedMetadata rewrites the first metadata block of a plain store
// through patch and reopens the store, returning whatever Open says.
func openWithPatchedMetadata(t *testing.T, patch func(t *testing.T, mb *metaBlock)) (*Store, error) {
	t.Helper()
	base, _ := setupPlainMultiPageStore(t, 60)

	db, err := Open(base, Options{Compression: CompressionNone})
	require.NoError(t, err)
	moff := firstMetadataBlock(t, db)
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	mb := loadPatchableMetaBlock(t, f, moff)
	patch(t, mb)
	mb.write(t)

	return Open(base, Options{Compression: CompressionNone})
}

// TestMetadataDirectoryMismatch: the payload header claims more directory
// entries than the directory bytes carry, so the block cannot be parsed — the
// schema index has no honest answer and the open must fail loudly.
func TestMetadataDirectoryMismatch(t *testing.T) {
	_, err := openWithPatchedMetadata(t, func(t *testing.T, mb *metaBlock) {
		var ph metadata.PayloadHeader
		require.NoError(t, ph.Unmarshal(mb.payload[:metadata.PayloadHeaderSize]))
		require.Greater(t, ph.ItemCount, uint32(0))
		ph.ItemCount += 1000
		require.NoError(t, ph.MarshalTo(mb.payload[:metadata.PayloadHeaderSize]))
	})
	requireCorruption(t, "Open with a metadata directory shorter than its item count", err)
}

// TestMetadataPayloadMagicRejected flips a byte of the payload magic: the block
// is intact by CRC but is not a metadata payload at all.
func TestMetadataPayloadMagicRejected(t *testing.T) {
	_, err := openWithPatchedMetadata(t, func(t *testing.T, mb *metaBlock) {
		require.NotZero(t, mb.payload[0], "the payload starts with a magic")
		mb.payload[0] ^= 0xFF
	})
	requireCorruption(t, "Open with a metadata payload whose magic is gone", err)
}
