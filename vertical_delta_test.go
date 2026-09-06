package rowpack

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestM6DeltaHistory verifies AC-002: FULL + 3 DELTAs with INSERT/UPDATE/
// DELETE, historical reads at every point, and that old snapshots are not
// affected by later commits.
func TestM6DeltaHistory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "m6")
	opts := DefaultOptions()
	opts.BlockSize = 4096
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}

	// FULL: table 1, rows 1..10, name="v0-<id>".
	fullW, err := db.BeginSnapshot(ctx(t), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fullW.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t1", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
	}}); err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= 10; i++ {
		if err := fullW.Insert(ctx(t), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("v0-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	full, err := fullW.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}

	// DELTA 1: INSERT 11, UPDATE 3.
	d1, err := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: full.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.Insert(ctx(t), 1, 11, 1, Row{Uint64(11), String("v1-11")}); err != nil {
		t.Fatal(err)
	}
	if err := d1.Update(ctx(t), 1, 3, 1, Row{Uint64(3), String("v1-3")}); err != nil {
		t.Fatal(err)
	}
	d1info, err := d1.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}

	// DELTA 2: DELETE 5, INSERT 12.
	d2, err := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: d1info.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := d2.Delete(ctx(t), 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := d2.Insert(ctx(t), 1, 12, 1, Row{Uint64(12), String("v2-12")}); err != nil {
		t.Fatal(err)
	}
	d2info, err := d2.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}

	// DELTA 3: UPDATE 7, DELETE 9.
	d3, err := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: d2info.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := d3.Update(ctx(t), 1, 7, 1, Row{Uint64(7), String("v3-7")}); err != nil {
		t.Fatal(err)
	}
	if err := d3.Delete(ctx(t), 1, 9); err != nil {
		t.Fatal(err)
	}
	d3info, err := d3.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}

	// Snapshot chain.
	got, err := db.ListSnapshots(ctx(t))
	if err != nil || len(got) != 4 {
		t.Fatalf("snapshots: %v %v", got, err)
	}

	// Historical reads: snapshot FULL sees original state.
	for i := uint64(1); i <= 10; i++ {
		row, err := db.Get(ctx(t), full.ID, 1, i)
		if err != nil {
			t.Fatalf("full get %d: %v", i, err)
		}
		name, _ := row[1].String()
		if name != fmt.Sprintf("v0-%d", i) {
			t.Fatalf("full row %d name %q", i, name)
		}
	}
	if _, err := db.Get(ctx(t), full.ID, 1, 11); err == nil {
		t.Fatal("full snapshot sees row 11")
	}

	// D1: row 3 updated, row 11 inserted, row 5 still present.
	r3, _ := db.Get(ctx(t), d1info.ID, 1, 3)
	if n, _ := r3[1].String(); n != "v1-3" {
		t.Fatalf("d1 row 3 = %q", n)
	}
	r11, _ := db.Get(ctx(t), d1info.ID, 1, 11)
	if n, _ := r11[1].String(); n != "v1-11" {
		t.Fatalf("d1 row 11 = %q", n)
	}
	if _, err := db.Get(ctx(t), d1info.ID, 1, 5); err != nil {
		t.Fatalf("d1 row 5 missing: %v", err)
	}

	// D2: row 5 deleted, row 12 present.
	if _, err := db.Get(ctx(t), d2info.ID, 1, 5); err == nil {
		t.Fatal("d2 row 5 not deleted")
	}
	if ok, _ := db.Exists(ctx(t), d2info.ID, 1, 5); ok {
		t.Fatal("d2 exists(5) true")
	}
	r12, _ := db.Get(ctx(t), d2info.ID, 1, 12)
	if n, _ := r12[1].String(); n != "v2-12" {
		t.Fatalf("d2 row 12 = %q", n)
	}

	// D3: row 7 updated, row 9 deleted; old snapshots unaffected.
	r7, _ := db.Get(ctx(t), d3info.ID, 1, 7)
	if n, _ := r7[1].String(); n != "v3-7" {
		t.Fatalf("d3 row 7 = %q", n)
	}
	if _, err := db.Get(ctx(t), d3info.ID, 1, 9); err == nil {
		t.Fatal("d3 row 9 not deleted")
	}
	// D2 must still see row 9 and v2 state of row 7.
	if _, err := db.Get(ctx(t), d2info.ID, 1, 9); err != nil {
		t.Fatalf("d2 row 9 affected by later delete: %v", err)
	}
	r7d2, _ := db.Get(ctx(t), d2info.ID, 1, 7)
	if n, _ := r7d2[1].String(); n != "v0-7" {
		t.Fatalf("d2 row 7 = %q (later update leaked)", n)
	}

	// Invalid parent.
	if _, err := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: 999}); err == nil {
		t.Fatal("delta with unknown parent accepted")
	}
	// FULL with parent rejected.
	if _, err := db.BeginSnapshot(ctx(t), SnapshotFull, SnapshotOptions{Parent: full.ID}); err == nil {
		t.Fatal("full with parent accepted")
	}
	// Duplicate row in delta rejected.
	dup, err := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: d3info.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := dup.Insert(ctx(t), 1, 3, 1, Row{Uint64(3), String("x")}); err == nil {
		t.Fatal("delta insert of existing row accepted in strict mode")
	}
	if err := dup.Abort(); err != nil {
		t.Fatal(err)
	}
	// ValidationNone skips the parent existence check.
	vnone := DefaultOptions()
	vnone.Validation = ValidationNone
	_ = vnone

	// Empty DELTA (allowed).
	empty, err := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: d3info.ID})
	if err != nil {
		t.Fatal(err)
	}
	emptyInfo, err := empty.Commit(ctx(t))
	if err != nil {
		t.Fatalf("empty delta commit: %v", err)
	}
	if emptyInfo.ChangeCount != 0 {
		t.Fatalf("empty delta has %d changes", emptyInfo.ChangeCount)
	}

	// Reopen: history must survive, DELTA chain intact.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	r7r, err := db2.Get(ctx(t), d3info.ID, 1, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r7r[1].String(); n != "v3-7" {
		t.Fatalf("reopened d3 row 7 = %q", n)
	}
	if _, err := db2.Get(ctx(t), d3info.ID, 1, 9); err == nil {
		t.Fatal("reopened d3 row 9 not deleted")
	}
	// Latest snapshot is the empty delta.
	latest, err := db2.LatestSnapshot(ctx(t))
	if err != nil || latest.ID != emptyInfo.ID {
		t.Fatalf("latest = %+v %v", latest, err)
	}
}

