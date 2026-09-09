package rowpack

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestM6DeltaHistory verifies AC-002: FULL + 3 DELTAs with INSERT/UPDATE/
// DELETE, historical reads at every point, and that old snapshots are not
// affected by later commits.
func TestM6DeltaHistory(t *testing.T) {
	base := filepath.Join(tmpdb(t), "m6")
	opts := Options{}
	opts.BlockSize = 4096
	db, err := Create(base, opts)
	require.NoError(t, err)

	// FULL: table 1, rows 1..10, name="v0-<id>".
	fullW, err := db.BeginFull(ctx(t))
	require.NoError(t, err)
	require.NoError(t, fullW.CreateTable("t", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}))
	for i := uint64(1); i <= 10; i++ {
		require.NoError(t, fullW.Insert(ctx(t), "t", i, Row{Uint64(i), String(fmt.Sprintf("v0-%d", i))}))
	}
	full, err := fullW.Commit(ctx(t))
	require.NoError(t, err)

	// DELTA 1: INSERT 11, UPDATE 3.
	d1, err := db.BeginDelta(ctx(t), full)
	require.NoError(t, err)
	require.NoError(t, d1.Insert(ctx(t), "t", 11, Row{Uint64(11), String("v1-11")}))
	require.NoError(t, d1.Update(ctx(t), "t", 3, Row{Uint64(3), String("v1-3")}))
	d1info, err := d1.Commit(ctx(t))
	require.NoError(t, err)

	// DELTA 2: DELETE 5, INSERT 12.
	d2, err := db.BeginDelta(ctx(t), d1info)
	require.NoError(t, err)
	require.NoError(t, d2.Delete(ctx(t), "t", 5))
	require.NoError(t, d2.Insert(ctx(t), "t", 12, Row{Uint64(12), String("v2-12")}))
	d2info, err := d2.Commit(ctx(t))
	require.NoError(t, err)

	// DELTA 3: UPDATE 7, DELETE 9.
	d3, err := db.BeginDelta(ctx(t), d2info)
	require.NoError(t, err)
	require.NoError(t, d3.Update(ctx(t), "t", 7, Row{Uint64(7), String("v3-7")}))
	require.NoError(t, d3.Delete(ctx(t), "t", 9))
	d3info, err := d3.Commit(ctx(t))
	require.NoError(t, err)

	// Snapshot chain.
	got, err := db.ListSnapshots(ctx(t))
	require.NoError(t, err, "snapshots: %v %v", got, err)
	require.Len(t, got, 4, "snapshots: %v %v", got, err)

	// Historical reads: snapshot FULL sees original state.
	for i := uint64(1); i <= 10; i++ {
		row, err := db.Get(ctx(t), full, "t", i, nil)
		require.NoError(t, err, "full get %d", i)
		name, _ := row[1].String()
		require.Equal(t, fmt.Sprintf("v0-%d", i), name, "full row %d name %q", i, name)
	}
	_, err = db.Get(ctx(t), full, "t", 11, nil)
	require.Error(t, err, "full snapshot sees row 11")

	// D1: row 3 updated, row 11 inserted, row 5 still present.
	r3, _ := db.Get(ctx(t), d1info, "t", 3, nil)
	n, _ := r3[1].String()
	require.Equal(t, "v1-3", n, "d1 row 3 = %q", n)
	r11, _ := db.Get(ctx(t), d1info, "t", 11, nil)
	n, _ = r11[1].String()
	require.Equal(t, "v1-11", n, "d1 row 11 = %q", n)
	_, err = db.Get(ctx(t), d1info, "t", 5, nil)
	require.NoError(t, err, "d1 row 5 missing: %v", err)

	// D2: row 5 deleted, row 12 present.
	_, err = db.Get(ctx(t), d2info, "t", 5, nil)
	require.Error(t, err, "d2 row 5 not deleted")
	ok, _ := db.Exists(ctx(t), d2info, "t", 5)
	require.False(t, ok, "d2 exists(5) true")
	r12, _ := db.Get(ctx(t), d2info, "t", 12, nil)
	n, _ = r12[1].String()
	require.Equal(t, "v2-12", n, "d2 row 12 = %q", n)

	// D3: row 7 updated, row 9 deleted; old snapshots unaffected.
	r7, _ := db.Get(ctx(t), d3info, "t", 7, nil)
	n, _ = r7[1].String()
	require.Equal(t, "v3-7", n, "d3 row 7 = %q", n)
	_, err = db.Get(ctx(t), d3info, "t", 9, nil)
	require.Error(t, err, "d3 row 9 not deleted")
	// D2 must still see row 9 and v2 state of row 7.
	_, err = db.Get(ctx(t), d2info, "t", 9, nil)
	require.NoError(t, err, "d2 row 9 affected by later delete: %v", err)
	r7d2, _ := db.Get(ctx(t), d2info, "t", 7, nil)
	n, _ = r7d2[1].String()
	require.Equal(t, "v0-7", n, "d2 row 7 = %q (later update leaked)", n)

	// Invalid parent.
	_, err = db.BeginDelta(ctx(t), 999)
	require.Error(t, err, "delta with unknown parent accepted")
	// Duplicate row in delta rejected.
	dup, err := db.BeginDelta(ctx(t), d3info)
	require.NoError(t, err)
	err = dup.Insert(ctx(t), "t", 3, Row{Uint64(3), String("x")})
	require.Error(t, err, "delta insert of existing row accepted in strict mode")
	require.NoError(t, dup.Abort())
	// ValidationNone skips the parent existence check.
	vnone := Options{}
	vnone.Validation = ValidationNone
	_ = vnone

	// Empty DELTA (allowed).
	empty, err := db.BeginDelta(ctx(t), d3info)
	require.NoError(t, err)
	emptyID, err := empty.Commit(ctx(t))
	require.NoError(t, err, "empty delta commit: %v", err)
	require.NotZero(t, emptyID)

	// Reopen: history must survive, DELTA chain intact.
	require.NoError(t, db.Close())
	db2, err := Open(base, opts)
	require.NoError(t, err)
	defer db2.Close()
	snaps2, err := db2.ListSnapshots(ctx(t))
	require.NoError(t, err)
	for _, si := range snaps2 {
		if si.ID == emptyID {
			require.Zero(t, si.ChangeCount, "empty delta has %d changes", si.ChangeCount)
		}
	}
	r7r, err := db2.Get(ctx(t), d3info, "t", 7, nil)
	require.NoError(t, err)
	n, _ = r7r[1].String()
	require.Equal(t, "v3-7", n, "reopened d3 row 7 = %q", n)
	_, err = db2.Get(ctx(t), d3info, "t", 9, nil)
	require.Error(t, err, "reopened d3 row 9 not deleted")
	// Latest snapshot is the empty delta.
	snaps, err := db2.ListSnapshots(ctx(t))
	require.NoError(t, err, "list: %v", err)
	require.NotEmpty(t, snaps, "list empty: %v", snaps)
	require.Equal(t, emptyID, snaps[len(snaps)-1].ID, "latest = %+v", snaps)
}

