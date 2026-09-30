package rowpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
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

// craftedView assembles one snapshot of the given type and parent carrying only
// the entries add emits, and applies it to a copy of the store's current view.
// The writer always commits the index and its blocks together, so an index that
// disagrees with the blocks can only be built here — that is what the
// "missing block"/"unindexed block"/"forged metadata" arms are about.
//
// The result is NOT published: callers that ask the store directly (schema
// derivation, for instance) use it as-is, callers that need the store to read
// through it use publishCraftedSnapshot.
func craftedView(t *testing.T, db *Store, snapID, parent uint64, typ format.SnapshotType, add func(b *index.Builder)) *index.View {
	t.Helper()
	st, err := db.captureState()
	require.NoError(t, err)
	b := index.NewBuilder(1)
	require.NoError(t, b.SetSnapshot(format.SnapshotIndexEntry{
		SnapshotID:       snapID,
		ParentSnapshotID: parent,
		SnapshotType:     typ,
		CreatedUnixNano:  time.Now().UnixNano(),
	}))
	if add != nil {
		add(b)
	}
	_, txn, err := b.Build(index.BodyBounds{}, 0)
	require.NoError(t, err)
	view, err := st.view.Apply(txn, format.DefaultMaxSnapshotDepth)
	require.NoError(t, err)
	return view
}

// publishCraftedSnapshot builds a snapshot like craftedView and makes it the
// store's published state, so every read path sees it.
func publishCraftedSnapshot(t *testing.T, db *Store, snapID, parent uint64, typ format.SnapshotType, add func(b *index.Builder)) {
	t.Helper()
	view := craftedView(t, db, snapID, parent, typ, add)
	schemas, err := db.buildIndex(view)
	require.NoError(t, err)
	db.state.Store(&publishedState{view: view, schemas: schemas})
}

// craftView is craftedView for a DELTA on top of the current snapshot.
func craftView(t *testing.T, db *Store, snapID, parent uint64, add func(b *index.Builder)) *index.View {
	t.Helper()
	return craftedView(t, db, snapID, parent, format.SnapshotDelta, add)
}

// publishCraftedView is publishCraftedSnapshot for a DELTA on top of the
// current snapshot.
func publishCraftedView(t *testing.T, db *Store, snapID, parent uint64, add func(b *index.Builder)) {
	t.Helper()
	publishCraftedSnapshot(t, db, snapID, parent, format.SnapshotDelta, add)
}
