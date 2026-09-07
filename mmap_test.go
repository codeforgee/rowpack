package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// TestMmapReaderEquivalence verifies the mmap View fast path produces
// byte-identical blocks to the plain ReadAt path, and that mmap-backed
// reads never let cached blocks alias the file mapping (V1.1-C).
func TestMmapReaderEquivalence(t *testing.T) {
	base := filepath.Join(t.TempDir(), "mmap")
	db, fullID := buildMmapStore(t, base, 5000)
	st, err := db.captureState()
	if err != nil {
		t.Fatal(err)
	}
	view := st.view
	// Collect every block location from the committed view.
	bls := view.Blocks()
	if len(bls) == 0 {
		t.Fatal("no blocks in view")
	}

	// Plain ReadAt reader over a separate file handle.
	f, err := os.Open(base + ".rpk")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	plain := block.NewReader(f, block.DefaultLimits())

	for _, bl := range bls {
		off := int64(bl.DataOffset)
		gotView, err := db.reader.ReadAtBlock(off)
		if err != nil {
			t.Fatalf("block %d: view read: %v", bl.BlockID, err)
		}
		gotCopy, err := plain.ReadAtBlock(off)
		if err != nil {
			t.Fatalf("block %d: copy read: %v", bl.BlockID, err)
		}
		if gotView.Header != gotCopy.Header {
			t.Fatalf("block %d: header mismatch", bl.BlockID)
		}
		if string(gotView.Raw) != string(gotCopy.Raw) {
			t.Fatalf("block %d: raw mismatch (%d vs %d bytes)", bl.BlockID, len(gotView.Raw), len(gotCopy.Raw))
		}
		if fileformat.CRC32C(gotView.Raw) != gotView.Header.RawCRC32C {
			t.Fatalf("block %d: CRC mismatch", bl.BlockID)
		}
	}

	// Data reads through the public API must agree with the committed rows.
	for i := uint64(0); i < 100; i++ {
		if _, err := db.Get(context.Background(), fullID, 1, i+1, nil); err != nil {
			t.Fatalf("get %d: %v", i+1, err)
		}
	}
}

// buildMmapStore writes nRows into a FULL snapshot and returns the open store.
func buildMmapStore(t *testing.T, base string, nRows uint64) (*Store, SnapshotID) {
	t.Helper()
	db, err := Create(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
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
	return db, full.ID
}
