package rowpack

import (
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGoldenManifest(t *testing.T) {
	want := map[string]string{
		"empty-store.rpk":            "cd0a96b72ad858d8bceb946b4ae77b1667b6bf17b9d79d72c9b282a52ddc34f7",
		"rows-payload-all-types.bin": "db95f863c3250ee64f55e09400dd4327a790814ca2846c7accbd0b3eca1f7433",
		"full-delta-store.rpk":       "71b2c6956c3025230f1fe99d10261c37991eb223c3dccfc766c9fea7ec348ac3",
		"encrypted-store.rpk":        "afaefc7673d8e32e1d5d4854c956b070febc50fc3c0edcde76347984c17fd3d8",
	}
	for name, digest := range want {
		data, err := os.ReadFile(goldenPath(name))
		require.NoError(t, err, "read golden %s", name)
		require.Equal(t, digest, fmt.Sprintf("%x", sha256.Sum256(data)),
			"golden %s changed; disk-format changes require versioning and explicit manifest review", name)
	}
}

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
	w, _ := db.BeginFull(context.Background())
	w.CreateTable("users", []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}, {Name: "active", Type: TypeBool},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	})
	w.CreateTable("empty", []Column{{Name: "x", Type: TypeInt64}})
	w.CreateTable("oversize", []Column{{Name: "blob", Type: TypeBytes}})
	for i := uint64(1); i <= 30; i++ {
		if err := w.Insert(context.Background(), "users", i, Row{
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
	require.NoError(t, w.Insert(context.Background(), "oversize", 1, Row{Bytes(big)}))
	full, err := w.Commit(context.Background())
	require.NoError(t, err)
	// DELTA: update + delete + insert.
	d, _ := db.BeginDelta(context.Background(), full)
	require.NoError(t, d.Update(context.Background(), "users", 2, Row{Uint64(2), String("updated-2"), Bool(true), DecimalValue(Decimal{Unscaled: bigI(777), Scale: 2})}))
	require.NoError(t, d.Delete(context.Background(), "users", 3))
	require.NoError(t, d.Insert(context.Background(), "users", 31, Row{Uint64(31), String("new-31"), Bool(false), DecimalValue(Decimal{Unscaled: bigI(1), Scale: 2})}))
	delta, err := d.Commit(context.Background())
	require.NoError(t, err)
	// Empty DELTA.
	e, _ := db.BeginDelta(context.Background(), delta)
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
	buildFullDeltaStore(t, base)
	generated, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	if *updateGolden {
		require.NoError(t, os.WriteFile(goldenPath("full-delta-store.rpk"), generated, 0o644))
		return
	}
	data, err := os.ReadFile(goldenPath("full-delta-store.rpk"))
	require.NoError(t, err, "read golden (regenerate with make golden): %v", err)
	require.Equal(t, data, generated, "writer output differs from locked golden (regenerate with make golden only for an intentional format change)")
	// Re-open a copy of the locked bytes, keeping semantic compatibility
	// verification independent from the just-generated file.
	base = filepath.Join(tmpdb(t), "golden-store-open")
	require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))
	db, err := Open(base, Options{})
	require.NoError(t, err)
	defer db.Close()
	snaps, err := db.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "snapshots: %v %v", snaps, err)
	// FULL content.
	r, err := db.Get(context.Background(), snaps[0].ID, "users", 1, nil)
	require.NoError(t, err)
	if n, _ := r[1].String(); n != "user-1" {
		require.Fail(t, "full row1 name = %q", n)
	}
	// DELTA content.
	if n, _ := func() (string, bool) {
		rr, e := db.Get(context.Background(), snaps[1].ID, "users", 2, nil)
		if e != nil {
			return "", false
		}
		s, _ := rr[1].String()
		return s, true
	}(); n != "updated-2" {
		require.Fail(t, "delta row2 name = %q", n)
	}
	_, err = db.Get(context.Background(), snaps[1].ID, "users", 3, nil)
	require.Error(t, err, "delta row3 not deleted")
	_, err = db.Get(context.Background(), snaps[1].ID, "users", 31, nil)
	require.NoError(t, err, "delta row31")
	// Empty delta sees delta state.
	_, err = db.Get(context.Background(), snaps[2].ID, "users", 31, nil)
	require.NoError(t, err, "empty delta row31")
	// Oversize row round trip.
	big, err := db.Get(context.Background(), snaps[0].ID, "oversize", 1, nil)
	require.NoError(t, err)
	b, _ := big[0].Bytes()
	require.Len(t, b, 4096, "oversize row corrupted: len=%d", len(b))
	require.Equal(t, byte(0), b[0])
	require.Equal(t, byte(255), b[255])
	// CreatedAt from the golden (deterministic override).
	require.True(t, snaps[0].CreatedAt.Equal(time.Unix(0, 1757400000000000000)), "createdAt = %v", snaps[0].CreatedAt)
}
