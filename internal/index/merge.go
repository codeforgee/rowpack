package index

import (
	"container/heap"
)

// MergeIter merges the sorted per-layer row iterators of a snapshot's parent
// chain into one deduplicated stream in ascending RowID order. It is the
// single implementation of the chain merge, shared by the root package's scan
// iterator and the view's logical row counting: layers positioned earlier in
// the slice win RowID ties (shallowest chain depth first), and each RowID is
// yielded exactly once, paired with the winning layer's Loc.
//
// A single live layer skips the heap entirely and walks its sorted iterator
// linearly (the common FULL-snapshot case). The zero value is not usable;
// build one with NewMergeIter. It is not safe for concurrent use.
type MergeIter struct {
	h      mergeHeap
	single *RowKeyIter // fast path: one live layer needs no heap
}

// mergeLayer is one chain layer's iterator plus its depth (its index in the
// layer slice passed to NewMergeIter): the depth breaks RowID ties.
type mergeLayer struct {
	keys  *RowKeyIter
	depth int
}

// mergeHeap is a min-heap over the current head of each layer, keyed by
// RowID with depth as tiebreaker.
type mergeHeap []*mergeLayer

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	if h[i].keys.RowID() != h[j].keys.RowID() {
		return h[i].keys.RowID() < h[j].keys.RowID()
	}
	return h[i].depth < h[j].depth
}
func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *mergeHeap) Push(x any)   { *h = append(*h, x.(*mergeLayer)) }
func (h *mergeHeap) Pop() any     { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// NewMergeIter builds a merge over the given layers, target snapshot first.
// Exhausted (or nil) layers are skipped; a returned iterator always has at
// least one live layer or reports ok=false on the first Next.
func NewMergeIter(layers ...*RowKeyIter) *MergeIter {
	m := &MergeIter{}
	var live []*mergeLayer
	for depth, keys := range layers {
		if keys != nil && !keys.Done() {
			live = append(live, &mergeLayer{keys: keys, depth: depth})
		}
	}
	switch len(live) {
	case 0:
		return m
	case 1:
		m.single = live[0].keys
		return m
	}
	m.h = make(mergeHeap, len(live))
	copy(m.h, live)
	heap.Init(&m.h)
	return m
}

// Next advances to the next distinct RowID and reports its winning location.
// Every layer positioned at that RowID is consumed, so overridden duplicates
// never surface. ok is false at the end of the merge.
func (m *MergeIter) Next() (rowID uint64, loc RowLoc, ok bool) {
	if m.single != nil {
		if m.single.Done() {
			return 0, RowLoc{}, false
		}
		rowID, loc = m.single.RowID(), m.single.Loc()
		m.single.Next()
		return rowID, loc, true
	}
	if len(m.h) == 0 {
		return 0, RowLoc{}, false
	}
	winner := heap.Pop(&m.h).(*mergeLayer)
	rowID, loc = winner.keys.RowID(), winner.keys.Loc()
	// All layers currently at rowID lose; consume them, then re-seat the
	// winner. The heap tiebreak ensures the winner is the shallowest layer.
	for len(m.h) > 0 && m.h[0].keys.RowID() == rowID {
		m.advance(heap.Pop(&m.h).(*mergeLayer))
	}
	m.advance(winner)
	return rowID, loc, true
}

// advance moves one layer past its current head and re-queues it when rows
// remain.
func (m *MergeIter) advance(l *mergeLayer) {
	l.keys.Next()
	if !l.keys.Done() {
		heap.Push(&m.h, l)
	}
}
