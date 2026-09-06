package metadata

import "testing"

func TestObjectIDAllocator(t *testing.T) {
	a := NewObjectIDAllocator()
	if a.Alloc("rowpack.meta.v1", "app.users") != 2 {
		t.Fatal("first alloc should be 2")
	}
	id1 := a.Alloc("rowpack.meta.v1", "app.users")
	id2 := a.Alloc("rowpack.meta.v1", "app.orders")
	if id1 != 2 || id2 != 3 {
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
	// TableID conversion.
	tid, err := TableID(id1)
	if err != nil || tid != 2 {
		t.Fatalf("TableID(%d) = %d, %v", id1, tid, err)
	}
	if _, err := TableID(0); err == nil {
		t.Fatal("zero ObjectID accepted as TableID")
	}
	if _, err := TableID(1 << 40); err == nil {
		t.Fatal("overflowing ObjectID accepted as TableID")
	}
}
