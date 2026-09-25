package rowpack

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// schema_fields_arms_test.go 补 schema.go 剩下的字段解析与派生兜底臂。它们的共同点
// 是需要一个「说不通」的 metadata 记录:字段缺失、字段类型不对、或者记录本身合法但
// 派生成 schema 之后不成立。写入路径只会产生自洽的记录,所以这些臂只能白盒构造——
// 直接把记录喂给 addSchema / deriveColumn,或者用 craftView 造一个指向真实物理记录
// 的索引条目。
//
// 覆盖:addressIndex 跳过没有可用 schema 的表、decoderFor 对未知表报
// ErrSchemaMismatch、addSchema 的 revision==0 与同版本重复派生、schema 校验失败
// (负的 decimal scale)、解码器编译失败(列数比 codec 策略宽)、fieldSint 的缺字段
// 与类型不符兜底、typeName 的未知类型、以及 readMetadataCached 沿父链上升和整条链
// 都找不到该对象。
//
// 剩下两条不可达臂已在 schema_arms_test.go 注出:父链中途断掉,以及版本排序(一张
// 表只有一个版本)。

// newSchemaEntry returns an empty per-table schema container like the one
// deriveTables creates before it derives anything.
func newSchemaEntry() *tableSchemas {
	return &tableSchemas{byVer: make(map[uint32]*codec.Schema), decoders: make(map[uint32]codec.Decoder)}
}

// metaColumnRecord builds a Column record as DefineSchema would write it.
func metaColumnRecord(oid uint64, columnID int64, name, typ string, scale int64) *metadata.Record {
	return &metadata.Record{
		ObjectID: oid,
		ParentID: 1,
		Revision: 1,
		Fields: []metadata.Field{
			{ID: metadata.ColColumnID, Value: columnID},
			{ID: metadata.ColColumnName, Value: name},
			{ID: metadata.ColColumnType, Value: typ},
			{ID: metadata.ColNullable, Value: "NO"},
			{ID: metadata.ColDataScale, Value: scale},
		},
	}
}

// TestAddressIndexSkipsTablesWithoutSchema: a table is addressable only once a
// schema version was derived for it. Tables whose derivation produced nothing
// (no container, or a container without versions) stay out of the reverse
// index instead of mapping their address to an empty name.
func TestAddressIndexSkipsTablesWithoutSchema(t *testing.T) {
	withVersion := newSchemaEntry()
	withVersion.ns = NSUser
	withVersion.byVer[1] = &codec.Schema{TableID: 3, Version: 1, Name: "t"}
	withVersion.versions = []uint32{1}

	addrs := addressIndex(map[uint32]*tableSchemas{
		1: nil,              // the table is known, its schemas are not
		2: newSchemaEntry(), // a container no version was ever added to
		3: withVersion,
	})
	require.Len(t, addrs, 1, "only the table with a derived version is addressable")
	require.Equal(t, TableID(3), addrs[Qualify("", "t")])
}

// TestDecoderForRejectsUnknownTable: a block claims a table the schema index
// does not know. That is an index/schema contradiction, reported as a schema
// mismatch rather than a silent empty decoder.
func TestDecoderForRejectsUnknownTable(t *testing.T) {
	db := armCommittedStore(t) // snapshot 1 knows table 1 only
	st, err := db.captureState()
	require.NoError(t, err)

	_, err = st.schemas.decoderFor(&index.BlockLoc{SnapshotID: 1, TableID: 42}, 1)
	require.ErrorIs(t, err, ErrSchemaMismatch)
	require.ErrorContains(t, err, "schema for table 42")

	_, err = st.schemas.decoderFor(&index.BlockLoc{SnapshotID: 1, TableID: 1}, 99)
	require.ErrorIs(t, err, ErrSchemaMismatch)
	require.ErrorContains(t, err, "decoder for table 1 version 99")
}

// TestAddSchemaSkipsZeroRevision: a Table record without a revision carries no
// schema version, so it contributes nothing and is not an error either — the
// record is simply not a schema definition.
func TestAddSchemaSkipsZeroRevision(t *testing.T) {
	db := armStore(t)
	ts := newSchemaEntry()
	rec := &metadata.Record{
		ObjectID: 1,
		Fields:   []metadata.Field{{ID: metadata.TableName, Value: "t"}},
	}
	require.NoError(t, db.addSchema(ts, rec, []*metadata.Record{metaColumnRecord(1<<32, 1, "id", "int64", 0)}))
	require.Empty(t, ts.versions, "revision 0 defines no version")
	require.Empty(t, ts.byVer)
	require.Empty(t, ts.decoders)
}

