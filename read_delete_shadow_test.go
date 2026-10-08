package rowpack

// Round-3 read-path probes: the OperationDelete shadow a DELTA can cast over
// an inherited table, and a metadata payload that passes the block CRC gate
// but is not a parseable metadata payload (VerifyFull must catch it).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/metadata"
)

// TestTablesShadowsDeletedTable: a DELTA that carries an OperationDelete
// entry for a table object shadows the definition the snapshot inherits from
// its parent chain. The table must vanish from Tables() at that snapshot —
// the delete tombstone is found first on the chain and reported as deleted —
// while the parent snapshot still lists it.
func TestTablesShadowsDeletedTable(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	oid, _ := realTableLoc(t, db)

	before, err := db.Tables(ctx, 1)
	require.NoError(t, err)
	require.NotEmpty(t, before)

	publishCraftedSnapshot(t, db, 2, 1, format.SnapshotDelta, func(b *index.Builder) {
		require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
			SnapshotID: 2, ObjectID: oid, Revision: 1,
			RecordType: uint32(format.RecordTable),
			BlockID:    999999, ItemOrdinal: 0,
			Operation: format.OperationDelete,
		}))
	})

	after, err := db.Tables(ctx, 2)
	require.NoError(t, err)
	require.Empty(t, after, "the deleted table is shadowed at snapshot 2")

	// The parent snapshot is untouched.
	again, err := db.Tables(ctx, 1)
	require.NoError(t, err)
	require.Len(t, again, len(before))

	// Every read path must agree with the catalog: schema derivation shadows
	// the table too, so data and schema reads fail with ErrNotFound instead
	// of silently resolving the table through the parent chain.
	_, err = db.Get(ctx, 2, "t", 1, nil)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Schema(ctx, 2, "t", 0)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.Scan(ctx, 2, "t", ScanOptions{})
	require.ErrorIs(t, err, ErrNotFound)
}

// TestVerifyFullCatchesUnparseableMetadataPayload: a metadata block whose
// payload is damaged but whose block-level CRC chain was restamped passes the
// loader and only metadata.Parse can reject it. VerifyFull must surface the
// corruption.
func TestVerifyFullCatchesUnparseableMetadataPayload(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "metapatch")
	db, err := Create(base, Options{Compression: CompressionNone, BlockSize: 1024})
	require.NoError(t, err)
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1)}))
	_, err = w.Commit(ctx)
	require.NoError(t, err)

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()

	moff := firstMetadataBlock(t, db)
	mb := loadPatchableMetaBlock(t, f, moff)
	// Flip a directory-entry byte: the block CRC is restamped by write, but
	// the payload's internal directory CRC no longer matches.
	mb.payload[len(mb.payload)-1] ^= 0xFF // a record-body byte: caught by the record CRC
	mb.write(t)

	publishCraftedSnapshot(t, db, 2, 1, format.SnapshotDelta, func(b *index.Builder) {
		require.NoError(t, b.AddBlock(format.BlockIndexEntry{
			BlockID:    999999,
			SnapshotID: 2,
			TableID:    1,
			BlockKind:  format.BlockKindMetadata,
			DataOffset: uint64(moff),
			RawSize:    mb.hdr.RawSize,
			StoredSize: mb.hdr.StoredSize,
		}))
	})

	_, err = db.Verify(ctx, VerifyFull, VerifyScope{})
	require.ErrorIs(t, err, ErrCorruptData)
}


// TestDerivedSchemaShadowsDeletedColumn: a DELETE entry for one column object
// removes that column from the schema derived for the shadowing snapshot —
// including its older definition on the parent chain — while the table and
// the untouched column survive.
func TestDerivedSchemaShadowsDeletedColumn(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{BlockSize: 1024})
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{
		{Name: "keep", Type: TypeInt64},
		{Name: "gone", Type: TypeInt64},
	}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Int64(1), Int64(2)}))
	snap1, err := w.Commit(ctx)
	require.NoError(t, err)

	// Find the object id of column "gone" at snapshot 1.
	st, err := db.captureState()
	require.NoError(t, err)
	var goneOID uint64
	for _, cid := range st.view.MetadataByType(1, uint32(format.RecordColumn)) {
		rec, err := db.readMetadataCached(st.view, 1, cid, nil)
		require.NoError(t, err)
		if fieldString(rec, metadata.ColColumnName) == "gone" {
			goneOID = cid
		}
	}
	require.NotZero(t, goneOID, "column gone must have an object id")

	publishCraftedSnapshot(t, db, 2, 1, format.SnapshotDelta, func(b *index.Builder) {
		require.NoError(t, b.AddMetadata(format.MetadataIndexEntry{
			SnapshotID: 2, ObjectID: goneOID, Revision: 1,
			RecordType: uint32(format.RecordColumn),
			BlockID:    999999, ItemOrdinal: 0,
			Operation: format.OperationDelete,
		}))
	})

	sch, err := db.Schema(ctx, 2, "t", 0)
	require.NoError(t, err)
	require.Len(t, sch.Columns, 1, "the deleted column is shadowed")
	require.Equal(t, "keep", sch.Columns[0].Name)

	// The parent snapshot still sees both columns.
	full, err := db.Schema(ctx, snap1, "t", 0)
	require.NoError(t, err)
	require.Len(t, full.Columns, 2)

	// Rows written before the delete still read back; the shadowed column's
	// value is simply absent from the derived schema.
	row, err := db.Get(ctx, 2, "t", 1, nil)
	require.NoError(t, err)
	v, ok := row[0].Int64()
	require.True(t, ok)
	require.Equal(t, int64(1), v)
}
