package index

import "sort"

func sortU64s(s []uint64) { sort.Slice(s, func(i, j int) bool { return s[i] < s[j] }) }

func sortU32s(s []uint32) { sort.Slice(s, func(i, j int) bool { return s[i] < s[j] }) }

func sortSnapshotMetas(s []*SnapshotMeta) {
	sort.Slice(s, func(i, j int) bool { return s[i].ID < s[j].ID })
}

func sortRowKeyLocs(s []RowKeyLoc) {
	sort.Slice(s, func(i, j int) bool { return s[i].RowID < s[j].RowID })
}
