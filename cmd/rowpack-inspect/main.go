// Command rowpack-inspect is a read-only debugging tool for RowPack stores.
//
// Usage:
//
//	rowpack-inspect header <base>
//	rowpack-inspect list <base>
//	rowpack-inspect verify <base>
//	rowpack-inspect dump <base> <snapshotID> <tableID>
//
// The command implementation lives in internal/inspect so every path is
// testable; this file only maps the result onto a process exit status.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/codeforgee/rowpack/internal/inspect"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run maps inspect.Run's result onto a process exit code. It never calls
// os.Exit itself, so the mapping is unit-testable; main is the only seam that
// terminates the process.
func run(argv []string, stdout, stderr io.Writer) int {
	err := inspect.Run(context.Background(), argv, stdout, stderr)
	if err == nil {
		return inspect.ExitOK
	}
	var ee *inspect.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	// Defensive: every inspect.Run error is an *ExitError today; this keeps a
	// future bare error from exiting 0 silently.
	fmt.Fprintln(stderr, "rowpack-inspect:", err)
	return inspect.ExitFailure
}
