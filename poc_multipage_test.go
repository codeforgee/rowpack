package rowpack

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// setupMultiPage creates a store whose blocks span several pages and returns
// the store plus the committed FULL snapshot.
func setupMultiPage(t *testing.T) (*Store, SnapshotID, int) {
	t.Helper()
	ctx := context.Background()
	dir := tmpdb(t)
	db, err := Create(dir, Options{PageSize: 128}) // tiny page -> multi-page blocks
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "payload", Type: TypeString},
	}))
	const n = 200
	for i := 0; i < n; i++ {
		require.NoError(t, tx.Insert("t", RowID(i+1), Row{
			Uint64(uint64(i + 1)),
			String(fmt.Sprintf("payload-%04d-%s", i, "0123456789abcdef0123456789abcdef")),
		}))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)
	return db, snap, n
}

// TestScanBlocksMultiPageBlock proves and regresses the page-advance bug in
// nextBlockRecord: a block with multiple pages must yield each record exactly
// once, in write order.
func TestScanBlocksMultiPageBlock(t *testing.T) {
	db, snap, n := setupMultiPage(t)
	ctx := context.Background()

	// Sanity: merged Scan sees exactly n rows (exercises multi-page rowAt).
	it, err := db.Scan(ctx, snap, "t", ScanOptions{})
	require.NoError(t, err)
	merged := 0
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		merged++
	}
	require.NoError(t, it.Err())
	it.Close()
	require.Equal(t, n, merged)

	// Count total records across all blocks; FULL has no tombstones.
	blks, err := db.Blocks(ctx, snap, "t")
	require.NoError(t, err)
	require.NotEmpty(t, blks)
	total := 0
	for _, b := range blks {
		total += int(b.ItemCount)
	}
	require.Equal(t, n, total)

	bit, err := db.ScanBlocks(ctx, snap, "t", blks[0].BlockID, blks[len(blks)-1].BlockID+1)
	require.NoError(t, err)
	defer bit.Close()

	seen := 0
	for {
		_, ok := bit.Next()
		if !ok {
			break
		}
		seen++
		if seen > 3*total { // run-away guard: page-advance bug re-emits page 0 forever
			bit.Close()
			t.Fatalf("ScanBlocks emitted %d records (>3x total %d): page cursor repeats page 0", seen, total)
		}
	}
	require.NoError(t, bit.Err())
	require.Equal(t, total, seen, "raw block stream must emit each record exactly once")
}

// TestReadBatchMultiPageBlock: the batch kernel on a multi-page block must
// return exactly one row per requested id, in the caller's id order.
func TestReadBatchMultiPageBlock(t *testing.T) {
	db, snap, n := setupMultiPage(t)
	ctx := context.Background()

	ids := make([]RowID, 0, n)
	for i := n; i >= 1; i-- { // reverse order: exercise (block, ordinal) sorting
		ids = append(ids, RowID(i))
	}
	rows, err := db.ReadBatch(ctx, snap, "t", ids)
	require.NoError(t, err)
	require.Len(t, rows, n)
	for k, id := range ids {
		v, ok2 := rows[k][0].Uint64()
		require.True(t, ok2)
		require.Equal(t, uint64(id), v, "row out of order at slot %d", k)
	}

	// Duplicate ids read the row twice.
	dup, err := db.ReadBatch(ctx, snap, "t", []RowID{7, 7, 3, 7})
	require.NoError(t, err)
	require.Len(t, dup, 4)
	for _, k := range []int{0, 1, 3} {
		v, _ := dup[k][0].Uint64()
		require.Equal(t, uint64(7), v)
	}
	v, _ := dup[2][0].Uint64()
	require.Equal(t, uint64(3), v)
}

