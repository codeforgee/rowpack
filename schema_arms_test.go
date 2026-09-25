package rowpack

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// schema_arms_test.go 覆盖 schema 推导的跳过臂、早退臂与回溯臂:没有版本的表不进
// 地址索引、未知表没有解码器、addSchema 的三条早退、比打开限制更宽的 schema 必须
// 让打开失败(而不是被静默误解码)、以及 metadata 记录沿父链回溯与彻底找不到。
//
// 剩下 8 个零覆盖块都是需要索引与块互相矛盾、或需要第二个 schema 版本才能走到的,
// 现状下不可达:DELETE 臂(写入只产生 Upsert)、table object 越界、父层缺失、
// 同一表的第二个版本(公开 API 不产生 v2)、块在视图里缺失、ordinal 越界;细节见
// schema_corrupt_arms_test.go 的头部注释。

// TestAddressIndexSkipsTablesWithoutSchema: a table whose schema never built
// (unknown column type, or no versions at all) owns no address, so it cannot
// shadow a table that did build.
func TestAddressIndexSkipsTablesWithoutSchema(t *testing.T) {
	built := func(name string) *tableSchemas {
		return &tableSchemas{
			versions: []uint32{1},
			byVer:    map[uint32]*codec.Schema{1: {Name: name}},
		}
	}
	si := addressIndex(map[uint32]*tableSchemas{
		1: {},  // no versions: nothing was derivable
		2: nil, // no entry at all
		3: built("t"),
		4: built("t"), // same address: the lowest id wins
	})
	require.Equal(t, map[string]TableID{"t": 3}, si)
}

// TestTypeNameUnknown: typeName is the inverse of columnType only for the
// strings the engine writes; anything else must be named, not guessed.
func TestTypeNameUnknown(t *testing.T) {
	for _, s := range []string{
		"bool", "int8", "int16", "int32", "int64", "uint8", "uint16", "uint32",
		"uint64", "float32", "float64", "string", "bytes", "date", "time",
		"datetime", "decimal",
	} {
		ct, err := columnType(s)
		require.NoError(t, err)
		require.Equal(t, s, typeName(ct), "typeName must invert columnType")
	}
	require.Equal(t, "unknown", typeName(codec.Type(99)))
	_, err := columnType("decimal128")
	require.ErrorIs(t, err, errUnknownColumnType)
}

// TestFieldAccessorsOnAbsentFields: schema derivation reads optional fields, so
// a missing one — or one stored under another wire type — is a zero value, not
// a panic.
func TestFieldAccessorsOnAbsentFields(t *testing.T) {
	rec := &metadata.Record{}
	require.Equal(t, "", fieldString(rec, metadata.TableName))
	require.Zero(t, fieldSint(rec, metadata.ColColumnID))

	// ColumnName and ColumnID share the numeric ids of TableName/ColColumnID
	// (fields are addressed per record type), so keep the ids distinct here.
	rec.Fields = []metadata.Field{
		{ID: metadata.ColColumnID, WireType: format.WireString, Value: "not a number"},
		{ID: metadata.ColColumnName, WireType: format.WireSint, Value: int64(7)},
	}
	require.Zero(t, fieldSint(rec, metadata.ColColumnID), "a sint field stored as a string reads as zero")
	require.Equal(t, "", fieldString(rec, metadata.ColColumnName), "a string field stored as a sint reads as empty")
}

// TestDecoderForUnknownTable: the read path resolves decoders from the index
// built at open; a location pointing at a table or version that index never saw
// is a schema mismatch, not a silent zero decoder.
func TestDecoderForUnknownTable(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t)
	snaps, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1)

	st := db.state.Load()
	tid, ok := st.schemas.tableID(uint64(snaps[0].ID), "t")
	require.True(t, ok)

	loc := &index.BlockLoc{SnapshotID: uint64(snaps[0].ID), TableID: uint32(tid)}
	_, err = st.schemas.decoderFor(loc, 99)
	require.ErrorIs(t, err, ErrSchemaMismatch, "an unknown version has no decoder")

	loc.TableID = 4242
	_, err = st.schemas.decoderFor(loc, 1)
	require.ErrorIs(t, err, ErrSchemaMismatch, "an unknown table has no decoder")
}

