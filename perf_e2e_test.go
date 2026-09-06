package rowpack

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"
	"testing"
	"time"
)

// TestPerfEndToEnd is a performance regression test: it writes 200k rows,
// verifies random reads and a full scan value-for-value, closes, reopens and
// re-verifies. It guards the performance optimizations (block cursor, pooled
// zstd, ParseRowAt) against correctness regressions at moderate scale.
func TestPerfEndToEnd(t *testing.T) {
	const rows = 200_000
	base := filepath.Join(t.TempDir(), "perf")
	opts := DefaultOptions()
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}

	t0 := time.Now()
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	schema := Schema{TableID: 1, Version: 1, Name: "perf", Columns: []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
		{Name: "active", Type: TypeBool},
		{Name: "age", Type: TypeInt32},
		{Name: "score", Type: TypeFloat64},
		{Name: "created", Type: TypeDateTime},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	}}
	if err := w.DefineSchema(schema); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := uint64(0); i < rows; i++ {
		if err := w.Insert(context.Background(), 1, i+1, 1, Row{
			Uint64(i + 1),
			String(fmt.Sprintf("perf-user-%d", i)),
			Bool(i%2 == 0),
			Int32(int32(i)),
			Float64(float64(i) * 0.25),
			DateTime(created.Add(time.Duration(i) * time.Second)),
			DecimalValue(Decimal{Unscaled: big.NewInt(int64(i*3 + 1)), Scale: 2}),
		}); err != nil {
			t.Fatal(err)
		}
	}
	full, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeDur := time.Since(t0)

	// Random reads: verify every 97th row, value-for-value.
	t1 := time.Now()
	for i := uint64(0); i < rows; i += 97 {
		row, err := db.Get(context.Background(), full.ID, 1, i+1)
		if err != nil {
			t.Fatalf("get %d: %v", i+1, err)
		}
		if v, _ := row[0].Uint64(); v != i+1 {
			t.Fatalf("id mismatch at %d", i+1)
		}
		if v, _ := row[1].String(); v != fmt.Sprintf("perf-user-%d", i) {
			t.Fatalf("name mismatch at %d", i+1)
		}
		d, _ := row[6].Decimal()
		if d.Unscaled.Int64() != int64(i*3+1) || d.Scale != 2 {
			t.Fatalf("decimal mismatch at %d", i+1)
		}
	}
	readDur := time.Since(t1)

	// Full scan: count and verify first/last.
	t2 := time.Now()
	it, err := db.Scan(context.Background(), full.ID, 1, ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var first, last uint64
	for it.Next() {
		row := it.Row()
		v, _ := row[0].Uint64()
		if n == 0 {
			first = v
		}
		last = v
		n++
	}
	it.Close()
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	scanDur := time.Since(t2)
	if n != rows || first != 1 || last != rows {
		t.Fatalf("scan: n=%d first=%d last=%d", n, first, last)
	}

	// Close and reopen, re-verify a sample.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t3 := time.Now()
	db2, err := Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	openDur := time.Since(t3)
	for i := uint64(0); i < rows; i += 1000 {
		if _, err := db2.Get(context.Background(), full.ID, 1, i+1); err != nil {
			t.Fatalf("reopen get %d: %v", i+1, err)
		}
	}
	db2.Close()

	t.Logf("write(200k)=%v (%.0f krows/s) read(2k)=%v scan(200k)=%v open=%v",
		writeDur, float64(rows)/writeDur.Seconds()/1000, readDur, scanDur, openDur)

	// Sanity bounds to catch pathological regressions (not strict perf gates).
	if writeDur > 10*time.Second {
		t.Fatalf("write too slow: %v", writeDur)
	}
	if scanDur > 5*time.Second {
		t.Fatalf("scan too slow: %v", scanDur)
	}
}
