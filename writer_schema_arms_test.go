package rowpack

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/metadata"
)

// writer_schema_arms_test.go 覆盖表定义与 schema 解析的拒绝臂:metadata 记录编码
// 失败、块体积上限、以及在 store 已关闭(state 置 nil)时仍被问到的 ns/列比较/表
// 解析——它们都必须给出确定答案,而不是悄悄编造一个。

var armCols = []Column{{Name: "id", Type: TypeUint64}}

func armStore(t *testing.T) *Store {
	t.Helper()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func armTx(t *testing.T, db *Store, parent SnapshotID) *Tx {
	t.Helper()
	tx, err := db.Begin(context.Background(), parent)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// armCommittedStore returns a store holding one committed snapshot with table
// "t" (one row) and reopens nothing: the committed chain is what the FULL
// redefinition branch of createTable resolves against.
func armCommittedStore(t *testing.T) *Store {
	t.Helper()
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	_, err := tx.Commit(context.Background())
	require.NoError(t, err)
	return db
}

// sabotageMetadata installs a metadata block builder with its own per-record
// ceiling, so the table-definition write path exercises the "record does not
// fit" error instead of silently dropping the definition.
func sabotageMetadata(tx *Tx, maxRaw int) {
	tx.w.metaBuilder = block.NewMetadataBuilder(1, 0, block.Config{
		BlockSize:   1 << 20,
		Compression: format.CompressionNone,
		Limits:      block.Limits{MaxRawBytes: uint32(maxRaw)},
	})
}

// TestWriteMetadataRejectsMalformedRecord: the encoder, not the block builder,
// owns record validity; a record that cannot be encoded must never be buffered.
func TestWriteMetadataRejectsMalformedRecord(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	require.ErrorContains(t, tx.w.writeMetadata(&metadata.Record{}), "namespace is empty")
}

// TestCreateTableRejectsOversizedTableRecord: the Table record itself is the
// first thing a definition writes, so the ceiling rejects it before any column.
func TestCreateTableRejectsOversizedTableRecord(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	sabotageMetadata(tx, 1)
	err := tx.DefineTable("t", armCols)
	require.ErrorContains(t, err, "exceeds limit")
	require.Len(t, tx.w.metaRecords, 1, "only the Table record ever reached the block builder")
	require.EqualValues(t, format.RecordTable, tx.w.metaRecords[0].RecordType)
}

// TestCreateTableRejectsOversizedColumnRecord: a Table record is ~77 bytes and
// fits under the ceiling, so the failure has to come from a column record that
// does not (a column carries five fields).
func TestCreateTableRejectsOversizedColumnRecord(t *testing.T) {
	tx := armTx(t, armStore(t), NoParent)
	sabotageMetadata(tx, 100)
	long := []Column{{Name: strings.Repeat("c", 200), Type: TypeUint64}}
	require.ErrorContains(t, tx.DefineTable("t", long), "exceeds limit")
	require.Len(t, tx.w.metaRecords, 2, "the Table record was accepted before the column failed")
	require.EqualValues(t, format.RecordColumn, tx.w.metaRecords[1].RecordType)
}

// TestCreateTableFullRedefinitionRejectsOversizedRecord: a FULL snapshot does
// not inherit visibility, so re-defining an ancestor's table writes its own
// metadata layer — and that write's failure belongs to the caller.
func TestCreateTableFullRedefinitionRejectsOversizedRecord(t *testing.T) {
	db := armCommittedStore(t)
	// NoParent on a store that already has snapshots: parentOf() resolves to the
	// latest one, so the definition takes the FULL redefinition branch.
	tx := armTx(t, db, NoParent)
	require.Equal(t, SnapshotFull, tx.w.typ)
	sabotageMetadata(tx, 1)
	require.ErrorContains(t, tx.DefineTable("t", armCols), "exceeds limit")
}

// TestSchemaResolutionWithoutCommittedState: Close() drops the published state,
// so every resolver that reads it must answer from what it still knows: the
// default ns, "no comparable schema", and "table not found".
func TestSchemaResolutionWithoutCommittedState(t *testing.T) {
	db := armCommittedStore(t)
	tx := armTx(t, db, Latest)
	require.NoError(t, tx.Insert("t", 2, Row{Uint64(2)}))
	tid := tx.w.tableIDs["t"]
	require.NotZero(t, tid, "the insert must have cached the table id")

	require.NoError(t, db.Close())

	require.Equal(t, NSUser, tx.w.nsOf(tid), "no state means no ns override")
	require.False(t, tx.w.columnsEqual(1, tid, armCols), "no state means nothing to compare against")
	require.NoError(t, tx.w.checkParent(tid, 1, ChangeInsert), "no state means no parent existence check")

	_, _, err := tx.w.tableForWrite("missing")
	require.ErrorIs(t, err, ErrNotFound, "a table unknown to the transaction needs the committed chain")
}

// TestTxnSchemaLookupsOnChainTable: a table this transaction merely cached has
// no txn schema version, so nameOf has nothing to report and txnColumnsEqual
// must defer to the chain branch rather than claim a match.
func TestTxnSchemaLookupsOnChainTable(t *testing.T) {
	db := armCommittedStore(t)
	tx := armTx(t, db, Latest)
	require.NoError(t, tx.Insert("t", 2, Row{Uint64(2)}))
	tid := tx.w.tableIDs["t"]
	require.NotZero(t, tid)

	require.Zero(t, tx.w.latestVersion(tid), "the cached table carries no txn version")
	require.Equal(t, "", tx.w.nameOf(tid))
	require.Equal(t, NSUser, tx.w.nsOf(tid), "a cached table carries no ns of its own; the chain answers")
	require.False(t, tx.w.txnColumnsEqual(tid, armCols))
	require.False(t, tx.w.columnsEqual(999, tid, armCols), "an unknown snapshot has no schema version")
}

// TestAbortAfterCommitIsRefused: Store.Close() only aborts the writer the store
// still holds, so the internal close path must keep its own committed check.
func TestAbortAfterCommitIsRefused(t *testing.T) {
	ctx := context.Background()
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	_, err := tx.Commit(ctx)
	require.NoError(t, err)

	require.ErrorIs(t, tx.w.abort(), ErrSnapshotCommitted)
}

// TestCommitOnClosedStoreFailsTheWriter: the store can go away under an open
// writer; Commit must fail the writer instead of writing into a closed file.
func TestCommitOnClosedStoreFailsTheWriter(t *testing.T) {
	ctx := context.Background()
	db := armStore(t)
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", armCols))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))

	require.NoError(t, db.Close())
	// Put the writer back into its pre-close state: the store is closed, but
	// the writer was never told.
	tx.w.state = writerOpen

	_, err := tx.Commit(ctx)
	require.Error(t, err)
	require.Equal(t, writerFailed, tx.w.state, "a commit that cannot even start must fail the writer")
}
