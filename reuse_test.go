package rowpack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/codec"
)

// buildReuseStore writes nRows into a FULL snapshot and depth DELTAs with
// updates/deletes, returning the open store plus the head snapshot.
func buildReuseStore(t *testing.T, base string, nRows uint64, depth int) (*Store, SnapshotID) {
	t.Helper()
	db, err := Create(base, Options{})
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

// TestGetReuse verifies Get with a reused dst matches Get with a nil dst
// value-for-value, including along a DELTA chain with updates and deletes,
// and that a reused dst keeps working (row growth between calls).
func TestGetReuse(t *testing.T) {
	base := filepath.Join(t.TempDir(), "reuse")
	db, head := buildReuseStore(t, base, 1000, 2)
	defer db.Close()

	var dst Row
	for i := uint64(0); i < 1000; i++ {
		want, werr := db.Get(context.Background(), head, 1, i+1, nil)
		got, gerr := db.Get(context.Background(), head, 1, i+1, dst)
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

// TestNextReuse verifies Scan over a DELTA chain returns rows consistent
// across independent iterators, including buffer reuse growth and strict
// RowID order.
func TestNextReuse(t *testing.T) {
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
	var lastRowID RowID
	for {
		want, ok2 := it2.Next()
		var wantID RowID
		if ok2 {
			wantID = it2.RowID()
		}
		got, ok := it.Next()
		if ok != ok2 {
			t.Fatalf("visibility mismatch: %v vs %v", ok, ok2)
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

// TestNextEndRowID verifies range-bounded scans terminate correctly in
// reuse mode.
func TestNextEndRowID(t *testing.T) {
	base := filepath.Join(t.TempDir(), "range")
	db, full := buildReuseStore(t, base, 100, 0)
	defer db.Close()
	it, err := db.Scan(context.Background(), full, 1, ScanOptions{StartRowID: 10, EndRowID: 20})
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	n := 0
	for {
		if _, ok := it.Next(); !ok {
			break
		}
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

// TestScanStringViewsSurviveArenaRotation verifies the iterator's string
// arena contract: string values handed out by an earlier Next remain valid
// (and byte-identical) after the scan advances past arena chunk rotations and
// block switches. The scan uses strings large enough to force multiple chunk
// rotations per block plus enough rows to cross block boundaries.
func TestScanStringViewsSurviveArenaRotation(t *testing.T) {
	const rows = 8000
	base := filepath.Join(t.TempDir(), "arenarot")
	db, err := Create(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err := w.DefineSchema(benchSchema()); err != nil {
		t.Fatal(err)
	}
	// Each string is ~6 KiB, so the 32 KiB arena rotates twice per row and
	// the scan spans multiple blocks.
	big := func(i uint64) string {
		b := make([]byte, 6000)
		for j := range b {
			b[j] = byte('a' + i%26)
		}
		return string(b)
	}
	for i := uint64(0); i < rows; i++ {
		r := Row{
			Uint64(i),
			String(big(i)),
			Bool(i%2 == 0),
			Int32(int32(i)),
			Float64(float64(i) * 0.5),
			DateTimeValueOf(1700000000000000000),
			DecimalValue(Decimal{Unscaled: bigI(int64(i)), Scale: 2}),
		}
		if err := w.Insert(context.Background(), 1, i+1, 1, r); err != nil {
			t.Fatal(err)
		}
	}
	full, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	it, err := db.Scan(context.Background(), full.ID, 1, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Retain one big string from every 100th row (covers chunk rotations and
	// block boundaries), then walk the rest of the scan before checking.
	type held struct {
		rowID RowID
		ok    bool
		str   string
	}
	heldStr := make(map[RowID]held)
	var cur Row
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		cur = row
		id := it.RowID()
		if id%100 == 1 {
			s, ok := cur[1].String()
			heldStr[id] = held{rowID: id, ok: ok, str: s}
		}
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	if len(heldStr) == 0 {
		t.Fatal("no strings retained")
	}
	for id, h := range heldStr {
		want := big(id - 1)
		if !h.ok || h.str != want {
			t.Fatalf("retained string for row %d corrupted after scan: got len %d want len %d",
				id, len(h.str), len(want))
		}
	}
}

// TestCodecSinkParity verifies the single decode entry codec.DecodeInto with
// a copying sink is byte-identical to the nil-sink (default) path, and that
// encode round-trips through the reusable buffers agree with the fresh
// allocate path.
func TestCodecSinkParity(t *testing.T) {
	s := benchSchema()
	cs := schemaToCodec(&s)
	row := benchRow(42)
	encFresh, err := codec.Encode(cs, []codec.Value(row), codec.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	encReuse, err := codec.EncodeInto(cs, []codec.Value(row), codec.DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(encFresh) != string(encReuse) {
		t.Fatal("fresh/reuse encode differ")
	}

	decDefault, err := codec.DecodeInto(nil, encFresh, cs, codec.DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// With a copy sink the decoded values must be byte-identical to the
	// default path.
	decSink, err := codec.DecodeInto(nil, encFresh, cs, codec.DefaultLimits(), func(p []byte) string {
		return string(p)
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range decDefault {
		if !rowValueEqual(decDefault[i], decSink[i]) {
			t.Fatalf("col %d mismatch across sink paths", i)
		}
	}
}

// schemaToCodec converts a root Schema into the codec representation for
// codec-level tests.
func schemaToCodec(s *Schema) *codec.Schema {
	cols := make([]codec.Column, len(s.Columns))
	for i, c := range s.Columns {
		cols[i] = codec.Column{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Scale: c.Scale}
	}
	return &codec.Schema{TableID: s.TableID, Version: s.Version, Name: s.Name, Columns: cols}
}
