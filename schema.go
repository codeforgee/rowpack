package rowpack

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/metadata"
)

// schemaIndex maps (SnapshotID, TableID) -> ordered schema versions and the
// resolved codec.Schema per version. It is derived from core Table+Column
// metadata records and is immutable once built.
type schemaIndex struct {
	bySnapshot map[uint64]map[uint32]*tableSchemas
	byAddress  map[uint64]map[string]TableID
}

// newSchemaIndex returns an empty index with all lookup directions ready.
func newSchemaIndex() *schemaIndex {
	return &schemaIndex{
		bySnapshot: make(map[uint64]map[uint32]*tableSchemas),
		byAddress:  make(map[uint64]map[string]TableID),
	}
}

// addressIndex maps the latest schema address of every table to its ID; tables
// whose latest schema could not be built are skipped.
//
// DefineTable rejects two tables claiming the same address, so collisions are
// impossible in a store the engine wrote. The lowest TableID wins if one is
// nonetheless present, keeping lookups deterministic instead of map-order
// dependent; Verify reports the duplicate.
func addressIndex(tables map[uint32]*tableSchemas) map[string]TableID {
	if len(tables) == 0 {
		return nil
	}
	tids := make([]uint32, 0, len(tables))
	for tid := range tables {
		tids = append(tids, tid)
	}
	slices.Sort(tids)
	addrs := make(map[string]TableID, len(tables))
	for _, tid := range tids {
		ts := tables[tid]
		if ts == nil || len(ts.versions) == 0 {
			continue
		}
		if s := ts.byVer[ts.versions[len(ts.versions)-1]]; s != nil {
			if _, taken := addrs[Qualify(ts.ns, s.Name)]; !taken {
				addrs[Qualify(ts.ns, s.Name)] = TableID(tid)
			}
		}
	}
	return addrs
}

type tableSchemas struct {
	versions []uint32 // sorted ascending
	byVer    map[uint32]*codec.Schema
	decoders map[uint32]codec.Decoder
	ns       string
}

// nameOf returns the bare name of a table at a snapshot, or "" when unknown.
func (si *schemaIndex) nameOf(snapshot uint64, table uint32) string {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil || len(ts.versions) == 0 {
		return ""
	}
	if sch := ts.byVer[ts.versions[len(ts.versions)-1]]; sch != nil {
		return sch.Name
	}
	return ""
}

// nsOf returns the ns a table resolves to at a snapshot (NSUser when the Table
// record carries none).
func (si *schemaIndex) nsOf(snapshot uint64, table uint32) string {
	if ts := si.bySnapshot[snapshot][table]; ts != nil && ts.ns != "" {
		return ts.ns
	}
	return NSUser
}

// Schema returns the schema for (snapshot, table, version), or nil.
func (si *schemaIndex) schema(snapshot uint64, table uint32, version uint32) *codec.Schema {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil {
		return nil
	}
	return ts.byVer[version]
}

// Latest returns the highest schema version of (snapshot, table).
func (si *schemaIndex) latest(snapshot uint64, table uint32) uint32 {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil || len(ts.versions) == 0 {
		return 0
	}
	return ts.versions[len(ts.versions)-1]
}

// maxColumns returns the widest column count among the schema versions of
// (snapshot, table), or 0 when the table has no usable schema. Batch decode
// sizes its row slab with it instead of resolving a schema per record.
func (si *schemaIndex) maxColumns(snapshot uint64, table uint32) int {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil {
		return 0
	}
	max := 0
	for _, v := range ts.versions {
		if s := ts.byVer[v]; s != nil && len(s.Columns) > max {
			max = len(s.Columns)
		}
	}
	return max
}

// tableID resolves a table address at a snapshot to its internal ID through the
// reverse index. The index for a snapshot already covers every ancestor layer
// (deriveTables walks the chain).
func (si *schemaIndex) tableID(snapshot uint64, address string) (TableID, bool) {
	tid, ok := si.byAddress[snapshot][address]
	return tid, ok
}

// decoderFor resolves the immutable, schema-compiled row decoder created
// while the schema index was built. Read paths never compile execution plans.
func (si *schemaIndex) decoderFor(bl *index.BlockLoc, version uint32) (codec.Decoder, error) {
	ts := si.bySnapshot[bl.SnapshotID][bl.TableID]
	if ts == nil {
		return codec.Decoder{}, fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, version)
	}
	decoder, ok := ts.decoders[version]
	if !ok {
		return codec.Decoder{}, fmt.Errorf("%w: decoder for table %d version %d not found", ErrSchemaMismatch, bl.TableID, version)
	}
	return decoder, nil
}

