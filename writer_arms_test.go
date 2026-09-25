package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// writer_arms_test.go 钉住「同一快照里一个 (表,行) 只能出现一次」这条语义:删除后再
// 插入同一行由写入器自己的行集挡住,所以索引构建器永远不会看到重复项——这也是
// addBlocksToTxn 里 AddRow 的错误臂不可达的原因。
//
// writer.go 另外三条零覆盖臂同样只能靠不存在的故障注入到达:createTable 的 FULL 分支
// 里 latest==0(地址索引与版本索引一致时不可能)、块的 AEAD Seal 失败(只有 nonce 耗尽
// 才会),以及写完块头之后写块体的 I/O 失败(现有故障点都在块头之前)。

// TestWriterRejectsResurrectedRow: deleting a row and inserting it again in the
// same snapshot is refused — one (table, row) pair, one record. The row stays
// deleted; the resurrect never reaches the index.
func TestWriterRejectsResurrectedRow(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t) // snapshot 1 holds row 1

	tx := armTx(t, db, 1)
	require.NoError(t, tx.Delete("t", 1))
	err := tx.Insert("t", 1, Row{Uint64(9)})
	require.ErrorContains(t, err, "duplicate")

	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	_, err = db.Get(ctx, snap, "t", 1, nil)
	require.ErrorIs(t, err, ErrNotFound, "the row is deleted, the refused insert changed nothing")
}
