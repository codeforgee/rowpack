package rowpack

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// apiOpen creates an empty store for the core-API tests.
func apiOpen(t *testing.T) (*Store, string) {
	t.Helper()
	base := tmpdb(t)
	db, err := Create(base, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, base
}

var apiUserCols = []Column{
	{Name: "id", Type: TypeUint64},
	{Name: "name", Type: TypeString},
}

func apiUserRow(id uint64, name string) Row {
	return Row{Uint64(id), String(name)}
}

func TestAPICreateTableAutoIDs(t *testing.T) {
	db, base := apiOpen(t)
	ctx := context.Background()

	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", apiUserCols))
	require.NoError(t, w.CreateTable("orders", []Column{{Name: "total", Type: TypeInt64}}))
	// Idempotent: same name + same columns is a no-op.
	require.NoError(t, w.CreateTable("users", apiUserCols))
	// Conflict: same name + different columns.
	require.ErrorIs(t, w.CreateTable("users", apiUserCols[:1]), ErrSchemaConflict)
	// Empty name.
	require.ErrorIs(t, w.CreateTable("", apiUserCols), ErrInvalidArgument)

	// Internal IDs allocate 1, 2 in definition order: verify via row writes.
	require.NoError(t, w.Insert(ctx, "users", 1, apiUserRow(1, "a")))
	require.NoError(t, w.Insert(ctx, "orders", 1, Row{Int64(10)}))
	snap, err := w.Commit(ctx)
	require.NoError(t, err)
	require.NotZero(t, snap)

	// Reads are by name; a second writer on the chain must not collide with
	// committed IDs.
	w2, err := db.BeginDelta(ctx, snap)
	require.NoError(t, err)
	require.NoError(t, w2.CreateTable("users", apiUserCols)) // chain hit: no-op
	require.NoError(t, w2.CreateTable("items", apiUserCols)) // new table
	require.NoError(t, w2.Insert(ctx, "items", 1, apiUserRow(9, "x")))
	snap2, err := w2.Commit(ctx)
	require.NoError(t, err)

	// Reopen: allocation continues past every committed ID.
	require.NoError(t, db.Close())
	db2, err := Open(base, Options{})
	require.NoError(t, err)
	defer func() { require.NoError(t, db2.Close()) }()
	w3, err := db2.BeginDelta(ctx, snap2)
	require.NoError(t, err)
	require.NoError(t, w3.CreateTable("third", apiUserCols))
	require.NoError(t, w3.Insert(ctx, "third", 1, apiUserRow(7, "z")))
	_, err = w3.Commit(ctx)
	require.NoError(t, err)
}

func TestAPIInsertUpdateDeleteByName(t *testing.T) {
	db, _ := apiOpen(t)
	ctx := context.Background()

	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", apiUserCols))
	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, w.Insert(ctx, "users", i, apiUserRow(i, "n")))
	}
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	// Unknown table name.
	err = w.Insert(ctx, "missing", 1, apiUserRow(1, "x"))
	require.ErrorIs(t, err, ErrNotFound)

	d, err := db.BeginDelta(ctx, full)
	require.NoError(t, err)
	require.NoError(t, d.Update(ctx, "users", 2, apiUserRow(2, "updated")))
	require.NoError(t, d.Delete(ctx, "users", 3))
	require.NoError(t, d.Insert(ctx, "users", 6, apiUserRow(6, "new")))
	delta, err := d.Commit(ctx)
	require.NoError(t, err)

	// Merged view.
	r, err := db.Get(ctx, full, "users", 2, nil)
	require.NoError(t, err)
	v, _ := r[1].String()
	require.Equal(t, "n", v)
	r, err = db.Get(ctx, delta, "users", 2, nil)
	require.NoError(t, err)
	v, _ = r[1].String()
	require.Equal(t, "updated", v)
	_, err = db.Get(ctx, delta, "users", 3, nil)
	require.ErrorIs(t, err, ErrNotFound)
	ok, err := db.Exists(ctx, delta, "users", 6)
	require.NoError(t, err)
	require.True(t, ok)

	// Scan merged + range.
	it, err := db.Scan(ctx, delta, "users", ScanOptions{})
	require.NoError(t, err)
	var got []RowID
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, it.RowID())
		require.NotEqual(t, ChangeDelete, it.ChangeType()) // 合并视图不产生 DELETE
		_ = row[1]
	}
	require.NoError(t, it.Err())
	require.NoError(t, it.Close())
	require.Equal(t, []RowID{1, 2, 4, 5, 6}, got)

	it, err = db.Scan(ctx, delta, "users", ScanOptions{Start: 2, End: 5})
	require.NoError(t, err)
	got = nil
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		got = append(got, it.RowID())
	}
	require.NoError(t, it.Close())
	require.Equal(t, []RowID{2, 4}, got)
}

