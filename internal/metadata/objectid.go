package metadata

import (
	"fmt"
	"math"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// ObjectIDAllocator assigns stable Store-wide ObjectIDs. Header keeps
// ObjectID 1; other objects start at 2. The same natural key always maps to
// the same ObjectID, so identity is stable across snapshots and does not
// depend on hashing or case policy.
type ObjectIDAllocator struct {
	next  uint64
	byKey map[string]uint64
	byID  map[uint64]string
}

// NewObjectIDAllocator creates an allocator seeded with the reserved Header
// object.
func NewObjectIDAllocator() *ObjectIDAllocator {
	return &ObjectIDAllocator{
		next:  fileformat.HeaderObjectID + 1,
		byKey: make(map[string]uint64),
		byID:  map[uint64]string{fileformat.HeaderObjectID: "header"},
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
	a.byID[id] = key
	return id
}

// ExternalKey returns the natural key that was assigned to an ObjectID, or ""
// if unknown.
func (a *ObjectIDAllocator) ExternalKey(id uint64) string { return a.byID[id] }

// Next returns the next ObjectID that will be assigned.
func (a *ObjectIDAllocator) Next() uint64 { return a.next }

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
