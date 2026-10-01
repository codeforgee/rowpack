package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/codeforgee/rowpack"
	"github.com/codeforgee/rowpack/internal/inspect"
	"github.com/stretchr/testify/require"
)

// newStore creates a small valid store: one FULL snapshot, one table, 3 rows.
func newStore(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "smoke")
	db, err := rowpack.Create(base, rowpack.Options{})
	require.NoError(t, err)
	ctx := context.Background()
	w, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("users", []rowpack.Column{
		{Name: "id", Type: rowpack.TypeUint64},
		{Name: "name", Type: rowpack.TypeString},
	}))
	for i := 1; i <= 3; i++ {
		require.NoError(t, w.Insert(ctx, "users", rowpack.RowID(i),
			rowpack.Row{rowpack.Uint64(uint64(i)), rowpack.String("user-" + string(rune('0'+i)))}))
	}
	_, err = w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return base
}

// TestRunExitCodeMapping pins run()'s mapping of inspect.Run results onto
// exit codes without touching os.Exit.
func TestRunExitCodeMapping(t *testing.T) {
	base := newStore(t)

	var out, errb bytes.Buffer
	code := run([]string{"list", base}, &out, &errb)
	require.Equal(t, inspect.ExitOK, code)
	require.Contains(t, out.String(), "snapshot 1 FULL")
	require.Empty(t, errb.String())

	// Usage error -> ExitUsage (bad flag, missing args).
	out.Reset()
	code = run([]string{"--nope"}, &out, &bytes.Buffer{})
	require.Equal(t, inspect.ExitUsage, code)
	code = run([]string{"list"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Equal(t, inspect.ExitUsage, code)

	// Command failure -> ExitFailure (missing store; inspect prints its own
	// diagnostics, so run's fallback line stays reserved for bare errors).
	code = run([]string{"list", filepath.Join(t.TempDir(), "gone")}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Equal(t, inspect.ExitFailure, code)
}

// TestBinarySmoke builds the real CLI binary and runs it against a store:
// the last mile from main() through os.Exit is only exercised by executing
// the built artifact.
func TestBinarySmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the CLI binary")
	}
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	pkgDir := filepath.Dir(thisFile)
	binName := "rowpack-inspect"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	bin := filepath.Join(t.TempDir(), binName)
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = pkgDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	base := newStore(t)
	runBin := func(args ...string) (string, int) {
		cmd := exec.Command(bin, args...)
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return string(out), code
	}

	out, code := runBin("list", base)
	require.Equal(t, 0, code)
	require.Contains(t, out, "snapshot 1 FULL")

	out, code = runBin("verify", base)
	require.Equal(t, 0, code)
	require.Contains(t, out, "OK snapshots=1")

	out, code = runBin("dump", base, "1", "1")
	require.Equal(t, 0, code)
	require.Contains(t, out, "user-2")

	_, code = runBin("header", base)
	require.Equal(t, 0, code)

	// Usage errors exit 2, failures exit 1.
	_, code = runBin("--bad-flag")
	require.Equal(t, 2, code)
	_, code = runBin("list")
	require.Equal(t, 2, code)
	_, code = runBin("list", filepath.Join(t.TempDir(), "gone"))
	require.Equal(t, 1, code)

	// The binary refuses a corrupted store file with a failure, not a panic.
	broken := filepath.Join(t.TempDir(), "broken")
	require.NoError(t, os.WriteFile(broken+".rpk", []byte("not a rowpack store at all......"), 0o644))
	_, code = runBin("list", broken)
	require.Equal(t, 1, code)
}
