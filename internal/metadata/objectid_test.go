package metadata

import "testing"

func TestObjectIDAllocator(t *testing.T) {
	a := NewObjectIDAllocator()
	if a.Alloc("rowpack.meta.v1", "app.users") != TableSpaceEnd {
		t.Fatal("first alloc should be TableSpaceEnd")
	}
	id1 := a.Alloc("rowpack.meta.v1", "app.users")
	id2 := a.Alloc("rowpack.meta.v1", "app.orders")
	if id1 != TableSpaceEnd || id2 != TableSpaceEnd+1 {
		t.Fatalf("allocator not monotonic: %d %d", id1, id2)
	}
	// Same key, different namespace -> distinct ID.
	id3 := a.Alloc("com.mysql", "app.users")
	if id3 == id1 {
		t.Fatal("same key across namespaces collided")
	}
	// Stable remapping.
	if a.Alloc("rowpack.meta.v1", "app.users") != id1 {
		t.Fatal("allocator not stable for repeated key")
	}
	if a.ExternalKey(id1) != "app.users" {
		t.Fatalf("external key mapping wrong: %q", a.ExternalKey(id1))
	}
	// Table-space IDs are reserved and never collide.
	if a.Alloc("rowpack.meta.v1", "col") < TableSpaceEnd {
		t.Fatal("allocator returned a table-space ID")
	}
	// TableID conversion: a small table-space ObjectID converts; non-table
	// and overflow IDs fail.
	tid, err := TableID(7)
	if err != nil || tid != 7 {
		t.Fatalf("TableID(7) = %d, %v", tid, err)
	}
	if _, err := TableID(0); err == nil {
		t.Fatal("zero ObjectID accepted as TableID")
	}
	if _, err := TableID(id1); err == nil {
		t.Fatal("non-table ObjectID accepted as TableID")
	}
	if _, err := TableID(1 << 40); err == nil {
		t.Fatal("overflowing ObjectID accepted as TableID")
	}
}
