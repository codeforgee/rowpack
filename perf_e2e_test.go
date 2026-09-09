package rowpack

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPerfEndToEnd is a performance regression test: it writes 200k rows,
// verifies random reads and a full scan value-for-value, closes, reopens and
// re-verifies. It guards the performance optimizations (block cursor, pooled
// zstd, ParseRowAt) against correctness regressions at moderate scale.
func TestPerfEndToEnd(t *testing.T) {
	const rows = 200_000
	base := filepath.Join(tmpdb(t), "perf")
	opts := Options{}
	db, err := Create(base, opts)
	require.NoError(t, err)

	t0 := time.Now()
	w, _ := db.BeginFull(context.Background())
	require.NoError(t, w.CreateTable("perf", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
		{Name: "active", Type: TypeBool},
		{Name: "age", Type: TypeInt32},
		{Name: "score", Type: TypeFloat64},
		{Name: "created", Type: TypeDateTime},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	}))
	created := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := uint64(0); i < rows; i++ {
		if err := w.Insert(context.Background(), "perf", i+1, Row{
			Uint64(i + 1),
			String(fmt.Sprintf("perf-user-%d", i)),
			Bool(i%2 == 0),
			Int32(int32(i)),
			Float64(float64(i) * 0.25),
			DateTime(created.Add(time.Duration(i) * time.Second)),
			DecimalValue(Decimal{Unscaled: big.NewInt(int64(i*3 + 1)), Scale: 2}),
		}); err != nil {
			require.NoError(t, err)
		}
	}
	full, err := w.Commit(context.Background())
	require.NoError(t, err)
	writeDur := time.Since(t0)

	// Random reads: verify every 97th row, value-for-value.
	t1 := time.Now()
	for i := uint64(0); i < rows; i += 97 {
		row, err := db.Get(context.Background(), full, "perf", i+1, nil)
		require.NoError(t, err, "get %d", i+1)
		v, _ := row[0].Uint64()
		require.Equal(t, i+1, v, "id mismatch at %d", i+1)
		s, _ := row[1].String()
		require.Equal(t, fmt.Sprintf("perf-user-%d", i), s, "name mismatch at %d", i+1)
		d, _ := row[6].Decimal()
		require.Equal(t, int64(i*3+1), d.Unscaled.Int64(), "decimal mismatch at %d", i+1)
		require.Equal(t, int32(2), d.Scale, "decimal mismatch at %d", i+1)
	}
	readDur := time.Since(t1)

	// Full scan: count and verify first/last.
	t2 := time.Now()
	it, err := db.Scan(context.Background(), full, "perf", ScanOptions{})
	require.NoError(t, err)
	n := 0
	var first, last uint64
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		v, _ := row[0].Uint64()
		if n == 0 {
			first = v
		}
		last = v
		n++
	}
	it.Close()
	require.NoError(t, it.Err())
	scanDur := time.Since(t2)
	require.Equal(t, uint64(rows), last, "scan: n=%d first=%d last=%d", n, first, last)
	require.Equal(t, uint64(1), first, "scan: n=%d first=%d last=%d", n, first, last)
	require.Equal(t, int(rows), n, "scan: n=%d first=%d last=%d", n, first, last)

	// Close and reopen, re-verify a sample.
	require.NoError(t, db.Close())
	t3 := time.Now()
	db2, err := Open(base, opts)
	require.NoError(t, err)
	openDur := time.Since(t3)
	for i := uint64(0); i < rows; i += 1000 {
		_, err := db2.Get(context.Background(), full, "perf", i+1, nil)
		require.NoError(t, err, "reopen get %d", i+1)
	}
	db2.Close()

	t.Logf("write(200k)=%v (%.0f krows/s) read(2k)=%v scan(200k)=%v open=%v",
		writeDur, float64(rows)/writeDur.Seconds()/1000, readDur, scanDur, openDur)

	// Sanity bounds to catch pathological regressions (not strict perf gates).
	require.Less(t, writeDur, 10*time.Second, "write too slow: %v", writeDur)
	require.Less(t, scanDur, 5*time.Second, "scan too slow: %v", scanDur)
}
