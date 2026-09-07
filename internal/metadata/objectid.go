package metadata

import (
	"fmt"
	"math"
)

// TableSpaceEnd is the exclusive end of the uint32 table ObjectID space.
// Table objects keep ObjectID == TableID below this bound; every other object
// (columns, constraints, extensions) lives at or above it, so the two spaces
// can never collide.
const TableSpaceEnd = uint64(1) << 32

// ObjectIDAllocator assigns stable Store-wide ObjectIDs for non-table objects.
// Objects start at TableSpaceEnd. The same natural key always maps to the
// same ObjectID, so identity is stable across snapshots and does not depend
// on hashing or case policy.
type ObjectIDAllocator struct {
	next  uint64
	byKey map[string]uint64
}

// NewObjectIDAllocator creates an allocator.
func NewObjectIDAllocator() *ObjectIDAllocator {
	return &ObjectIDAllocator{
		next:  TableSpaceEnd,
		byKey: make(map[string]uint64),
	}
}

// naturalKey joins the namespace and the caller-constructed identity string.
func naturalKey(namespace, key string) string {
	return namespace + "\x00" + key
}

// Alloc returns the stable ObjectID for (namespace, key), assigning a new one
// on first use.
func (a *ObjectIDAllocator) Alloc(namespace, key string) uint64 {
	k := naturalKey(namespace, key)
	if id, ok := a.byKey[k]; ok {
		return id
	}
	id := a.next
	a.next++
	a.byKey[k] = id
	return id
}

// Force registers an already-assigned ObjectID (e.g. from a previous snapshot)
// so future allocations never collide with it.
func (a *ObjectIDAllocator) Force(id uint64, key string) {
	if id >= a.next {
		a.next = id + 1
	}
	if _, ok := a.byKey[naturalKey("", key)]; !ok {
		a.byKey[naturalKey("", key)] = id
	}
}

// TableID returns the ObjectID as a uint32 TableID, failing when the object
// does not fit (only Table objects must be uint32-convertible).
func TableID(objectID uint64) (uint32, error) {
	if objectID == 0 {
		return 0, fmt.Errorf("rowpack: zero ObjectID has no TableID")
	}
	if objectID > math.MaxUint32 {
		return 0, fmt.Errorf("rowpack: ObjectID %d does not fit TableID", objectID)
	}
	return uint32(objectID), nil
}

// ObjectID returns a TableID widened back to ObjectID.
func ObjectID(tableID uint32) uint64 { return uint64(tableID) }