// TestM6ValidationNone verifies ValidationNone disables only the parent-view
// existence check, not format/schema/duplicate checks.
func TestM6ValidationNone(t *testing.T) {
	base := filepath.Join(t.TempDir(), "vnone")
	opts := DefaultOptions()
	opts.Validation = ValidationNone
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := db.BeginSnapshot(ctx(t), SnapshotFull, SnapshotOptions{})
	w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{{Name: "id", Type: TypeUint64}, {Name: "s", Type: TypeString}}})
	if err := w.Insert(ctx(t), 1, 1, 1, Row{Uint64(1), String("a")}); err != nil {
		t.Fatal(err)
	}
	full, err := w.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	// DELTA insert of an existing row is allowed under ValidationNone.
	d, _ := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: full.ID})
	if err := d.Insert(ctx(t), 1, 1, 1, Row{Uint64(1), String("overwrite")}); err != nil {
		t.Fatalf("ValidationNone should allow parent-overwrite insert: %v", err)
	}
	dinfo, err := d.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.Get(ctx(t), dinfo.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := row[1].String(); n != "overwrite" {
		t.Fatalf("row = %q", n)
	}
	// Duplicate within the same snapshot is still rejected.
	d2, _ := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: dinfo.ID})
	if err := d2.Insert(ctx(t), 1, 2, 1, Row{Uint64(2), String("b")}); err != nil {
		t.Fatal(err)
	}
	if err := d2.Insert(ctx(t), 1, 2, 1, Row{Uint64(2), String("c")}); err == nil {
		t.Fatal("same-snapshot duplicate accepted even under ValidationNone")
	}
	_ = d2.Abort()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, db *Store, snapshot SnapshotID, table TableID, opts ScanOptions) []string {
	t.Helper()
	it, err := db.Scan(ctx(t), snapshot, table, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var out []string
	for it.Next() {
		row := it.Row()
		id, _ := row[0].Uint64()
		name, _ := row[1].String()
		out = append(out, fmt.Sprintf("%d:%s", id, name))
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestM6Scan verifies strictly ascending RowID output, override/tombstone
// filtering, ranges, and that Scan memory does not depend on table size.
func TestM6Scan(t *testing.T) {
	base := filepath.Join(t.TempDir(), "scan")
	opts := DefaultOptions()
	opts.BlockSize = 512
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	w, _ := db.BeginSnapshot(ctx(t), SnapshotFull, SnapshotOptions{})
	w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "t", Columns: []Column{{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}}})
	for i := uint64(1); i <= 100; i++ {
		if err := w.Insert(ctx(t), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("a-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	full, err := w.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	// DELTA: update evens, delete multiples of 7.
	d, _ := db.BeginSnapshot(ctx(t), SnapshotDelta, SnapshotOptions{Parent: full.ID})
	for i := uint64(2); i <= 100; i += 2 {
		if err := d.Update(ctx(t), 1, i, 1, Row{Uint64(i), String(fmt.Sprintf("b-%d", i))}); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(7); i <= 100; i += 14 {
		if err := d.Delete(ctx(t), 1, i); err != nil {
			t.Fatal(err)
		}
	}
	dinfo, err := d.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}

	// Full scan at delta: ascending, no tombstones (odd multiples of 7:
	// 7,21,...,91 = 7 rows), evens updated.
	rows := collect(t, db, dinfo.ID, 1, ScanOptions{})
	if len(rows) != 100-7 {
		t.Fatalf("scan len %d, want 93", len(rows))
	}
	prev := uint64(0)
	for i, r := range rows {
		var id uint64
		fmt.Sscanf(r, "%d:", &id)
		if id <= prev {
			t.Fatalf("scan not ascending at %d: %v", i, rows[i])
		}
		prev = id
		if id%2 == 0 {
			if r != fmt.Sprintf("%d:b-%d", id, id) {
				t.Fatalf("even row not updated: %s", r)
			}
		} else {
			if r != fmt.Sprintf("%d:a-%d", id, id) {
				t.Fatalf("odd row wrong: %s", r)
			}
		}
	}

	// Scan at FULL snapshot still sees original (no tombstones/updates).
	rowsFull := collect(t, db, full.ID, 1, ScanOptions{})
	if len(rowsFull) != 100 {
		t.Fatalf("full scan len %d", len(rowsFull))
	}

	// Range scan [30, 40): rows 30..39, with 35 (odd multiple of 7) deleted.
	rng := collect(t, db, dinfo.ID, 1, ScanOptions{StartRowID: 30, EndRowID: 40})
	want := 40 - 30 - 1 // 35 deleted
	if len(rng) != want {
		t.Fatalf("range scan len %d, want %d", len(rng), want)
	}
	if rng[0] != "30:b-30" {
		t.Fatalf("range start %s", rng[0])
	}
	if rng[len(rng)-1] != "39:a-39" {
		t.Fatalf("range end %s", rng[len(rng)-1])
	}

	// Range from 95 (inclusive) to end.
	rng2 := collect(t, db, dinfo.ID, 1, ScanOptions{StartRowID: 95})
	if rng2[0] != "95:a-95" {
		t.Fatalf("start range %s", rng2[0])
	}
	if rng2[len(rng2)-1] != "100:b-100" {
		t.Fatalf("end range %s", rng2[len(rng2)-1])
	}

	// Invalid range.
	if _, err := db.Scan(ctx(t), dinfo.ID, 1, ScanOptions{StartRowID: 50, EndRowID: 40}); err == nil {
		t.Fatal("invalid range accepted")
	}

	// Scan a nonexistent table yields no rows.
	it, err := db.Scan(ctx(t), dinfo.ID, 99, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	if it.Next() {
		t.Fatal("nonexistent table yielded rows")
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