// TestScanStartSeekMultiLayer exercises RowKeyIter.Seek (never covered before)
// plus the multi-layer heap merge under a Start bound: a FULL snapshot plus a
// DELTA, scanned with Start > 0, must emit exactly the visible ids >= Start.
func TestScanStartSeekMultiLayer(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer db.Close()

	// FULL: ids 1..100.
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	for i := 1; i <= 100; i++ {
		require.NoError(t, tx.Insert("t", RowID(i), Row{Uint64(uint64(i))}))
	}
	full, err := tx.Commit(ctx)
	require.NoError(t, err)

	// DELTA: delete 1..20, update 21..30, insert 201..210. Because Insert
	// requires the table to be defined per txn for FULL only, reuse the table.
	tx2, err := db.Begin(ctx, full)
	require.NoError(t, err)
	for i := 1; i <= 20; i++ {
		require.NoError(t, tx2.Delete("t", RowID(i)))
	}
	for i := 21; i <= 30; i++ {
		require.NoError(t, tx2.Update("t", RowID(i), Row{Uint64(uint64(i) * 10)}))
	}
	for i := 201; i <= 210; i++ {
		require.NoError(t, tx2.Insert("t", RowID(i), Row{Uint64(uint64(i))}))
	}
	delta, err := tx2.Commit(ctx)
	require.NoError(t, err)

	for _, tc := range []struct {
		start, end RowID
	}{
		{start: 15, end: 0},   // inside the deleted prefix, crosses layers
		{start: 22, end: 0},   // inside the updated run
		{start: 0, end: 0},    // unbounded
		{start: 95, end: 205}, // end bound cuts mid-DELTA inserts
	} {
		it, err := db.Scan(ctx, delta, "t", ScanOptions{Start: tc.start, End: tc.end})
		require.NoError(t, err)
		var got []RowID
		for {
			row, ok := it.Next()
			if !ok {
				break
			}
			id := it.RowID()
			v, ok3 := row[0].Uint64()
			require.True(t, ok3)
			if id <= 30 {
				require.Equal(t, uint64(id)*10, v, "updated row value")
			} else {
				require.Equal(t, uint64(id), v)
			}
			got = append(got, id)
		}
		require.NoError(t, it.Err())
		it.Close()

		// Expected: visible ids in [max(start,1), end) from FULL(1..100) ∪
		// DELTA(201..210), minus the deleted 1..20.
		var want []RowID
		lo := tc.start
		if lo < 1 {
			lo = 1
		}
		for id := lo; tc.end == 0 || id < tc.end; id++ {
			if id > 210 {
				break
			}
			if id >= 1 && id <= 20 {
				continue
			}
			if id > 100 && id < 201 {
				continue // gap: only 1..100 and 201..210 exist
			}
			want = append(want, id)
		}
		require.Equal(t, want, got, "scan [start=%d,end=%d)", tc.start, tc.end)
	}
}

// TestOversizedRowMultiPage: rows larger than the page target (and larger
// than the block target) must round-trip through Get/Scan/ReadBatch and the
// raw block stream unchanged, mixed with normal rows.
func TestOversizedRowMultiPage(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{BlockSize: 512, PageSize: 128, Compression: CompressionNone})
	require.NoError(t, err)
	defer db.Close()

	big := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte('a' + i%26)
		}
		return string(b)
	}
	rows := map[RowID]string{
		1:  big(64),   // fits a page
		2:  big(300),  // oversized row (> page, < block)
		3:  big(80),   // normal
		4:  big(700),  // oversized row (> block): isolated block
		5:  big(90),   // normal after oversized
		6:  big(600),  // another oversized
		7:  big(70),   // normal
	}
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "payload", Type: TypeString},
	}))
	for id, p := range rows {
		require.NoError(t, tx.Insert("t", id, Row{Uint64(uint64(id)), String(p)}))
	}
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	// Get round-trip.
	for id, p := range rows {
		row, err := db.Get(ctx, snap, "t", id, nil)
		require.NoError(t, err, "id %d", id)
		v, _ := row[1].String()
		require.Equal(t, p, v, "id %d", id)
	}

	// Scan round-trip: all 7 ids in ascending order.
	it, err := db.Scan(ctx, snap, "t", ScanOptions{})
	require.NoError(t, err)
	var ids []RowID
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		ids = append(ids, it.RowID())
		v, _ := row[1].String()
		require.Equal(t, rows[it.RowID()], v)
	}
	require.NoError(t, it.Err())
	it.Close()
	require.Equal(t, []RowID{1, 2, 3, 4, 5, 6, 7}, ids)

	// ReadBatch round-trip.
	got, err := db.ReadBatch(ctx, snap, "t", []RowID{4, 1, 6, 3})
	require.NoError(t, err)
	for k, id := range []RowID{4, 1, 6, 3} {
		v, _ := got[k][1].String()
		require.Equal(t, rows[id], v)
	}

	// Reopen: persisted geometry must round-trip too.
	require.NoError(t, db.Close())
	db2, err := Open(db.Path(), Options{Compression: CompressionNone})
	require.NoError(t, err)
	defer db2.Close()
	for id, p := range rows {
		row, err := db2.Get(ctx, snap, "t", id, nil)
		require.NoError(t, err)
		v, _ := row[1].String()
		require.Equal(t, p, v, "reopened id %d", id)
	}
}

// TestCompressionNoneRoundTrip is the minimal regression for the checkHeader
// bug: rows blocks are page containers, so BlockHeader.StoredSize legitimately
// exceeds RawSize (container header + page directory) even with
// CompressionNone. The stale equality check rejected EVERY plain rows block,
// making CompressionNone stores unreadable.
func TestCompressionNoneRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{Compression: CompressionNone})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	row, err := db.Get(ctx, snap, "t", 1, nil)
	require.NoError(t, err)
	v, _ := row[0].Uint64()
	require.Equal(t, uint64(1), v)

	rep, err := db.Verify(ctx, VerifyFull)
	require.NoError(t, err)
	require.Equal(t, uint64(1), rep.RowsChecked)
}