// blockDecoders resolves schema-bound row decoders against the schema index
// while memoizing the current block's last version. Blocks normally hold a
// single schema version, so consecutive rows of one page — and the pages of
// one block — skip the index walk and reuse the compiled decoder, which also
// hoists invariant schema/bitmap work out of every row decode. Rebind it when
// the block cursor moves. Not safe for concurrent use; one per read cursor.
type blockDecoders struct {
	si  *schemaIndex
	blk *index.BlockLoc

	ver uint32
	dec codec.Decoder
	ok  bool
}

// bind points the memo at a new block cursor and invalidates the cached
// decoder.
func (d *blockDecoders) bind(si *schemaIndex, blk *index.BlockLoc) {
	d.si, d.blk, d.ok = si, blk, false
}

// forVersion returns the prepared decoder for a schema version under the
// bound block.
func (d *blockDecoders) forVersion(ver uint32) (codec.Decoder, error) {
	if d.ok && d.ver == ver {
		return d.dec, nil
	}
	dec, err := d.si.decoderFor(d.blk, ver)
	if err != nil {
		return codec.Decoder{}, err
	}
	d.ver, d.dec, d.ok = ver, dec, true
	return dec, nil
}

// buildIndex derives schemas for every snapshot in the view by reading
// its Table and Column metadata records (resolved along the parent chain).
// One decode memo is shared across all snapshots: deriveTables for snapshot
// S re-reads every ancestor layer's records, so without the memo an open
// with N snapshots decompresses the same metadata blocks O(N²) times.
func (s *Store) buildIndex(view *index.View) (*schemaIndex, error) {
	si := newSchemaIndex()
	memo := make(map[metaRecKey]*metadata.Record)
	for _, sm := range view.Snapshots() {
		d, err := s.deriveTables(view, sm.ID, memo)
		if err != nil {
			return nil, err
		}
		if len(d.tables) > 0 {
			si.bySnapshot[sm.ID] = d.tables
		}
		if len(d.byAddress) > 0 {
			si.byAddress[sm.ID] = d.byAddress
		}
	}
	return si, nil
}

// derivedSchemas is the schema derivation result for one snapshot.
type derivedSchemas struct {
	tables    map[uint32]*tableSchemas
	byAddress map[string]TableID
}

// deriveTables builds the schema index for one snapshot by walking its
// metadata (UPSERT records win over parent records; DELETE hides them).
// memo (may be nil) caches decoded records by physical block slot across
// calls; see buildIndex.
func (s *Store) deriveTables(view *index.View, snapshot uint64, memo map[metaRecKey]*metadata.Record) (*derivedSchemas, error) {
	result := make(map[uint32]*tableSchemas)
	// deleted holds object ids a DELETE entry shadows: readers resolve records
	// along the parent chain, so a tombstone on a newer layer must hide the
	// same object's older definitions (and the columns hanging off a shadowed
	// table). Without this, Tables() reports the table gone while Get/Schema
	// still resolve it through the chain — the two answers disagree.
	deleted := make(map[uint64]struct{})
	walk := func(snap uint64) error {
		// Decode and group the layer's columns once. Previously every table
		// rescanned every column, making schema derivation quadratic in tables.
		columnsByParent := make(map[uint64][]*metadata.Record)
		for _, cid := range view.MetadataByType(snap, uint32(format.RecordColumn)) {
			loc := view.Metadata(snap, cid)
			if loc == nil || loc.RecordType != uint32(format.RecordColumn) {
				continue
			}
			if loc.Operation == format.OperationDelete {
				deleted[cid] = struct{}{}
				continue
			}
			if _, gone := deleted[cid]; gone {
				continue
			}
			rec, err := s.readMetadataCached(view, snap, cid, memo)
			if err != nil {
				return err
			}
			if _, gone := deleted[rec.ParentID]; gone {
				continue // the table this column belongs to is shadowed
			}
			columnsByParent[rec.ParentID] = append(columnsByParent[rec.ParentID], rec)
		}
		// Gather table records of this snapshot layer.
		tableIDs := view.MetadataByType(snap, uint32(format.RecordTable))
		for _, oid := range tableIDs {
			loc := view.Metadata(snap, oid)
			if loc == nil {
				continue
			}
			if loc.Operation == format.OperationDelete {
				deleted[oid] = struct{}{}
				continue
			}
			if _, gone := deleted[oid]; gone {
				continue
			}
			rec, err := s.readMetadataCached(view, snap, oid, memo)
			if err != nil {
				return err
			}
			tableID, err := metadata.TableID(oid)
			if err != nil {
				return fmt.Errorf("rowpack: table object %d: %w", oid, err)
			}
			ts := result[tableID]
			if ts == nil {
				ts = &tableSchemas{byVer: make(map[uint32]*codec.Schema), decoders: make(map[uint32]codec.Decoder)}
				result[tableID] = ts
			}
			before := len(ts.versions)
			if err := s.addSchema(ts, rec, columnsByParent[rec.ObjectID]); err != nil {
				if errors.Is(err, errUnknownColumnType) {
					// The engine does not interpret this table's column type
					// strings: the records are plain stored data. Skip the
					// table without failing the open.
					if len(ts.versions) == before {
						delete(result, tableID)
					}
					continue
				}
				return err
			}
			if ts.ns == "" {
				if ns := fieldString(rec, metadata.TableNS); ns != "" {
					ts.ns = ns
				}
			}
		}
		return nil
	}
	// Walk from the snapshot up to its FULL ancestor.
	if sm := view.Snapshot(snapshot); sm != nil {
		for _, snap := range sm.Chain() {
			if err := walk(snap); err != nil {
				return nil, err
			}
		}
	}
	for _, ts := range result {
		slices.Sort(ts.versions)
	}
	return &derivedSchemas{tables: result, byAddress: addressIndex(result)}, nil
}

