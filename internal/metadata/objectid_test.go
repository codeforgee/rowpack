package metadata

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestObjectIDAllocator(t *testing.T) {
	a := NewObjectIDAllocator()
	require.Equal(t, uint64(TableSpaceEnd), a.Alloc("rowpack.meta.v1", "app.users"), "first alloc should be TableSpaceEnd")
	id1 := a.Alloc("rowpack.meta.v1", "app.users")
	id2 := a.Alloc("rowpack.meta.v1", "app.orders")
	require.Equal(t, uint64(TableSpaceEnd), id1, "allocator not monotonic: %d %d", id1, id2)
	require.Equal(t, uint64(TableSpaceEnd+1), id2, "allocator not monotonic: %d %d", id1, id2)
	// Same key, different namespace -> distinct ID.
	id3 := a.Alloc("com.mysql", "app.users")
	require.NotEqual(t, id1, id3, "same key across namespaces collided")
	// Stable remapping.
	require.Equal(t, id1, a.Alloc("rowpack.meta.v1", "app.users"), "allocator not stable for repeated key")
	// Table-space IDs are reserved and never collide.
	require.GreaterOrEqual(t, a.Alloc("rowpack.meta.v1", "col"), uint64(TableSpaceEnd), "allocator returned a table-space ID")
	// TableID conversion: a small table-space ObjectID converts; non-table
	// and overflow IDs fail.
	tid, err := TableID(7)
	require.NoError(t, err, "TableID(7) = %d, %v", tid, err)
	require.Equal(t, uint32(7), tid, "TableID(7) = %d, %v", tid, err)
	_, err = TableID(0)
	require.Error(t, err, "zero ObjectID accepted as TableID")
	_, err = TableID(id1)
	require.Error(t, err, "non-table ObjectID accepted as TableID")
	_, err = TableID(1 << 40)
	require.Error(t, err, "overflowing ObjectID accepted as TableID")
}
