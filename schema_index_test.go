package rowpack

import (
	"testing"

	"github.com/rowpack/rowpack/internal/codec"
)

// TestSchemaIndexAccessorGuards pins the lookup contract of schemaIndex on
// unknown snapshots/tables/versions: empty results, not panics. Every accessor
// must tolerate a missing (snapshot, table) key.
func TestSchemaIndexAccessorGuards(t *testing.T) {
	si := newSchemaIndex()

	// Nothing in the index at all.
	if got := si.nameOf(1, 1); got != "" {
		t.Fatalf("nameOf(empty) = %q", got)
	}
	if got := si.nsOf(1, 1); got != NSUser {
		t.Fatalf("nsOf(empty) = %q, want %q", got, NSUser)
	}
	if si.schema(1, 1, 1) != nil {
		t.Fatal("schema(empty) != nil")
	}
	if got := si.latest(1, 1); got != 0 {
		t.Fatalf("latest(empty) = %d", got)
	}
	if got := si.maxColumns(1, 1); got != 0 {
		t.Fatalf("maxColumns(empty) = %d", got)
	}
	if got, ok := si.tableID(1, "x"); got != 0 || ok {
		t.Fatalf("tableID(empty) = %d, %v", got, ok)
	}

	// A table entry with zero usable versions (e.g. derivation failed midway)
	// must behave like a missing table for the version-sensitive accessors.
	si.bySnapshot[2] = map[uint32]*tableSchemas{7: {ns: NSUser}}
	if got := si.nameOf(2, 7); got != "" {
		t.Fatalf("nameOf(no versions) = %q", got)
	}
	if got := si.latest(2, 7); got != 0 {
		t.Fatalf("latest(no versions) = %d", got)
	}

	// A table whose latest version has no decodable schema is skipped by the
	// address index; nil schema at the latest version yields "" from nameOf.
	sch := &codec.Schema{Name: "t"}
	si.bySnapshot[3] = map[uint32]*tableSchemas{
		9: {versions: []uint32{1}, byVer: map[uint32]*codec.Schema{1: nil}, ns: "s"},
	}
	_ = sch
	if got := si.nameOf(3, 9); got != "" {
		t.Fatalf("nameOf(nil latest) = %q", got)
	}

	// addressIndex on an empty table set returns nil (no map churn).
	if addressIndex(nil) != nil {
		t.Fatal("addressIndex(nil) != nil")
	}
}

// TestSchemaIndexAddressPrecedence pins the deterministic lowest-table-id rule
// when two tables (impossible via DefineTable, defensive) claim one address.
func TestSchemaIndexAddressPrecedence(t *testing.T) {
	si := newSchemaIndex()
	mk := func(name string) *tableSchemas {
		return &tableSchemas{
			versions: []uint32{1},
			byVer:    map[uint32]*codec.Schema{1: {Name: name}},
			decoders: map[uint32]codec.Decoder{},
			ns:       NSUser,
		}
	}
	si.bySnapshot[1] = map[uint32]*tableSchemas{
		5: mk("dup"),
		2: mk("dup"),
	}
	si.byAddress[1] = addressIndex(si.bySnapshot[1])
	if got, ok := si.tableID(1, "dup"); got != 2 || !ok {
		t.Fatalf("tableID(dup) = %d, %v; want lowest id 2", got, ok)
	}
}
