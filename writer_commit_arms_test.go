package rowpack

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/fault"
	"github.com/codeforgee/rowpack/internal/format"
)

// writer_commit_arms_test.go 覆盖 put() 与 commitLocked() 的失败传播:任何一步
// 拒绝都必须成为一个失败的提交(而不是半个快照),并且 writer 必须进入 failed。
//
// 剩下四个零覆盖块都是防御性的,无法从任何一个真实状态走到:
//   - createTable 的 FULL 重定义分支里 latest==0 兜底:能解析出 tableID 的链上
//     表必然有版本;
//   - sealPendingBlock 里整块 Seal 的错误臂:seal.Cipher.Seal 恒不失败;
//   - addBlocksToTxn 里 AddRow 的错误臂:快照号由本 writer 填写,且提交路径关
//     闭了行去重,唯一剩下的失败条件不可能触发;
//   - writePendingBlock 里负载 Append 的错误臂:它与块头共用同一个文件,块头
//     写成功后负载没有独立的失败方式。

// TestPutRejectsAfterAbort: put() re-checks the writer state on its own, so a
// writer that went away between the resolve and the write cannot sneak a row in.
func TestPutRejectsAfterAbort(t *testing.T) {
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Rollback())

	err := tx.w.put(context.Background(), rowChange{
		typ: ChangeInsert, table: 1, rowID: 1, row: Row{Uint64(1)},
	})
	require.ErrorIs(t, err, ErrSnapshotAborted)
}

// TestPutRejectsOversizedRow: a row larger than the page target takes the
// oversized-row path, which stores it as its own page. If that page cannot be
// encoded, put() must fail and must not count the row.
func TestPutRejectsOversizedRow(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeString}}))
	tid := tx.w.tableIDs["t"]

	rb := block.NewRowsBuilder(1, uint32(tid), block.Config{
		BlockSize:   1 << 20,
		Compression: format.Compression(99), // cannot be encoded
		Limits:      block.Limits{MaxRawBytes: 1 << 20},
	})
	rb.SetPageSize(64) // every row is "oversized" and gets its own page
	tx.w.rowBuilders[tid] = rb

	require.Error(t, tx.Insert(context.Background(), "t", 1, Row{String(strings.Repeat("x", 200))}))
	require.Zero(t, tx.w.rowRecordCount, "a refused row must not be counted")
}

// TestCommitRejectsUnflushableMetadata: flushAll() is the first thing a commit
// does, so a metadata block that cannot be encoded aborts before a single byte
// of the snapshot is written.
func TestCommitRejectsUnflushableMetadata(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	// A builder with an unknown codec: Add() buffers happily, Flush() cannot
	// encode, so the failure lands in flushAll.
	sabotageMetadataWith(tx, block.Config{
		BlockSize:   1 << 20,
		Compression: format.Compression(99),
		Limits:      block.Limits{MaxRawBytes: 1 << 20},
	})
	require.NoError(t, tx.DefineTable("t", armCols))

	_, err := tx.Commit(context.Background())
	require.Error(t, err)
	require.Equal(t, writerFailed, tx.w.state)
}

// TestCommitRejectsUnflushableRows does the same for the rows side of flushAll.
func TestCommitRejectsUnflushableRows(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	tid := tx.w.tableIDs["t"]
	require.NotZero(t, tid)

	rb := block.NewRowsBuilder(1, uint32(tid), block.Config{
		BlockSize:   1 << 20,
		Compression: format.Compression(99),
		Limits:      block.Limits{MaxRawBytes: 1 << 20},
	})
	rb.SetPageSize(4096)
	tx.w.rowBuilders[tid] = rb
	require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))

	_, err := tx.Commit(context.Background())
	require.Error(t, err)
	require.Equal(t, writerFailed, tx.w.state)
}

// TestCommitRejectsInvalidSnapshotType: the index builder validates the snapshot
// entry, so a writer carrying an unknown type cannot publish one.
func TestCommitRejectsInvalidSnapshotType(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))
	tx.w.typ = SnapshotType(9)

	_, err := tx.Commit(context.Background())
	require.ErrorContains(t, err, "bad type")
	require.Equal(t, writerFailed, tx.w.state)
}

