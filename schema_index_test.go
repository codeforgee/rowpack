package rowpack

import (
	"testing"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/stretchr/testify/require"
)

// TestSchemaIndexAccessorGuards pins the lookup contract of schemaIndex on
// unknown snapshots/tables/versions: empty results, not panics. Every accessor
// must tolerate a missing (snapshot, table) key.
func TestSchemaIndexAccessorGuards(t *testing.T) {
	si := newSchemaIndex()

	// Nothing in the index at all.
	require.Equal(t, "", si.nameOf(1, 1), "nameOf on empty index")
	require.Equal(t, NSUser, si.nsOf(1, 1), "nsOf on empty index")
	require.Nil(t, si.schema(1, 1, 1), "schema on empty index")
	require.Equal(t, uint32(0), si.latest(1, 1), "latest on empty index")
	require.EqualValues(t, 0, si.maxColumns(1, 1), "maxColumns on empty index")
	got, ok := si.tableID(1, "x")
	require.False(t, ok, "tableID on empty index must miss")
	require.Equal(t, TableID(0), got)

	// A table entry with zero usable versions (e.g. derivation failed midway)
	// must behave like a missing table for the version-sensitive accessors.
	si.bySnapshot[2] = map[uint32]*tableSchemas{7: {ns: NSUser}}
	require.Equal(t, "", si.nameOf(2, 7), "nameOf with no versions")
	require.Equal(t, uint32(0), si.latest(2, 7), "latest with no versions")

	// A table whose latest version has no decodable schema yields "".
	si.bySnapshot[3] = map[uint32]*tableSchemas{
		9: {versions: []uint32{1}, byVer: map[uint32]*codec.Schema{1: nil}, ns: "s"},
	}
	require.Equal(t, "", si.nameOf(3, 9), "nameOf with nil latest schema")

	// addressIndex on an empty table set returns nil (no map churn).
	require.Nil(t, addressIndex(nil), "addressIndex(nil)")
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
	got, ok := si.tableID(1, "dup")
	require.True(t, ok, "dup address must resolve")
	require.Equal(t, TableID(2), got, "lowest table id wins")
}
