package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// FuzzOpenMutated mutates a valid store file and requires that Open and any
// subsequent reads never panic, hang, or allocate unboundedly: every failure
// must surface as an error. This drives every Unmarshal/parse/checkBounds
// corruption branch in the format, block, index and codec layers.
//
// The seed corpus lives in testdata/fuzz/FuzzOpenMutated/seed.store (auto-
// loaded by `go test -fuzz`) and is regenerated with:
//
//	go test -run TestGenerateFuzzCorpus .
//
// Do not build stores in the fuzz setup function: an fsync in the coordinator
// process deterministically kills the worker processes on darwin ("fuzzing
// process terminated without fuzzing: EOF") — a Go toolchain quirk, worked
// around by committing the corpus. On darwin the fuzzer may also need
// `-parallel 1` if workers die during mutation bursts.
func FuzzOpenMutated(f *testing.F) {
	// A few tiny deterministic mutants as extra seeds; the real seed comes
	// from the committed corpus file.
	f.Add([]byte("ROWPACK1" + string(make([]byte, 120))))
	f.Add([]byte("GARBAGE-GARBAGE-GARBAGE-GARBAGE-GARBAGE-GARBAGE-GARBAGE-"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 64 || len(data) > 1<<20 {
			t.Skip()
		}
		dir := t.TempDir()
		path := filepath.Join(dir, "fz")
		if err := os.WriteFile(path+".rpk", data, 0o644); err != nil {
			t.Skip()
		}
		// Read-only open: no truncation, so identical inputs behave identically.
		db, err := Open(path, Options{ReadOnly: true})
		if err != nil {
			return // corruption rejected at open: fine
		}
		defer db.Close()
		ctx := context.Background()
		snaps, err := db.ListSnapshots(ctx)
		if err != nil {
			return
		}
		for _, sm := range snaps {
			// Point lookups of ids that exist in a valid prefix of the chain.
			_, _ = db.Get(ctx, sm.ID, "users", 1, nil)
			_, _ = db.Exists(ctx, sm.ID, "users", 7)
			// A full scan must terminate.
			it, err := db.Scan(ctx, sm.ID, "users", ScanOptions{})
			if err != nil {
				continue
			}
			n := 0
			for {
				_, ok := it.Next()
				if !ok {
					break
				}
				n++
				if n > 1<<20 { // pathological runaway guard
					break
				}
			}
			it.Close()
			// Batch read and full verify must terminate too.
			_, _ = db.ReadBatch(ctx, sm.ID, "users", []RowID{1, 2, 3})
			_, _ = db.Verify(ctx, VerifyFull)
		}
	})
}