// TestAddSchemaDerivesEachVersionOnce: the same revision seen twice — a table
// record reachable through more than one layer — is derived once. Redefining a
// table identically reuses its revision, so this is the normal case, not a
// repair path.
func TestAddSchemaDerivesEachVersionOnce(t *testing.T) {
	db := armStore(t)
	ts := newSchemaEntry()
	rec := &metadata.Record{
		ObjectID: 1, Revision: 1,
		Fields: []metadata.Field{{ID: metadata.TableName, Value: "t"}},
	}
	cols := []*metadata.Record{metaColumnRecord(1<<32, 1, "id", "int64", 0)}

	require.NoError(t, db.addSchema(ts, rec, cols))
	require.NoError(t, db.addSchema(ts, rec, cols))
	require.Equal(t, []uint32{1}, ts.versions, "one version, derived once")
	require.Len(t, ts.decoders, 1)
}

// TestAddSchemaRejectsNegativeDecimalScale: a decimal column whose scale is
// negative cannot be decoded, so the record is a schema the engine refuses
// rather than one it rounds into shape.
func TestAddSchemaRejectsNegativeDecimalScale(t *testing.T) {
	db := armStore(t)
	ts := newSchemaEntry()
	rec := &metadata.Record{
		ObjectID: 1, Revision: 1,
		Fields: []metadata.Field{{ID: metadata.TableName, Value: "t"}},
	}
	cols := []*metadata.Record{metaColumnRecord(1<<32, 1, "amount", "decimal", -1)}

	require.ErrorContains(t, db.addSchema(ts, rec, cols), "negative scale")
	require.Empty(t, ts.versions, "a schema that does not validate is not published")
}

// TestDeriveTablesRejectsSchemaWiderThanCodecLimit: the stored schema is valid
// on its own but wider than the codec policy the store is opened with. The
// decoder cannot be compiled, and unlike an unknown column type this is not
// something to skip — the table would be unreadable, so the open fails.
func TestDeriveTablesRejectsSchemaWiderThanCodecLimit(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(tmpdb(t), "store")
	db, err := Create(base, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "a", Type: TypeUint64}, {Name: "b", Type: TypeUint64}}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(base, Options{Limits: Limits{MaxColumns: 1}})
	require.ErrorContains(t, err, "columns, limit 1", "a schema the codec policy cannot compile fails the open")
}

// TestFieldSintFallsBackToZero: a field that is absent, or present with a
// value that is not a sint, reads as zero. Schema derivation never guesses a
// value out of a field it does not understand.
func TestFieldSintFallsBackToZero(t *testing.T) {
	require.Zero(t, fieldSint(&metadata.Record{}, metadata.ColDataScale), "no field, no value")

	wrongType := &metadata.Record{Fields: []metadata.Field{{ID: metadata.ColDataScale, Value: "2"}}}
	require.Zero(t, fieldSint(wrongType, metadata.ColDataScale), "a string is not a sint")

	right := &metadata.Record{Fields: []metadata.Field{{ID: metadata.ColDataScale, Value: int64(2)}}}
	require.Equal(t, int64(2), fieldSint(right, metadata.ColDataScale))
	require.Zero(t, fieldString(right, metadata.ColDataScale), "nor is a sint a string")
}

// TestTypeNameReportsUnknownType: a codec type the engine has no canonical
// string for is named "unknown", never an empty string that would look like a
// valid, nameless type.
func TestTypeNameReportsUnknownType(t *testing.T) {
	require.Equal(t, "unknown", typeName(codec.Type(0)), "the zero type has no canonical string")
	require.Equal(t, "int64", typeName(codec.TypeInt64))
}

// TestReadMetadataInheritsFromParent: a metadata object of an ancestor snapshot
// is visible in the child — that is what makes a table defined once readable
// in every snapshot below it. The child index carries no entry of its own, so
// the read climbs to the layer that does.
func TestReadMetadataInheritsFromParent(t *testing.T) {
	db := armCommittedStore(t) // snapshot 1 holds the table record
	oid, _ := realTableLoc(t, db)
	view := craftView(t, db, 2, 1, nil) // snapshot 2 adds no metadata of its own

	rec, err := db.readMetadataCached(view, 2, oid, nil)
	require.NoError(t, err, "the record is inherited from the parent layer")
	require.Equal(t, oid, rec.ObjectID)
}

// TestReadMetadataReportsObjectAbsentInChain: no layer of the chain carries the
// object. The answer is "not found", not a zero record.
func TestReadMetadataReportsObjectAbsentInChain(t *testing.T) {
	db := armCommittedStore(t)
	view := craftView(t, db, 2, 1, nil)

	_, err := db.readMetadataCached(view, 2, metadata.TableSpaceEnd+7, nil)
	require.ErrorIs(t, err, ErrNotFound)
	require.ErrorContains(t, err, "metadata object")
}