// TestCommitRejectsForgedPendingBlocks feeds the txn builder block, metadata
// and row entries it must refuse. The bytes are appended to the file before the
// index is built (a torn commit), but nothing is published.
func TestCommitRejectsForgedPendingBlocks(t *testing.T) {
	t.Run("block owned by another snapshot", func(t *testing.T) {
		tx := armTx(t, armStore(t), NoParent)
		require.NoError(t, tx.DefineTable("t", armCols))
		require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))
		forgePending(tx, format.BlockHeader{
			SnapshotID: 0, // never this writer's snapshot
			BlockKind:  format.BlockKindRows,
			TableID:    uint32(tx.w.tableIDs["t"]),
		}, nil, nil)
		_, err := tx.Commit(context.Background())
		require.ErrorContains(t, err, "block entry snapshot mismatch")
	})

	t.Run("metadata owned by another snapshot", func(t *testing.T) {
		tx := armTx(t, armStore(t), NoParent)
		require.NoError(t, tx.DefineTable("t", armCols))
		require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))
		forgePending(tx, format.BlockHeader{
			SnapshotID: uint64(tx.w.id),
			BlockKind:  format.BlockKindMetadata,
		}, nil, []format.MetadataIndexEntry{{SnapshotID: 0, ObjectID: 1}})
		_, err := tx.Commit(context.Background())
		require.ErrorContains(t, err, "metadata entry snapshot mismatch")
	})

	t.Run("row carrying an unpackable change type", func(t *testing.T) {
		tx := armTx(t, armStore(t), NoParent)
		require.NoError(t, tx.DefineTable("t", armCols))
		require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))
		forgePending(tx, format.BlockHeader{
			SnapshotID: uint64(tx.w.id),
			BlockKind:  format.BlockKindRows,
			TableID:    uint32(tx.w.tableIDs["t"]),
		}, []format.RowDirectoryEntry{
			{RowID: 2, SchemaVersion: 1, ChangeType: format.ChangeType(9)},
		}, nil)
		_, err := tx.Commit(context.Background())
		require.Error(t, err)
		require.Equal(t, writerFailed, tx.w.state)
	})
}

// TestCommitRejectsUnsealableBlock: on an encrypted store every rows block is
// sealed page by page, which means parsing it as a container first. A block the
// sealer cannot parse must stop the commit.
func TestCommitRejectsUnsealableBlock(t *testing.T) {
	db, err := Create(tmpdb(t), Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k1", key: testKey("k1")},
		KeyID:       "k1",
	}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert(context.Background(), "t", 1, Row{Uint64(1)}))
	// "forged" is not a container: the page sealer must refuse it.
	forgePending(tx, format.BlockHeader{
		SnapshotID: uint64(tx.w.id),
		BlockKind:  format.BlockKindRows,
		TableID:    uint32(tx.w.tableIDs["t"]),
	}, nil, nil)

	_, err = tx.Commit(context.Background())
	require.Error(t, err)
	require.Equal(t, writerFailed, tx.w.state)
}

// TestCommitUnknownWhenSchemaDerivationFails: schema derivation reads the
// snapshot's own metadata back through the block reader. Failing there happens
// after the single sync, so the bytes are durable: the outcome is unknown and
// the store must latch must-reopen.
func TestCommitUnknownWhenSchemaDerivationFails(t *testing.T) {
	ctx := context.Background()
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert(ctx, "t", 1, Row{Uint64(1)}))

	fault.Inject("commit.sync.after", func() { _ = db.data.Close() })
	defer fault.Clear()
	_, err := tx.Commit(ctx)
	require.Error(t, err)

	var ce *CommitError
	require.ErrorAs(t, err, &ce)
	require.True(t, ce.Unknown, "the schema index is derived after the sync")
	_, err = db.Begin(ctx, NoParent)
	require.ErrorIs(t, err, ErrMustReopen)
}

// TestAddressOfUncachedTable: addressOf only ever answers for tables this
// transaction resolved, so an unknown id must say so instead of inventing one.
func TestAddressOfUncachedTable(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.Equal(t, "", tx.w.addressOf(9999))
	require.Equal(t, "t", tx.w.addressOf(tx.w.tableIDs["t"]))
}

// sabotageMetadataWith installs a metadata block builder with the given config.
func sabotageMetadataWith(tx *Tx, cfg block.Config) {
	tx.w.metaBuilder = block.NewMetadataBuilder(1, 0, cfg)
}

// forgePending appends a synthetic pending block: commitLocked writes it and
// then hands its directory entries to the index builder, which is where the
// forged entries get refused.
func forgePending(tx *Tx, hdr format.BlockHeader, rows []format.RowDirectoryEntry, meta []format.MetadataIndexEntry) {
	tx.w.pending = append(tx.w.pending, &pendingBlock{
		header:  hdr,
		payload: []byte("forged"),
		rowsDir: rows,
		meta:    meta,
	})
}
