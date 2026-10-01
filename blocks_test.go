package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBlocksSpanTiling verifies that per-batch SealTable produces one block
// per batch whose [MinRowID, MaxRowID) ranges tile the table's RowID space in
// physical write order — the invariant span-based readers rely on.
func TestBlocksSpanTiling(t *testing.T) {
	ctx := context.Background()
	db := testDB(t, Options{})
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "id", Type: TypeInt64, PrimaryKey: true},
		{Name: "v", Type: TypeUint64},
	}))
	const batches = 5
	const perBatch = 50
	rowID := uint64(0)
	for b := 0; b < batches; b++ {
		for i := 0; i < perBatch; i++ {
			rowID++
			require.NoError(t, tx.Insert(ctx, "t", rowID, Row{Int64(int64(rowID)), Uint64(uint64(rowID * 7))}))
		}
		require.NoError(t, tx.SealTable("t"))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	blks, err := db.Blocks(snap, "t")
	require.NoError(t, err)
	require.Len(t, blks, batches, "one block per sealed batch")
	for i, b := range blks {
		require.Equal(t, uint64(i*perBatch+1), uint64(b.MinRowID), "block %d first RowID", i)
		require.Equal(t, uint64((i+1)*perBatch+1), uint64(b.MaxRowID), "block %d exclusive end", i)
		require.Equal(t, uint32(perBatch), b.ItemCount, "block %d row count", i)
	}

	// A ranged Scan over one span returns exactly that block's rows.
	it, err := db.Scan(ctx, snap, "t", ScanOptions{Start: blks[2].MinRowID, End: blks[2].MaxRowID})
	require.NoError(t, err)
	defer it.Close()
	count := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		count++
	}
	require.NoError(t, it.Err())
	require.Equal(t, perBatch, count)

	// A table without any block reports none.
	blks, err = db.Blocks(snap, "missing")
	require.Error(t, err)
	require.Nil(t, blks)
}

// SealTable multi-block stores must survive a whole-container parse (the
// encrypted path seals the full container, which runs the page-geometry
// checks that a lazy parse skips).
func TestSealTableEncryptedWholeContainer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	key := []byte("01234567890123456789012345678901")
	db, err := Create(dir+"/enc", Options{
		Encryption: &EncryptionConfig{KeyProvider: staticTestProvider{key: key}, KeyID: "k"},
	})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "v", Type: TypeString}}))
	for i := 0; i < 3; i++ {
		require.NoError(t, tx.Insert(ctx, "t", uint64(i+1), Row{String("row")}))
		require.NoError(t, tx.SealTable("t"))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	db2, err := Open(dir+"/enc", Options{
		Encryption: &EncryptionConfig{KeyProvider: staticTestProvider{key: key}, KeyID: "k"},
	})
	require.NoError(t, err)
	defer db2.Close()

	blks, err := db2.Blocks(snap, "t")
	require.NoError(t, err)
	require.Len(t, blks, 3, "one block per sealed batch")

	var got []string
	it, err := db2.Scan(ctx, snap, "t", ScanOptions{})
	require.NoError(t, err)
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, row[0].StringOr(""))
	}
	require.NoError(t, it.Err())
	require.Equal(t, []string{"row", "row", "row"}, got)
}

type staticTestProvider struct{ key []byte }

func (p staticTestProvider) Key(context.Context, string, uint32) ([]byte, error) { return p.key, nil }
