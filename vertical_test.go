package rowpack

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func ctx(t *testing.T) context.Context { return context.Background() }

func TestCreateOpenPaths(t *testing.T) {
	base := filepath.Join(t.TempDir(), "db")
	if _, err := Create(base+".rpk", Options{}); err == nil {
		t.Fatal("accepted .rpk extension")
	}
	if _, err := Create(base+".rpi", Options{}); err == nil {
		t.Fatal("accepted .rpi extension")
	}
	_, err := Open(base, Options{})
	if err == nil {
		t.Fatal("opened nonexistent store")
	}
	db, err := Create(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Create must not overwrite.
	if _, err := Create(base, Options{}); err == nil {
		t.Fatal("Create overwrote existing store")
	}
	// Reopen read-write and read-only.
	db2, err := Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if db2.ReadOnly() {
		t.Fatal("opened read-only unexpectedly")
	}
	db2.Close()
	ro, err := Open(base, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if !ro.ReadOnly() {
		t.Fatal("read-only flag lost")
	}
	ro.Close()
}

// TestM5VerticalSlice implements the M5 vertical acceptance scenario: create a
// store, write Header/Table/Column/PK metadata, write two Rows blocks, commit
// a FULL snapshot, close/reopen, then read metadata and random rows with
// value-by-value comparison.
func TestM5VerticalSlice(t *testing.T) {
	base := filepath.Join(t.TempDir(), "v")
	opts := Options{}
	opts.BlockSize = 512 // small blocks force multiple Rows blocks
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}

	w, err := db.BeginSnapshot(ctx(t), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Schema with 10 base types.
	schema := Schema{
		TableID: 1,
		Version: 1,
		Name:    "users",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "name", Type: TypeString},
			{Name: "active", Type: TypeBool},
			{Name: "age", Type: TypeInt32},
			{Name: "score", Type: TypeFloat64},
			{Name: "email", Type: TypeString, Nullable: true},
			{Name: "created", Type: TypeDateTime},
			{Name: "balance", Type: TypeDecimal, Scale: 2},
		},
	}
	if err := w.DefineSchema(schema); err != nil {
		t.Fatal(err)
	}
	// Define a second table.
	if err := w.DefineSchema(Schema{TableID: 2, Version: 1, Name: "orders", Columns: []Column{
		{Name: "order_id", Type: TypeUint64},
		{Name: "user_id", Type: TypeUint64},
	}}); err != nil {
		t.Fatal(err)
	}

	// Write 2000 rows across two tables; block size 512 forces multiple blocks.
	created := time.Date(2024, 1, 2, 3, 4, 5, 678, time.UTC)
	for i := uint64(0); i < 2000; i++ {
		row := Row{
			Uint64(i + 1),
			String(nameFor(i)),
			Bool(i%2 == 0),
			Int32(int32(i)),
			Float64(float64(i) * 1.5),
			nullOrEmail(i),
			DateTime(created.Add(time.Duration(i) * time.Second)),
			DecimalValue(Decimal{Unscaled: big.NewInt(int64(i*100 + 99)), Scale: 2}),
		}
		if err := w.Insert(ctx(t), 1, i+1, 1, row); err != nil {
			t.Fatal(err)
		}
		if err := w.Insert(ctx(t), 2, i+1, 1, Row{Uint64(i + 1), Uint64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}

	full, err := w.Commit(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if full.Type != SnapshotFull || full.ID != 1 || full.ChangeCount != 4000 {
		t.Fatalf("bad commit info: %+v", full)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen and verify.
	db2, err := Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	snapshots, err := db2.ListSnapshots(ctx(t))
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshots: %v %v", snapshots, err)
	}

	// Schema round trip.
	gotSchema, err := db2.Schema(ctx(t), full.ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotSchema.Columns) != len(schema.Columns) {
		t.Fatalf("schema columns %d != %d", len(gotSchema.Columns), len(schema.Columns))
	}
	for i := range schema.Columns {
		g, e := gotSchema.Columns[i], schema.Columns[i]
		if g.Name != e.Name || g.Type != e.Type || g.Nullable != e.Nullable || g.Scale != e.Scale {
			t.Fatalf("column %d mismatch: %+v vs %+v", i, g, e)
		}
	}

	// Latest schema and tables.
	latest, err := db2.LatestSchema(ctx(t), full.ID, 1)
	if err != nil || latest.Version != 1 {
		t.Fatalf("latest schema: %v %v", latest, err)
	}
	tables, err := db2.Tables(ctx(t), full.ID)
	if err != nil || len(tables) != 2 {
		t.Fatalf("tables: %v %v", tables, err)
	}

	// Random row reads with value-by-value comparison.
	for i := uint64(0); i < 2000; i += 37 {
		row, err := db2.Get(ctx(t), full.ID, 1, i+1, nil)
		if err != nil {
			t.Fatalf("get row %d: %v", i+1, err)
		}
		// Compare each value.
		if v, _ := row[0].Uint64(); v != i+1 {
			t.Fatalf("row %d id = %d", i+1, v)
		}
		if v, _ := row[1].String(); v != nameFor(i) {
			t.Fatalf("row %d name = %q", i+1, v)
		}
		if v, _ := row[2].Bool(); v != (i%2 == 0) {
			t.Fatalf("row %d active", i+1)
		}
		if v, _ := row[3].Int32(); v != int32(i) {
			t.Fatalf("row %d age", i+1)
		}
		if v, _ := row[4].Float64(); v != float64(i)*1.5 {
			t.Fatalf("row %d score", i+1)
		}
		em, _ := row[5].String()
		if (i%50 == 0) != (em == "") {
			t.Fatalf("row %d email null mismatch", i+1)
		}
		ts, _ := row[6].DateTimeValue()
		want := created.Add(time.Duration(i) * time.Second)
		if ts.UnixNano() != want.UnixNano() {
			t.Fatalf("row %d created mismatch", i+1)
		}
		d, _ := row[7].Decimal()
		if d.Scale != 2 || d.Unscaled.Int64() != int64(i*100+99) {
			t.Fatalf("row %d balance = %v", i+1, d)
		}
	}

	// Exists.
	ok, err := db2.Exists(ctx(t), full.ID, 1, 1)
	if err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	ok, err = db2.Exists(ctx(t), full.ID, 1, 99999)
	if err != nil || ok {
		t.Fatalf("exists miss: %v %v", ok, err)
	}

	// Get of a nonexistent row returns ErrNotFound.
	if _, err := db2.Get(ctx(t), full.ID, 1, 99999, nil); err == nil {
		t.Fatal("get nonexistent row succeeded")
	}

	// Stats sanity.
	stats := db2.Stats()
	if stats.Snapshots != 1 || stats.Blocks < 2 {
		t.Fatalf("stats: %+v", stats)
	}
}

func nameFor(i uint64) string {
	switch i % 3 {
	case 0:
		return "alice"
	case 1:
		return "张三"
	}
	return "bob@example.com"
}

func nullOrEmail(i uint64) Value {
	if i%50 == 0 {
		return Null()
	}
	return String("user" + itoa(i) + "@x.com")
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var b [24]byte
	pos := len(b)
	for v > 0 {
		pos--
		b[pos] = byte('0' + v%10)
		v /= 10
	}
	return string(b[pos:])
}

var _ = os.Remove
var _ = fileformat.MagicDataFile
