package rowpack

// rowIDSet is a packed open-addressing set of RowIDs used by the snapshot
// writer to reject duplicate (table, row) pairs. It replaces a Go map to cut
// the per-row footprint of large FULL loads by ~4x (~90 B/row -> ~11 B/row
// at the 0.8 load factor): RowID 0 is the empty-slot sentinel (the writer
// rejects zero RowIDs up front), slots are raw uint64s in one slice, and the
// table dimension is a small outer map held by the writer.
type rowIDSet struct {
	slots []uint64
	count int
}

const (
	rowIDSetInitialCap = 64 // first allocation; keeps small writes tiny
	rowIDSetLoadNum    = 8  // grow when count/slots exceeds 8/10
	rowIDSetLoadDen    = 10
)

// hashRowID is the splitmix64 finalizer: fast and well distributed for
// sequential and sparse RowIDs alike.
func hashRowID(v uint64) uint64 {
	v ^= v >> 33
	v *= 0xff51afd7ed558ccd
	v ^= v >> 33
	v *= 0xc4ceb9fe1a85ec53
	v ^= v >> 33
	return v
}

// Contains reports whether rowID is present. Nil-receiver safe (a table
// without a set yet has no rows).
func (s *rowIDSet) Contains(rowID uint64) bool {
	if s == nil || len(s.slots) == 0 {
		return false
	}
	mask := uint64(len(s.slots) - 1)
	i := hashRowID(rowID) & mask
	for {
		slot := s.slots[i]
		if slot == 0 {
			return false
		}
		if slot == rowID {
			return true
		}
		i = (i + 1) & mask
	}
}

// Insert adds rowID, which must be known to be absent (the writer calls
// Contains first and errors out on duplicates before ever inserting).
func (s *rowIDSet) Insert(rowID uint64) {
	if (s.count+1)*rowIDSetLoadDen > len(s.slots)*rowIDSetLoadNum {
		s.rehash()
	}
	mask := uint64(len(s.slots) - 1)
	i := hashRowID(rowID) & mask
	for s.slots[i] != 0 {
		i = (i + 1) & mask
	}
	s.slots[i] = rowID
	s.count++
}

// Len returns the number of stored RowIDs.
func (s *rowIDSet) Len() int { return s.count }

// rehash grows the slot slice to twice its size (or the initial capacity)
// and reinserts every live entry.
func (s *rowIDSet) rehash() {
	newCap := len(s.slots) * 2
	if newCap == 0 {
		newCap = rowIDSetInitialCap
	}
	old := s.slots
	s.slots = make([]uint64, newCap)
	s.count = 0
	mask := uint64(newCap - 1)
	for _, v := range old {
		if v == 0 {
			continue
		}
		i := hashRowID(v) & mask
		for s.slots[i] != 0 {
			i = (i + 1) & mask
		}
		s.slots[i] = v
		s.count++
	}
}