func TestAPIBlocksAndScanBlocks(t *testing.T) {
	base := tmpdb(t)
	db, err := Create(base, Options{BlockSize: 4096})
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	ctx := context.Background()

	pad := make([]byte, 64)
	for i := range pad {
		pad[i] = 'x'
	}

	// Full snapshot with enough rows to span several 4 KiB blocks.
	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", apiUserCols))
	const n = 300
	for i := uint64(1); i <= n; i++ {
		require.NoError(t, w.Insert(ctx, "users", i, Row{Uint64(i), String(string(pad))}))
	}
	full, err := w.Commit(ctx)
	require.NoError(t, err)

	blocks, err := db.Blocks(ctx, full, "users")
	require.NoError(t, err)
	require.NotEmpty(t, blocks)
	require.Less(t, 1, len(blocks), "expected multiple blocks")
	// Ascending BlockID, contiguous key ranges, counts sum to n.
	var total uint32
	prevMax := RowID(0)
	for i, b := range blocks {
		if i > 0 {
			require.Greater(t, b.BlockID, blocks[i-1].BlockID)
			require.Equal(t, prevMax, b.MinRowID)
		}
		total += b.ItemCount
		prevMax = b.MaxRowID
	}
	require.Equal(t, uint32(n), total)
	require.Equal(t, RowID(n+1), blocks[len(blocks)-1].MaxRowID)
	// First block covers the lowest keys.
	require.Equal(t, RowID(1), blocks[0].MinRowID)

	// ScanBlocks: the raw change stream over a block subrange.
	lo, hi := blocks[0].BlockID, blocks[len(blocks)-1].BlockID+1
	it, err := db.ScanBlocks(ctx, full, "users", lo, hi)
	require.NoError(t, err)
	count := 0
	maxSeen := RowID(0)
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		count++
		require.Equal(t, ChangeInsert, it.ChangeType())
		require.NotZero(t, it.RowID())
		if it.RowID() > maxSeen {
			maxSeen = it.RowID()
		}
		require.Len(t, row, 2)
	}
	require.NoError(t, it.Err())
	require.NoError(t, it.Close())
	require.Equal(t, int(n), count)
	require.Equal(t, RowID(n), maxSeen)

	// A single block scans exactly ItemCount records.
	it, err = db.ScanBlocks(ctx, full, "users", blocks[1].BlockID, blocks[1].BlockID+1)
	require.NoError(t, err)
	c1 := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		c1++
	}
	require.NoError(t, it.Close())
	require.Equal(t, int(blocks[1].ItemCount), c1)

	// Out-of-range [lo, hi).
	_, err = db.ScanBlocks(ctx, full, "users", blocks[len(blocks)-1].BlockID+1, hi+1)
	require.ErrorIs(t, err, ErrInvalidArgument)
	_, err = db.ScanBlocks(ctx, full, "users", 5, 5)
	require.ErrorIs(t, err, ErrInvalidArgument)

	// DELTA: Blocks lists only the delta's own changes; ScanBlocks shows the
	// raw UPDATE/DELETE stream without merging.
	d, err := db.BeginDelta(ctx, full)
	require.NoError(t, err)
	require.NoError(t, d.Update(ctx, "users", 1, apiUserRow(1, "u1")))
	require.NoError(t, d.Delete(ctx, "users", 2))
	delta, err := d.Commit(ctx)
	require.NoError(t, err)

	dblocks, err := db.Blocks(ctx, delta, "users")
	require.NoError(t, err)
	require.Len(t, dblocks, 1)
	require.Equal(t, RowID(1), dblocks[0].MinRowID)
	require.Equal(t, RowID(3), dblocks[0].MaxRowID)

	it, err = db.ScanBlocks(ctx, delta, "users", dblocks[0].BlockID, dblocks[0].BlockID+1)
	require.NoError(t, err)
	type rec struct {
		id RowID
		ct ChangeType
	}
	var recs []rec
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		recs = append(recs, rec{it.RowID(), it.ChangeType()})
		if it.ChangeType() == ChangeDelete {
			require.Nil(t, row)
		}
	}
	require.NoError(t, it.Close())
	require.Equal(t, []rec{{1, ChangeUpdate}, {2, ChangeDelete}}, recs)

	// Blocks of the FULL snapshot remain unchanged after the delta.
	again, err := db.Blocks(ctx, full, "users")
	require.NoError(t, err)
	require.Len(t, again, len(blocks))
}

func TestAPIEmptyDeltaBlocks(t *testing.T) {
	db, _ := apiOpen(t)
	ctx := context.Background()

	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", apiUserCols))
	require.NoError(t, w.Insert(ctx, "users", 1, apiUserRow(1, "a")))
	fullID, err := w.Commit(ctx)
	require.NoError(t, err)

	blocks, err := db.Blocks(ctx, fullID, "missing")
	require.ErrorIs(t, err, ErrNotFound)
	require.Nil(t, blocks)

	_, err = db.Blocks(ctx, 999, "users")
	require.ErrorIs(t, err, ErrNotFound)
}