// TestReadMetadataCachedWalksParentChain: a DELTA that writes no metadata of its
// own still resolves the definitions it inherits; an object no layer carries is
// simply absent.
func TestReadMetadataCachedWalksParentChain(t *testing.T) {
	ctx := context.Background()
	db := armCommittedStore(t) // snapshot 1 defines table t (object 1)
	tx := armTx(t, db, Latest)
	require.NoError(t, tx.Insert("t", 2, Row{Uint64(2)}))
	snap2, err := tx.Commit(ctx)
	require.NoError(t, err)

	st := db.state.Load()
	rec, err := db.readMetadataCached(st.view, uint64(snap2), 1, nil)
	require.NoError(t, err, "the record lives in the parent layer")
	require.EqualValues(t, format.RecordTable, rec.RecordType)

	_, err = db.readMetadataCached(st.view, uint64(snap2), 987654, nil)
	require.ErrorIs(t, err, ErrNotFound, "no layer of the chain carries this object")
}

// TestAddSchemaEarlyExits: version 0 is not a schema version, re-deriving the
// same version must not duplicate it, and a version that cannot validate is
// never recorded.
func TestAddSchemaEarlyExits(t *testing.T) {
	db := armStore(t)
	ts := &tableSchemas{
		byVer:    make(map[uint32]*codec.Schema),
		decoders: make(map[uint32]codec.Decoder),
	}
	cols := []*metadata.Record{armColumnRecord(2, 1, "id", "uint64")}

	require.NoError(t, db.addSchema(ts, armTableRecord(1, 0, "t"), cols))
	require.Empty(t, ts.versions, "revision 0 is not a schema version")

	require.NoError(t, db.addSchema(ts, armTableRecord(1, 1, "t"), cols))
	require.Equal(t, []uint32{1}, ts.versions)

	require.NoError(t, db.addSchema(ts, armTableRecord(1, 1, "t"), cols))
	require.Equal(t, []uint32{1}, ts.versions, "re-deriving a version is idempotent")

	require.ErrorContains(t, db.addSchema(ts, armTableRecord(1, 2, ""), cols), "name is empty")
	require.Equal(t, []uint32{1}, ts.versions, "a rejected version is not recorded")
}

// TestOpenRejectsSchemaWiderThanOpenLimits: a store written under one column
// limit is opened under a narrower one. Compiling the decoder is where the
// narrower limit bites, and the open must fail rather than decode rows with a
// schema it cannot represent.
func TestOpenRejectsSchemaWiderThanOpenLimits(t *testing.T) {
	ctx := context.Background()
	path := tmpdb(t)
	db, err := Create(path, Options{})
	require.NoError(t, err)

	cols := make([]Column, 20)
	for i := range cols {
		cols[i] = Column{Name: fmt.Sprintf("c%d", i), Type: TypeUint64}
	}
	tx := armTx(t, db, NoParent)
	require.NoError(t, tx.DefineTable("t", cols))
	row := make(Row, len(cols))
	for i := range row {
		row[i] = Uint64(uint64(i))
	}
	require.NoError(t, tx.Insert("t", 1, row))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	_, err = Open(path, Options{Limits: Limits{MaxColumns: 8}})
	require.ErrorContains(t, err, "limit 8")
}

// armTableRecord builds a Table metadata record as the writer would.
func armTableRecord(objectID uint64, revision uint32, name string) *metadata.Record {
	return &metadata.Record{
		RecordType:  uint32(format.RecordTable),
		ObjectID:    objectID,
		Revision:    revision,
		Namespace:   format.NamespaceCore,
		ExternalKey: name,
		Fields: []metadata.Field{
			{ID: metadata.TableName, WireType: format.WireString, Value: name},
		},
	}
}

// armColumnRecord builds a Column metadata record as the writer would.
func armColumnRecord(objectID uint64, columnID int64, name, typ string) *metadata.Record {
	return &metadata.Record{
		RecordType: uint32(format.RecordColumn),
		ObjectID:   objectID,
		Revision:   1,
		Namespace:  format.NamespaceCore,
		Fields: []metadata.Field{
			{ID: metadata.ColColumnID, WireType: format.WireSint, Value: columnID},
			{ID: metadata.ColColumnName, WireType: format.WireString, Value: name},
			{ID: metadata.ColColumnType, WireType: format.WireString, Value: typ},
			{ID: metadata.ColNullable, WireType: format.WireString, Value: "NO"},
			{ID: metadata.ColDataScale, WireType: format.WireSint, Value: int64(0)},
		},
	}
}
