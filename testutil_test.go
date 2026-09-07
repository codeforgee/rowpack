package rowpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// tmpdb returns a fresh per-call temporary directory under testdata/tmpdb so
// that every artifact a test produces lands in one gitignored place
// (see .gitignore: **/testdata/tmpdb/) instead of the OS temp dir. The caller
// owns the directory; it is left in place for inspection after the run.
func tmpdb(t testing.TB) string {
	t.Helper()
	root := filepath.Join("testdata", "tmpdb")
	if err := os.MkdirAll(root, 0o755); err != nil {
		require.Fail(t, "mkdir testdata/tmpdb: %v", err)
	}
	dir, err := os.MkdirTemp(root, tmpdbName(t)+"-")
	if err != nil {
		require.Fail(t, "mkdirtemp testdata/tmpdb: %v", err)
	}
	return dir
}

// tmpdbName derives a filesystem-safe prefix from the test name.
func tmpdbName(t testing.TB) string {
	name := strings.ReplaceAll(t.Name(), "/", "_")
	if len(name) > 60 {
		name = name[:60]
	}
	return name
}
