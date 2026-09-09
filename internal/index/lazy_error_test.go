package index

import (
	"errors"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

type failingLazySource struct{ err error }

func (s failingLazySource) LoadIndexPage(uint64, int, fileformat.RowIndexFenceEntry, bool) ([]fileformat.RowIndexEntry, error) {
	return nil, s.err
}

func TestResolveRowPropagatesLazyPageError(t *testing.T) {
	want := errors.New("bad lazy index page")
	v := EmptyView()
	v.snapshots[1] = &SnapshotMeta{ID: 1, Type: fileformat.SnapshotFull}
	v.lazy = &lazyIndex{
		source: failingLazySource{err: want},
		snapshots: map[uint64]*lazySnapshot{1: {fences: []fileformat.RowIndexFenceEntry{{
			SnapshotID: 1, TableID: 2, MinRowID: 1, MaxRowID: 10, EntryCount: 1,
		}}}},
	}
	_, ok, err := v.ResolveRow(1, 2, 5)
	if ok || !errors.Is(err, want) {
		t.Fatalf("ResolveRow = ok %v, err %v; want propagated %v", ok, err, want)
	}
}
