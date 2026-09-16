package rowpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestGenerateFuzzCorpus regenerates the committed seed corpus for
// FuzzOpenMutated. Run explicitly (not by CI):
//
//	go test -run TestGenerateFuzzCorpus .
//
// It builds a small deterministic store with the normal write API and records
// its bytes as a Go fuzz corpus file under testdata/fuzz/FuzzOpenMutated/.
// The fuzz target itself must NOT build stores in its setup: calling
// Create (an fsync in the coordinator process) deterministically kills the
// worker processes on darwin ("fuzzing process terminated without fuzzing:
// EOF") — a Go toolchain quirk, worked around by committing the corpus.
func TestGenerateFuzzCorpus(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	base := filepath.Join(dir, "seed")
	db, err := Create(base, Options{BlockSize: 512, PageSize: 128})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin(ctx, NoParent)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.DefineTable("users", []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
		{Name: "score", Type: TypeFloat64},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 12; i++ {
		if err := tx.Insert("users", RowID(i), Row{
			Uint64(uint64(i)),
			String(fmt.Sprintf("user-%d", i)),
			Float64(float64(i) * 1.5),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	tx2, err := db.Begin(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Delete("users", 3); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Update("users", 5, Row{Uint64(5), String("updated"), Float64(9.75)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(base + ".rpk")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out := "go test fuzz v1\n[]byte(" + strconv.Quote(string(data)) + ")\n"
	corpusDir := filepath.Join("testdata", "fuzz", "FuzzOpenMutated")
	if err := os.MkdirAll(corpusDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpusDir, "seed.store"), []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote corpus: %d bytes", len(data))
}
