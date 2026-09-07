package rowpack

import (
	"context"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func ctx(t *testing.T) context.Context { return context.Background() }

func TestCreateOpenPaths(t *testing.T) {
	base := filepath.Join(tmpdb(t), "db")
	_, err := Create(base+".rpk", Options{})
	require.Error(t, err, "accepted .rpk extension")
	_, err = Create(base+".rpi", Options{})
	require.Error(t, err, "accepted .rpi extension")
	_, err = Open(base, Options{})
	require.Error(t, err, "opened nonexistent store")
	db, err := Create(base, Options{})
	require.NoError(t, err)
	require.NoError(t, db.Close())
	// Create must not overwrite.
	_, err = Create(base, Options{})
	require.Error(t, err, "Create overwrote existing store")
	// Reopen read-write and read-only.
	db2, err := Open(base, Options{})
	require.NoError(t, err)
	require.False(t, db2.ReadOnly(), "opened read-only unexpectedly")
	db2.Close()
	ro, err := Open(base, Options{ReadOnly: true})
	require.NoError(t, err)
	require.True(t, ro.ReadOnly(), "read-only flag lost")
	ro.Close()
}

// TestM5VerticalSlice implements the M5 vertical acceptance scenario: create a
// store, write Header/Table/Column/PK metadata, write two Rows blocks, commit
// a FULL snapshot, close/reopen, then read metadata and random rows with
// value-by-value comparison.
func TestM5VerticalSlice(t *testing.T) {
	base := filepath.Join(tmpdb(t), "v")
	opts := Options{}
	opts.BlockSize = 512 // small blocks force multiple Rows blocks
	db, err := Create(base, opts)
	require.NoError(t, err)

	w, err := db.BeginSnapshot(ctx(t), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)

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
	require.NoError(t, w.DefineSchema(schema))
	// Define a second table.
	require.NoError(t, w.DefineSchema(Schema{TableID: 2, Version: 1, Name: "orders", Columns: []Column{
		{Name: "order_id", Type: TypeUint64},
		{Name: "user_id", Type: TypeUint64},
	}}))

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
		require.NoError(t, w.Insert(ctx(t), 1, i+1, 1, row))
		require.NoError(t, w.Insert(ctx(t), 2, i+1, 1, Row{Uint64(i + 1), Uint64(i + 1)}))
	}

	full, err := w.Commit(ctx(t))
	require.NoError(t, err)
	require.Equal(t, SnapshotFull, full.Type, "bad commit info: %+v", full)
	require.Equal(t, uint64(1), full.ID, "bad commit info: %+v", full)
	require.Equal(t, uint64(4000), full.ChangeCount, "bad commit info: %+v", full)
	require.NoError(t, db.Close())

	// Reopen and verify.
	db2, err := Open(base, opts)
	require.NoError(t, err)
	defer db2.Close()

	snapshots, err := db2.ListSnapshots(ctx(t))
	require.NoError(t, err, "snapshots: %v %v", snapshots, err)
	require.Len(t, snapshots, 1, "snapshots: %v %v", snapshots, err)

	// Schema round trip.
	gotSchema, err := db2.Schema(ctx(t), full.ID, 1, 1)
	require.NoError(t, err)
	require.Len(t, gotSchema.Columns, len(schema.Columns), "schema columns %d != %d", len(gotSchema.Columns), len(schema.Columns))
	for i := range schema.Columns {
		g, e := gotSchema.Columns[i], schema.Columns[i]
		require.Equal(t, e.Name, g.Name, "column %d mismatch: %+v vs %+v", i, g, e)
		require.Equal(t, e.Type, g.Type, "column %d mismatch: %+v vs %+v", i, g, e)
		require.Equal(t, e.Nullable, g.Nullable, "column %d mismatch: %+v vs %+v", i, g, e)
		require.Equal(t, e.Scale, g.Scale, "column %d mismatch: %+v vs %+v", i, g, e)
	}

	// Latest schema and tables.
	latest, err := db2.LatestSchema(ctx(t), full.ID, 1)
	require.NoError(t, err, "latest schema: %v %v", latest, err)
	require.Equal(t, uint32(1), latest.Version, "latest schema: %v %v", latest, err)
	tables, err := db2.Tables(ctx(t), full.ID)
	require.NoError(t, err, "tables: %v %v", tables, err)
	require.Len(t, tables, 2, "tables: %v %v", tables, err)

	// Random row reads with value-by-value comparison.
	for i := uint64(0); i < 2000; i += 37 {
		row, err := db2.Get(ctx(t), full.ID, 1, i+1, nil)
		require.NoError(t, err, "get row %d", i+1)
		// Compare each value.
		v, _ := row[0].Uint64()
		require.Equal(t, i+1, v, "row %d id = %d", i+1, v)
		s, _ := row[1].String()
		require.Equal(t, nameFor(i), s, "row %d name = %q", i+1, s)
		b, _ := row[2].Bool()
		require.Equal(t, i%2 == 0, b, "row %d active", i+1)
		age, _ := row[3].Int32()
		require.Equal(t, int32(i), age, "row %d age", i+1)
		f, _ := row[4].Float64()
		require.Equal(t, float64(i)*1.5, f, "row %d score", i+1)
		em, _ := row[5].String()
		require.Equal(t, i%50 == 0, em == "", "row %d email null mismatch", i+1)
		ts, _ := row[6].DateTimeValue()
		want := created.Add(time.Duration(i) * time.Second)
		require.Equal(t, want.UnixNano(), ts.UnixNano(), "row %d created mismatch", i+1)
		d, _ := row[7].Decimal()
		require.Equal(t, int32(2), d.Scale, "row %d balance = %v", i+1, d)
		require.Equal(t, int64(i*100+99), d.Unscaled.Int64(), "row %d balance = %v", i+1, d)
	}

	// Exists.
	ok, err := db2.Exists(ctx(t), full.ID, 1, 1)
	require.NoError(t, err, "exists: %v %v", ok, err)
	require.True(t, ok, "exists: %v %v", ok, err)
	ok, err = db2.Exists(ctx(t), full.ID, 1, 99999)
	require.NoError(t, err, "exists miss: %v %v", ok, err)
	require.False(t, ok, "exists miss: %v %v", ok, err)

	// Get of a nonexistent row returns ErrNotFound.
	_, err = db2.Get(ctx(t), full.ID, 1, 99999, nil)
	require.Error(t, err, "get nonexistent row succeeded")

	// Stats sanity.
	stats := db2.Stats()
	require.Equal(t, uint64(1), stats.Snapshots, "stats: %+v", stats)
	require.GreaterOrEqual(t, stats.Blocks, uint64(2), "stats: %+v", stats)
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