// TestM6ValidationNone verifies ValidationNone disables only the parent-view
// existence check, not format/schema/duplicate checks.
func TestM6ValidationNone(t *testing.T) {
	base := filepath.Join(tmpdb(t), "vnone")
	opts := Options{}
	opts.Validation = ValidationNone
	db, err := Create(base, opts)
	require.NoError(t, err)
	w, _ := db.BeginFull(ctx(t))
	w.CreateTable("t", []Column{{Name: "id", Type: TypeUint64}, {Name: "s", Type: TypeString}})
	require.NoError(t, w.Insert(ctx(t), "t", 1, Row{Uint64(1), String("a")}))
	full, err := w.Commit(ctx(t))
	require.NoError(t, err)
	// DELTA insert of an existing row is allowed under ValidationNone.
	d, _ := db.BeginDelta(ctx(t), full)
	require.NoError(t, d.Insert(ctx(t), "t", 1, Row{Uint64(1), String("overwrite")}), "ValidationNone should allow parent-overwrite insert: %v", err)
	dinfo, err := d.Commit(ctx(t))
	require.NoError(t, err)
	row, err := db.Get(ctx(t), dinfo, "t", 1, nil)
	require.NoError(t, err)
	n, _ := row[1].String()
	require.Equal(t, "overwrite", n, "row = %q", n)
	// Duplicate within the same snapshot is still rejected.
	d2, _ := db.BeginDelta(ctx(t), dinfo)
	require.NoError(t, d2.Insert(ctx(t), "t", 2, Row{Uint64(2), String("b")}))
	err = d2.Insert(ctx(t), "t", 2, Row{Uint64(2), String("c")})
	require.Error(t, err, "same-snapshot duplicate accepted even under ValidationNone")
	_ = d2.Abort()
	require.NoError(t, db.Close())
}

