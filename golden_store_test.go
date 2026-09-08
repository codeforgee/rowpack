package rowpack

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// updateGolden regenerates store golden samples. Enable with
// `go test ./... -run TestGolden -args -update-golden` (see Makefile).
var updateGolden = flag.Bool("update-golden", false, "regenerate golden files")

func goldenPath(name string) string {
	return filepath.Join("testdata", "golden", name)
}

// buildFullDeltaStore writes a deterministic FULL + DELTA + empty DELTA store
// with an oversize row, used both to generate and to verify the golden
// samples.
func buildFullDeltaStore(t *testing.T, base string) {
	t.Helper()
	uuid := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	testUUIDOverride = &uuid
	testNowOverride = 1757400000000000000
	nonce := uint64(0x4E4F4E4345474F4C) // "NOCEGOL" — fixed so golden bytes are deterministic
	testNonceOverride = &nonce
	t.Cleanup(func() {
		testUUIDOverride = nil
		testNowOverride = 0
		testNonceOverride = nil
	})
	opts := Options{}
	opts.BlockSize = 1024
	db, err := Create(base, opts)
	require.NoError(t, err)
	// FULL with three tables.
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	w.DefineSchema(Schema{TableID: 1, Version: 1, Name: "users", Columns: []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}, {Name: "active", Type: TypeBool},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	}})
	w.DefineSchema(Schema{TableID: 2, Version: 1, Name: "empty", Columns: []Column{{Name: "x", Type: TypeInt64}}})
	w.DefineSchema(Schema{TableID: 3, Version: 1, Name: "oversize", Columns: []Column{{Name: "blob", Type: TypeBytes}}})
	for i := uint64(1); i <= 30; i++ {
		if err := w.Insert(context.Background(), 1, i, 1, Row{
			Uint64(i), String(fmt.Sprintf("user-%d", i)), Bool(i%2 == 0),
			DecimalValue(Decimal{Unscaled: bigI(int64(i * 100)), Scale: 2}),
		}); err != nil {
			require.NoError(t, err)
		}
	}
	// Oversize row (> 1024 target block) on table 3.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i)
	}
	require.NoError(t, w.Insert(context.Background(), 3, 1, 1, Row{Bytes(big)}))
	full, err := w.Commit(context.Background())
	require.NoError(t, err)
	// DELTA: update + delete + insert.
	d, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full.ID})
	require.NoError(t, d.Update(context.Background(), 1, 2, 1, Row{Uint64(2), String("updated-2"), Bool(true), DecimalValue(Decimal{Unscaled: bigI(777), Scale: 2})}))
	require.NoError(t, d.Delete(context.Background(), 1, 3))
	require.NoError(t, d.Insert(context.Background(), 1, 31, 1, Row{Uint64(31), String("new-31"), Bool(false), DecimalValue(Decimal{Unscaled: bigI(1), Scale: 2})}))
	delta, err := d.Commit(context.Background())
	require.NoError(t, err)
	// Empty DELTA.
	e, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: delta.ID})
	empty, err := e.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_ = empty
}

func bigI(v int64) *big.Int {
	return big.NewInt(v)
}

// TestGoldenStoreSamples locks the FULL+DELTA+empty store and verifies it.
func TestGoldenStoreSamples(t *testing.T) {
	base := filepath.Join(tmpdb(t), "golden-store")
	if *updateGolden {
		buildFullDeltaStore(t, base)
		data, err := os.ReadFile(base + ".rpk")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(goldenPath("full-delta-store.rpk"), data, 0o644))
		return
	}
	data, err := os.ReadFile(goldenPath("full-delta-store.rpk"))
	require.NoError(t, err, "read golden (regenerate with make golden): %v", err)
	require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))
	db, err := Open(base, Options{})
	require.NoError(t, err)
	defer db.Close()
	snaps, err := db.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "snapshots: %v %v", snaps, err)
	// FULL content.
	r, err := db.Get(context.Background(), snaps[0].ID, 1, 1, nil)
	require.NoError(t, err)
	if n, _ := r[1].String(); n != "user-1" {
		require.Fail(t, "full row1 name = %q", n)
	}
	// DELTA content.
	if n, _ := func() (string, bool) {
		rr, e := db.Get(context.Background(), snaps[1].ID, 1, 2, nil)
		if e != nil {
			return "", false
		}
		s, _ := rr[1].String()
		return s, true
	}(); n != "updated-2" {
		require.Fail(t, "delta row2 name = %q", n)
	}
	_, err = db.Get(context.Background(), snaps[1].ID, 1, 3, nil)
	require.Error(t, err, "delta row3 not deleted")
	_, err = db.Get(context.Background(), snaps[1].ID, 1, 31, nil)
	require.NoError(t, err, "delta row31")
	// Empty delta sees delta state.
	_, err = db.Get(context.Background(), snaps[2].ID, 1, 31, nil)
	require.NoError(t, err, "empty delta row31")
	// Oversize row round trip.
	big, err := db.Get(context.Background(), snaps[0].ID, 3, 1, nil)
	require.NoError(t, err)
	b, _ := big[0].Bytes()
	require.Len(t, b, 4096, "oversize row corrupted: len=%d", len(b))
	require.Equal(t, byte(0), b[0])
	require.Equal(t, byte(255), b[255])
	// CreatedAt from the golden (deterministic override).
	require.True(t, snaps[0].CreatedAt.Equal(time.Unix(0, 1757400000000000000)), "createdAt = %v", snaps[0].CreatedAt)
}