// addSchema resolves one Table record and its columns into a
// codec.Schema for the given schema version (Table Revision).
func (s *Store) addSchema(ts *tableSchemas, tableRec *metadata.Record, columnRecords []*metadata.Record) error {
	version := tableRec.Revision
	if version == 0 {
		return nil
	}
	if existing, ok := ts.byVer[version]; ok {
		// Idempotent: same version already derived; verify consistency later.
		_ = existing
		return nil
	}
	name := fieldString(tableRec, metadata.TableName)
	// The table object ID was validated to fit a uint32 by the deriveTables
	// walk (metadata.TableID), so the direct conversion cannot lose bits.
	schema := &codec.Schema{TableID: uint32(tableRec.ObjectID), Version: version, Name: name}
	var derived []derivedColumn
	for _, rec := range columnRecords {
		col, err := deriveColumn(rec)
		if err != nil {
			return fmt.Errorf("rowpack: derive column %d: %w", rec.ObjectID, err)
		}
		derived = append(derived, col)
	}
	// Sort columns by column ID (field 10), stable: column records sharing
	// an ID keep their record order.
	slices.SortStableFunc(derived, func(a, b derivedColumn) int { return cmp.Compare(a.columnID, b.columnID) })
	for _, c := range derived {
		schema.Columns = append(schema.Columns, c.codecColumn)
	}
	if err := schema.Validate(codec.DefaultLimits()); err != nil {
		return err
	}
	decoder, err := s.rowCodec().CompileDecoder(schema)
	if err != nil {
		return err
	}
	ts.byVer[version] = schema
	ts.decoders[version] = decoder
	ts.versions = append(ts.versions, version)
	return nil
}

type derivedColumn struct {
	columnID    int64
	codecColumn codec.Column
}

func deriveColumn(rec *metadata.Record) (derivedColumn, error) {
	colType := fieldString(rec, metadata.ColColumnType)
	nullableStr := fieldString(rec, metadata.ColNullable)
	t, err := columnType(colType)
	if err != nil {
		return derivedColumn{}, err
	}
	scale := int32(fieldSint(rec, metadata.ColDataScale))
	pk := fieldSint(rec, metadata.ColPrimaryKey) == 1
	return derivedColumn{
		columnID: fieldSint(rec, metadata.ColColumnID),
		codecColumn: codec.Column{
			Name:       fieldString(rec, metadata.ColColumnName),
			Type:       t,
			Nullable:   isNullableString(nullableStr),
			Scale:      scale,
			PrimaryKey: pk,
		},
	}, nil
}

func fieldString(rec *metadata.Record, id uint16) string {
	f := rec.FieldByID(id)
	if f == nil {
		return ""
	}
	s, _ := f.Value.(string)
	return s
}

func fieldSint(rec *metadata.Record, id uint16) int64 {
	f := rec.FieldByID(id)
	if f == nil {
		return 0
	}
	v, _ := f.Value.(int64)
	return v
}

// isNullableString decodes the nullable marker written by DefineSchema (the
// write side emits the canonical YES/NO text; the extra accepted spellings
// are tolerance for foreign writers).
func isNullableString(s string) bool {
	// 可空性按 DefineSchema 写入的 YES/NO 原文判断（NO/no/N/0/FALSE 视为不可空）。
	switch s {
	case "NO", "No", "no", "N", "0", "FALSE", "false":
		return false
	}
	return true
}

// errUnknownColumnType marks a Column record whose type string the engine
// does not interpret. Such records are treated as plain stored data: their
// table is not added to the schema index, and opening never fails because of
// them.
var errUnknownColumnType = errors.New("rowpack: unknown column type string")