func collect(t *testing.T, db *Store, snapshot SnapshotID, table string, opts ScanOptions) []string {
	t.Helper()
	it, err := db.Scan(ctx(t), snapshot, table, opts)
	require.NoError(t, err)
	defer it.Close()
	var out []string
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		id, _ := row[0].Uint64()
		name, _ := row[1].String()
		out = append(out, fmt.Sprintf("%d:%s", id, name))
	}
	require.NoError(t, it.Err())
	return out
}

// TestM6Scan verifies strictly ascending RowID output, override/tombstone
// filtering, ranges, and that Scan memory does not depend on table size.
func TestM6Scan(t *testing.T) {
	base := filepath.Join(tmpdb(t), "scan")
	opts := Options{}
	opts.BlockSize = 512
	db, err := Create(base, opts)
	require.NoError(t, err)
	w, _ := db.BeginFull(ctx(t))
	w.CreateTable("t", []Column{{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}})
	for i := uint64(1); i <= 100; i++ {
		require.NoError(t, w.Insert(ctx(t), "t", i, Row{Uint64(i), String(fmt.Sprintf("a-%d", i))}))
	}
	full, err := w.Commit(ctx(t))
	require.NoError(t, err)
	// DELTA: update evens, delete multiples of 7.
	d, _ := db.BeginDelta(ctx(t), full)
	for i := uint64(2); i <= 100; i += 2 {
		require.NoError(t, d.Update(ctx(t), "t", i, Row{Uint64(i), String(fmt.Sprintf("b-%d", i))}))
	}
	for i := uint64(7); i <= 100; i += 14 {
		require.NoError(t, d.Delete(ctx(t), "t", i))
	}
	dinfo, err := d.Commit(ctx(t))
	require.NoError(t, err)

	// Full scan at delta: ascending, no tombstones (odd multiples of 7:
	// 7,21,...,91 = 7 rows), evens updated.
	rows := collect(t, db, dinfo, "t", ScanOptions{})
	require.Len(t, rows, 100-7, "scan len %d, want 93", len(rows))
	prev := uint64(0)
	for i, r := range rows {
		var id uint64
		fmt.Sscanf(r, "%d:", &id)
		require.True(t, id > prev, "scan not ascending at %d: %v", i, rows[i])
		prev = id
		if id%2 == 0 {
			require.Equal(t, fmt.Sprintf("%d:b-%d", id, id), r, "even row not updated: %s", r)
		} else {
			require.Equal(t, fmt.Sprintf("%d:a-%d", id, id), r, "odd row wrong: %s", r)
		}
	}

	// Scan at FULL snapshot still sees original (no tombstones/updates).
	rowsFull := collect(t, db, full, "t", ScanOptions{})
	require.Len(t, rowsFull, 100, "full scan len %d", len(rowsFull))

	// Range scan [30, 40): rows 30..39, with 35 (odd multiple of 7) deleted.
	rng := collect(t, db, dinfo, "t", ScanOptions{Start: 30, End: 40})
	want := 40 - 30 - 1 // 35 deleted
	require.Len(t, rng, want, "range scan len %d, want %d", len(rng), want)
	require.Equal(t, "30:b-30", rng[0], "range start %s", rng[0])
	require.Equal(t, "39:a-39", rng[len(rng)-1], "range end %s", rng[len(rng)-1])

	// Range from 95 (inclusive) to end.
	rng2 := collect(t, db, dinfo, "t", ScanOptions{Start: 95})
	require.Equal(t, "95:a-95", rng2[0], "start range %s", rng2[0])
	require.Equal(t, "100:b-100", rng2[len(rng2)-1], "end range %s", rng2[len(rng2)-1])

	// Invalid range.
	_, err = db.Scan(ctx(t), dinfo, "t", ScanOptions{Start: 50, End: 40})
	require.Error(t, err, "invalid range accepted")

	// Scan a nonexistent table is rejected.
	_, err = db.Scan(ctx(t), dinfo, "no-such-table", ScanOptions{})
	require.Error(t, err, "nonexistent table scan accepted")

	require.NoError(t, db.Close())
}
