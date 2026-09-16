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
// testable; this file only maps its exit status onto the process.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/rowpack/rowpack/internal/inspect"
)

func main() {
	err := inspect.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	if err == nil {
		return
	}
	var ee *inspect.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.Code)
	}
	fmt.Fprintln(os.Stderr, "rowpack-inspect:", err)
	os.Exit(inspect.ExitFailure)
}
