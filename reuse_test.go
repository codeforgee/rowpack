package rowpack

import (
	"context"
	"path/filepath"
	"testing"
)

// buildReuseStore writes nRows into a FULL snapshot and depth DELTAs with
// updates/deletes, returning the open store plus the head snapshot.
func buildReuseStore(t *testing.T, base string, nRows uint64, depth int) (*Store, SnapshotID) {
	t.Helper()
	db, err := Create(base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err := w.DefineSchema(benchSchema()); err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < nRows; i++ {
		if err := w.Insert(context.Background(), 1, i+1, 1, benchRow(i)); err != nil {
			t.Fatal(err)
		}
	}
	full, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	parent := full.ID
	for d := 0; d < depth; d++ {
		w, err := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: parent})
		if err != nil {
			t.Fatal(err)
		}
		// Update every 10th row, delete every 50th.
		for i := uint64(0); i < nRows; i++ {
			switch {
			case i%50 == 49:
				if err := w.Delete(context.Background(), 1, i+1); err != nil {
					t.Fatal(err)
				}
			case i%10 == 9:
				row := benchRow(i + 1_000_000)
				if err := w.Update(context.Background(), 1, i+1, 1, row); err != nil {
					t.Fatal(err)
				}
			}
		}
		info, err := w.Commit(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		parent = info.ID
	}
	return db, parent
}

// TestGetIntoReuse verifies GetInto matches Get value-for-value, including
// along a DELTA chain with updates and deletes, and that a reused dst keeps
// working (row growth between calls).
func TestGetIntoReuse(t *testing.T) {
	base := filepath.Join(t.TempDir(), "reuse")
	db, head := buildReuseStore(t, base, 1000, 2)
	defer db.Close()

	var dst Row
	for i := uint64(0); i < 1000; i++ {
		want, werr := db.Get(context.Background(), head, 1, i+1)
		got, gerr := db.GetInto(context.Background(), head, 1, i+1, dst)
		if (werr != nil) != (gerr != nil) {
			t.Fatalf("row %d: err mismatch: %v vs %v", i+1, werr, gerr)
		}
		if werr != nil {
			continue // deleted row
		}
		if len(got) != len(want) {
			t.Fatalf("row %d: len mismatch", i+1)
		}
		for c := range want {
			if !rowValueEqual(want[c], got[c]) {
				t.Fatalf("row %d col %d mismatch: %v vs %v", i+1, c, want[c], got[c])
			}
		}
		dst = got
	}
}

// TestNextIntoReuse verifies NextInto matches Next/Row value-for-value over a
// DELTA chain, including growth from a nil dst and strict RowID order.
func TestNextIntoReuse(t *testing.T) {
	base := filepath.Join(t.TempDir(), "scanreuse")
	db, head := buildReuseStore(t, base, 1000, 2)
	defer db.Close()

	it, err := db.Scan(context.Background(), head, 1, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	it2, err := db.Scan(context.Background(), head, 1, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var dst Row
	var lastRowID RowID
	for {
		ok2 := it2.Next()
		var want Row
		var wantID RowID
		if ok2 {
			wantID = it2.RowID()
			want = it2.Row()
		}
		got, ok := it.NextInto(dst)
		if ok != ok2 {
			t.Fatalf("visibility mismatch: NextInto=%v Next=%v", ok, ok2)
		}
		if !ok {
			break
		}
		if it.RowID() != wantID {
			t.Fatalf("row id mismatch: %d vs %d", it.RowID(), wantID)
		}
		if lastRowID > 0 && it.RowID() <= lastRowID {
			t.Fatalf("row ids not strictly ascending: %d after %d", it.RowID(), lastRowID)
		}
		lastRowID = it.RowID()
		if len(got) != len(want) {
			t.Fatalf("row %d: len mismatch", wantID)
		}
		for c := range want {
			if !rowValueEqual(want[c], got[c]) {
				t.Fatalf("row %d col %d mismatch", wantID, c)
			}
		}
		dst = got
	}
	if err := it2.Err(); err != nil {
		t.Fatal(err)
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if err := it2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestNextIntoEndRowID verifies range-bounded scans terminate correctly in
// reuse mode.
func TestNextIntoEndRowID(t *testing.T) {
	base := filepath.Join(t.TempDir(), "range")
	db, full := buildReuseStore(t, base, 100, 0)
	defer db.Close()
	it, err := db.Scan(context.Background(), full, 1, ScanOptions{StartRowID: 10, EndRowID: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	n := 0
	var dst Row
	for {
		row, ok := it.NextInto(dst)
		if !ok {
			break
		}
		dst = row
		id := it.RowID()
		if id < 10 || id >= 20 {
			t.Fatalf("row %d outside range", id)
		}
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("got %d rows, want 10", n)
	}
}

// rowValueEqual compares two Values across the public getters.
func rowValueEqual(a, b Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() == b.IsNull()
	}
	if a.Type() != b.Type() {
		return false
	}
	switch a.Type() {
	case TypeString:
		x, _ := a.String()
		y, _ := b.String()
		return x == y
	case TypeBytes:
		x, _ := a.Bytes()
		y, _ := b.Bytes()
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case TypeDecimal:
		x, _ := a.Decimal()
		y, _ := b.Decimal()
		return x.Scale == y.Scale && x.Unscaled.Cmp(y.Unscaled) == 0
	case TypeBool:
		x, _ := a.Bool()
		y, _ := b.Bool()
		return x == y
	case TypeFloat64:
		x, _ := a.Float64()
		y, _ := b.Float64()
		return x == y
	case TypeDateTime:
		x, _ := a.DateTimeValue()
		y, _ := b.DateTimeValue()
		return x.Equal(y)
	default:
		x, _ := a.Int64()
		y, _ := b.Int64()
		return x == y
	}
}
