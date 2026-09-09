package index

import (
	"fmt"
	"sort"
	"sync"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// LazySource loads and decodes one Row Index Page for a snapshot on demand.
// It is implemented by the store, which owns the file reader, the chunk
// crypto context, and the decoded-IndexPageCache LRU. pageIdx is the page's
// ordinal within the snapshot's fence (the chunk sequence of an encrypted page
// is the txn's chunk count seq + pageIdx). The returned slice is owned by the
// source/cache and must not be mutated or retained. A non-nil error means the
// page is unreadable or corrupt; the View records it and treats the lookup as
// absent, surfacing it to the store via LazyError.
type LazySource interface {
	// LoadIndexPage reads, OPENs, decompresses and decodes the page described by
	// fence for snapshotID. pageIdx is the page's ordinal within the snapshot's
	// fence (chunk sequence = Seq + pageIdx). When cache is true the store may
	// serve from / promote into its IndexPageCache; a streaming scan passes
	// cache=false so it does not pollute the random-read cache. The returned
	// slice is owned by the source/cache and must not be mutated or retained.
	LoadIndexPage(snapshotID uint64, pageIdx int, fence fileformat.RowIndexFenceEntry, cache bool) ([]fileformat.RowIndexEntry, error)
}

// lazySnapshot is the per-snapshot lazy row index: the sorted fence directory
// plus the number of pages, so a (snapshot, table, rowID) query binary-searches
// the fence to the single table page that must contain the row, then loads that
// page on demand. Every page is single-table (the writer splits pages on table
// boundaries), so fence.TableID + fence.MinRowID is a monotonic page-start key.
type lazySnapshot struct {
	fences []fileformat.RowIndexFenceEntry // sorted by (TableID, MinRowID) then StoredOffset
}

// lazyIndex is the View's Lazy row index: per-snapshot fence directories plus a
// page source. It is nil in Eager mode, and the per-snapshot eager rowShards are
// nil in Lazy mode.
type lazyIndex struct {
	source     LazySource
	snapshots  map[uint64]*lazySnapshot
	fenceBytes uint64 // resident bytes of all fence directories
	errMu      sync.Mutex
	err        error // first page-load error (surfaced to the store); guarded by errMu
}

// setErr records the first page-load error (ignored after the first one).
func (lz *lazyIndex) setErr(err error) {
	lz.errMu.Lock()
	if lz.err == nil {
		lz.err = err
	}
	lz.errMu.Unlock()
}

// fenceStartLess reports whether fence (TableID, MinRowID) is positionally <=
// (table, rowID). Because pages are single-table and sorted by (TableID, RowID),
// this is a monotonic key over the fence directory.
func fenceStartLess(f *fileformat.RowIndexFenceEntry, table uint32, rowID uint64) bool {
	if f.TableID != table {
		return f.TableID < table
	}
	return f.MinRowID <= rowID
}

// row resolves (snapshot, table, rowID) by binary-searching the fence to the
// single table page that must contain the row, loading that page on demand.
func (lz *lazyIndex) row(snapshot uint64, table uint32, rowID uint64) (RowLoc, bool) {
	ls := lz.snapshots[snapshot]
	if ls == nil || len(ls.fences) == 0 {
		return RowLoc{}, false
	}
	fences := ls.fences
	// Largest index i with fenceStartLess(fences[i], table, rowID). For a
	// single-table page the start key equals the page's first (TableID, RowID),
	// so this is the page that must contain the row (if it exists).
	lo, hi := 0, len(fences)
	for lo < hi {
		mid := (lo + hi) / 2
		if fenceStartLess(&fences[mid], table, rowID) {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		return RowLoc{}, false
	}
	idx := lo - 1
	f := &fences[idx]
	if f.TableID != table || rowID < f.MinRowID || rowID > f.MaxRowID {
		return RowLoc{}, false
	}
	entries, err := lz.source.LoadIndexPage(snapshot, idx, *f, true)
	if err != nil {
		lz.setErr(fmt.Errorf("rowpack: lazy load index page %d of snapshot %d: %w", idx, snapshot, err))
		return RowLoc{}, false
	}
	i := sort.Search(len(entries), func(i int) bool { return entries[i].RowID >= rowID })
	if i >= len(entries) || entries[i].RowID != rowID {
		return RowLoc{}, false
	}
	return RowLoc{BlockID: entries[i].BlockID, ItemOrdinal: entries[i].ItemOrdinal, ChangeType: entries[i].ChangeType}, true
}

// tables returns the distinct TableIDs that have row entries at the snapshot,
// derived from the sorted fence (no page is decoded).
func (lz *lazyIndex) tables(snapshot uint64) []uint32 {
	ls := lz.snapshots[snapshot]
	if ls == nil || len(ls.fences) == 0 {
		return nil
	}
	out := make([]uint32, 0, 4)
	var last uint32
	have := false
	for i := range ls.fences {
		t := ls.fences[i].TableID
		if !have || t != last {
			out = append(out, t)
			last = t
			have = true
		}
	}
	return out
}

// pageRangeFor returns the fence index range [start, end) covering `table`'s
// pages. Returns start == end when the table has no pages.
func (lz *lazyIndex) pageRangeFor(snapshot uint64, table uint32) (start, end int) {
	ls := lz.snapshots[snapshot]
	if ls == nil {
		return 0, 0
	}
	// First index with TableID >= table.
	start = sort.Search(len(ls.fences), func(i int) bool { return ls.fences[i].TableID >= table })
	end = start
	for end < len(ls.fences) && ls.fences[end].TableID == table {
		end++
	}
	return start, end
}

// rowIterFor returns a lazy RowKeyIter over (snapshot, table), or nil when the
// snapshot has no rows for the table.
func (lz *lazyIndex) rowIterFor(snapshot uint64, table uint32) *RowKeyIter {
	ls := lz.snapshots[snapshot]
	if ls == nil {
		return nil
	}
	start, end := lz.pageRangeFor(snapshot, table)
	if start == end {
		return nil
	}
	total := 0
	for i := start; i < end; i++ {
		total += int(ls.fences[i].EntryCount)
	}
	if total == 0 {
		return nil
	}
	it := &lazyRowIter{
		lz:       lz,
		snapshot: snapshot,
		table:    table,
		page:     start,
		pageEnd:  end,
		fences:   ls.fences,
		total:    total,
	}
	it.seat()
	return &RowKeyIter{it: it}
}

// lazyRowIter streams (snapshot, table) rows by loading one page at a time in
// fence order and walking its sorted entries. It never holds more than one
// decoded page, so a scan does not materialize the full table index and does
// not pollute the random-read page cache. It implements the rowIter contract
// (position-based forward iteration) so it feeds the same k-way merge used by
// Eager scans.
type lazyRowIter struct {
	lz       *lazyIndex
	snapshot uint64
	table    uint32
	page     int // current fence index
	pageEnd  int
	fences   []fileformat.RowIndexFenceEntry

	entries []fileformat.RowIndexEntry
	pos     int
	loaded  bool
	total   int
	done    bool

	curRowID uint64
	curLoc   RowLoc
}

// Len returns the total row count for the table (from the fence, no decode).
func (it *lazyRowIter) Len() int { return it.total }

// Done reports whether the iterator is exhausted.
func (it *lazyRowIter) Done() bool { return it.done }

// RowID returns the current row's RowID.
func (it *lazyRowIter) RowID() uint64 { return it.curRowID }

// Loc returns the current row's RowLoc.
func (it *lazyRowIter) Loc() RowLoc { return it.curLoc }

// Next advances to the next row.
func (it *lazyRowIter) Next() {
	it.pos++
	if it.pos >= len(it.entries) {
		it.loaded = false
	}
	it.seat()
}

// seat positions the iterator at the current row, loading the next page as
// needed. It sets done when exhausted or when a page fails to load.
func (it *lazyRowIter) seat() {
	for {
		if !it.loaded {
			if it.page >= it.pageEnd {
				it.done = true
				return
			}
			f := &it.fences[it.page]
			entries, err := it.lz.source.LoadIndexPage(it.snapshot, it.page, *f, false)
			if err != nil {
				it.done = true
				it.lz.setErr(fmt.Errorf("rowpack: lazy load index page %d of snapshot %d: %w", it.page, it.snapshot, err))
				return
			}
			it.entries = entries
			it.pos = 0
			it.loaded = true
			it.page++
		}
		if it.pos < len(it.entries) {
			e := it.entries[it.pos]
			it.curRowID = e.RowID
			it.curLoc = RowLoc{BlockID: e.BlockID, ItemOrdinal: e.ItemOrdinal, ChangeType: e.ChangeType}
			return
		}
		it.loaded = false
	}
}
