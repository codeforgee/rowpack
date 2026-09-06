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
	t.Cleanup(func() {
		testUUIDOverride = nil
		testNowOverride = 0
	})
	opts := DefaultOptions()
	opts.BlockSize = 1024
	db, err := Create(base, opts)
	if err != nil {
		t.Fatal(err)
	}
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
			t.Fatal(err)
		}
	}
	// Oversize row (> 1024 target block) on table 3.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i)
	}
	if err := w.Insert(context.Background(), 3, 1, 1, Row{Bytes(big)}); err != nil {
		t.Fatal(err)
	}
	full, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// DELTA: update + delete + insert.
	d, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: full.ID})
	if err := d.Update(context.Background(), 1, 2, 1, Row{Uint64(2), String("updated-2"), Bool(true), DecimalValue(Decimal{Unscaled: bigI(777), Scale: 2})}); err != nil {
		t.Fatal(err)
	}
	if err := d.Delete(context.Background(), 1, 3); err != nil {
		t.Fatal(err)
	}
	if err := d.Insert(context.Background(), 1, 31, 1, Row{Uint64(31), String("new-31"), Bool(false), DecimalValue(Decimal{Unscaled: bigI(1), Scale: 2})}); err != nil {
		t.Fatal(err)
	}
	delta, err := d.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Empty DELTA.
	e, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: delta.ID})
	empty, err := e.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_ = empty
}

func bigI(v int64) *big.Int {
	return big.NewInt(v)
}

// TestGoldenStoreSamples locks the FULL+DELTA+empty store and verifies it.
func TestGoldenStoreSamples(t *testing.T) {
	base := filepath.Join(t.TempDir(), "golden-store")
	if *updateGolden {
		buildFullDeltaStore(t, base)
		for _, ext := range []string{".rpk", ".rpi"} {
			data, err := os.ReadFile(base + ext)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath("full-delta-store"+ext), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	// Copy the golden samples into a temp dir and open them.
	for _, ext := range []string{".rpk", ".rpi"} {
		data, err := os.ReadFile(goldenPath("full-delta-store" + ext))
		if err != nil {
			t.Fatalf("read golden %s: %v (regenerate with make golden)", ext, err)
		}
		if err := os.WriteFile(base+ext, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(base, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snaps, err := db.ListSnapshots(context.Background())
	if err != nil || len(snaps) != 3 {
		t.Fatalf("snapshots: %v %v", snaps, err)
	}
	// FULL content.
	r, err := db.Get(context.Background(), snaps[0].ID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := r[1].String(); n != "user-1" {
		t.Fatalf("full row1 name = %q", n)
	}
	// DELTA content.
	if n, _ := func() (string, bool) {
		rr, e := db.Get(context.Background(), snaps[1].ID, 1, 2)
		if e != nil {
			return "", false
		}
		s, _ := rr[1].String()
		return s, true
	}(); n != "updated-2" {
		t.Fatalf("delta row2 name = %q", n)
	}
	if _, err := db.Get(context.Background(), snaps[1].ID, 1, 3); err == nil {
		t.Fatal("delta row3 not deleted")
	}
	if _, err := db.Get(context.Background(), snaps[1].ID, 1, 31); err != nil {
		t.Fatalf("delta row31: %v", err)
	}
	// Empty delta sees delta state.
	if _, err := db.Get(context.Background(), snaps[2].ID, 1, 31); err != nil {
		t.Fatalf("empty delta row31: %v", err)
	}
	// Oversize row round trip.
	big, err := db.Get(context.Background(), snaps[0].ID, 3, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := big[0].Bytes()
	if len(b) != 4096 || b[0] != 0 || b[255] != 255 {
		t.Fatalf("oversize row corrupted: len=%d", len(b))
	}
	// CreatedAt from the golden (deterministic override).
	if !snaps[0].CreatedAt.Equal(time.Unix(0, 1757400000000000000)) {
		t.Fatalf("createdAt = %v", snaps[0].CreatedAt)
	}
}