// columnTypeNames maps every engine-interpreted type to its canonical type
// string — the single source of truth for the (codec.Type <-> string) mapping
// written by DefineSchema and resolved by schema derivation. columnTypes is
// its inverse.
var columnTypeNames = map[codec.Type]string{
	codec.TypeBool:       "bool",
	codec.TypeInt8:       "int8",
	codec.TypeInt16:      "int16",
	codec.TypeInt32:      "int32",
	codec.TypeInt64:      "int64",
	codec.TypeUint8:      "uint8",
	codec.TypeUint16:     "uint16",
	codec.TypeUint32:     "uint32",
	codec.TypeUint64:     "uint64",
	codec.TypeFloat32:    "float32",
	codec.TypeFloat64:    "float64",
	codec.TypeString:     "string",
	codec.TypeBytes:      "bytes",
	codec.TypeDate:       "date",
	codec.TypeTime:       "time",
	codec.TypeDateTime:   "datetime",
	codec.TypeDateTimeTZ: "datetime_tz",
	codec.TypeDecimal:    "decimal",
}

var columnTypes = func() map[string]codec.Type {
	m := make(map[string]codec.Type, len(columnTypeNames))
	for t, name := range columnTypeNames {
		m[name] = t
	}
	return m
}()

// columnType resolves the engine-interpreted TypedTuple type of a Column
// record's canonical type string. Only the strings written by DefineSchema
// are understood; unknown type strings mark the record as plain stored data
// (their table is skipped in the schema index, never failing the open).
func columnType(t string) (codec.Type, error) {
	if v, ok := columnTypes[t]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("%w: %q", errUnknownColumnType, t)
}

// typeName maps a codec type to its canonical type string (the inverse of
// columnType).
func typeName(t codec.Type) string {
	if name, ok := columnTypeNames[t]; ok {
		return name
	}
	return "unknown"
}

// metaRecKey identifies a metadata record by its physical block slot; the
// decoded content of a slot never changes, so it memoizes safely across
// snapshots (the record is treated as read-only by all callers).
type metaRecKey struct {
	blockID uint64
	ordinal uint32
}

// readMetadataCached reads and decodes one metadata record from the data
// file via its index location, resolving along the parent chain. An optional
// memo avoids decoding the same physical record repeatedly.
func (s *Store) readMetadataCached(view *index.View, snapshot, objectID uint64, memo map[metaRecKey]*metadata.Record) (*metadata.Record, error) {
	sm := view.Snapshot(snapshot)
	if sm == nil {
		return nil, fmt.Errorf("%w: metadata object %d in snapshot %d", ErrNotFound, objectID, snapshot)
	}
	for _, cur := range sm.Chain() {
		loc := view.Metadata(cur, objectID)
		if loc == nil {
			continue
		}
		if loc.Operation == format.OperationDelete {
			return nil, fmt.Errorf("%w: metadata object %d deleted", ErrNotFound, objectID)
		}
		if memo != nil {
			key := metaRecKey{blockID: loc.BlockID, ordinal: loc.ItemOrdinal}
			if rec, ok := memo[key]; ok {
				return rec, nil
			}
			rec, err := s.decodeRecord(view, loc, objectID)
			if err != nil {
				return nil, err
			}
			memo[key] = rec
			return rec, nil
		}
		return s.decodeRecord(view, loc, objectID)
	}
	return nil, fmt.Errorf("%w: metadata object %d in snapshot %d", ErrNotFound, objectID, snapshot)
}

// decodeRecord loads, parses and decodes the metadata record at loc.
func (s *Store) decodeRecord(view *index.View, loc *index.MetadataLoc, objectID uint64) (*metadata.Record, error) {
	bl := view.Block(loc.BlockID)
	if bl == nil {
		return nil, fmt.Errorf("rowpack: metadata object %d block %d missing", objectID, loc.BlockID)
	}
	blk, err := s.loader.Load(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return nil, err
	}
	payload, err := metadata.Parse(blk.Raw)
	if err != nil {
		return nil, s.recordError(bl, SnapshotID(bl.SnapshotID), TableID(bl.TableID), err)
	}
	if int(loc.ItemOrdinal) >= len(payload.Records) {
		return nil, s.recordError(bl, SnapshotID(bl.SnapshotID), TableID(bl.TableID),
			fmt.Errorf("rowpack: metadata object %d ordinal %d out of range", objectID, loc.ItemOrdinal))
	}
	rec := &metadata.Record{}
	raw := payload.Records[loc.ItemOrdinal]
	if len(raw) >= 8 {
		rec.RecordType = binary.LittleEndian.Uint32(raw[4:])
	}
	if err := rec.Decode(raw, metadata.CoreFieldSchemas[rec.RecordType]); err != nil {
		return nil, s.recordError(bl, SnapshotID(bl.SnapshotID), TableID(bl.TableID), err)
	}
	return rec, nil
}
