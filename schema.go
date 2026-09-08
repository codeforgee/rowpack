package rowpack

import (
	"errors"
	"fmt"
	"sort"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// schemaIndex maps (SnapshotID, TableID) -> ordered schema versions and the
// resolved codec.Schema per version. It is derived from core Table+Column
// metadata records and is immutable once built.
type schemaIndex struct {
	bySnapshot map[uint64]map[uint32]*tableSchemas
}

type tableSchemas struct {
	versions []uint32 // sorted ascending
	byVer    map[uint32]*codec.Schema
}

// Schema returns the schema for (snapshot, table, version), or nil.
func (si *schemaIndex) schema(snapshot uint64, table uint32, version uint32) *codec.Schema {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil {
		return nil
	}
	return ts.byVer[version]
}

// Versions returns the sorted schema versions of (snapshot, table).
func (si *schemaIndex) versions(snapshot uint64, table uint32) []uint32 {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil {
		return nil
	}
	out := make([]uint32, len(ts.versions))
	copy(out, ts.versions)
	return out
}

// Latest returns the highest schema version of (snapshot, table).
func (si *schemaIndex) latest(snapshot uint64, table uint32) uint32 {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil || len(ts.versions) == 0 {
		return 0
	}
	return ts.versions[len(ts.versions)-1]
}

// schemaFor resolves the codec schema for one block's version, reporting
// ErrSchemaMismatch when the metadata layer has no such schema version.
// Shared by the Get/Scan/batch decode paths.
func (si *schemaIndex) schemaFor(bl *index.BlockLoc, version uint32) (*codec.Schema, error) {
	schema := si.schema(bl.SnapshotID, bl.TableID, version)
	if schema == nil {
		return nil, fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, version)
	}
	return schema, nil
}

// buildSchemaIndex derives schemas for every snapshot in the view by reading
// its Table and Column metadata records (resolved along the parent chain).
// One decode memo is shared across all snapshots: deriveTables for snapshot
// S re-reads every ancestor layer's records, so without the memo an open
// with N snapshots decompresses the same metadata blocks O(N²) times.
func (s *Store) buildSchemaIndex(view *index.View) (*schemaIndex, error) {
	si := &schemaIndex{bySnapshot: make(map[uint64]map[uint32]*tableSchemas)}
	memo := make(map[metaRecKey]*metadata.Record)
	for _, sm := range view.Snapshots() {
		tables, err := s.deriveTables(view, sm.ID, memo)
		if err != nil {
			return nil, err
		}
		if len(tables) > 0 {
			si.bySnapshot[sm.ID] = tables
		}
	}
	return si, nil
}

// deriveTables builds the schema index for one snapshot by walking its
// metadata (UPSERT records win over parent records; DELETE hides them).
// memo (may be nil) caches decoded records by physical block slot across
// calls; see buildSchemaIndex.
func (s *Store) deriveTables(view *index.View, snapshot uint64, memo map[metaRecKey]*metadata.Record) (map[uint32]*tableSchemas, error) {
	result := make(map[uint32]*tableSchemas)
	walk := func(snap uint64) error {
		// Gather table + column records of this snapshot layer.
		tableIDs := view.MetadataByType(snap, uint32(fileformat.RecordTable))
		for _, oid := range tableIDs {
			loc := view.Metadata(snap, oid)
			if loc == nil || loc.Operation == fileformat.OperationDelete {
				continue
			}
			rec, err := s.readMetadataRecordMemo(view, snap, oid, memo)
			if err != nil {
				return err
			}
			tableID, err := metadata.TableID(oid)
			if err != nil {
				return fmt.Errorf("rowpack: table object %d: %w", oid, err)
			}
			ts := result[tableID]
			if ts == nil {
				ts = &tableSchemas{byVer: make(map[uint32]*codec.Schema)}
				result[tableID] = ts
			}
			before := len(ts.versions)
			if err := s.addDerivedSchema(view, snap, ts, rec, memo); err != nil {
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
		}
		return nil
	}
	// Walk from the snapshot up to its FULL ancestor.
	snap := snapshot
	for depth := 0; ; depth++ {
		sm := view.Snapshot(snap)
		if sm == nil {
			break
		}
		if err := walk(snap); err != nil {
			return nil, err
		}
		if sm.Parent == 0 {
			break
		}
		snap = sm.Parent
	}
	// Finalize ordering.
	for _, ts := range result {
		sort.Slice(ts.versions, func(i, j int) bool { return ts.versions[i] < ts.versions[j] })
	}
	return result, nil
}

// addDerivedSchema resolves one Table record and its columns into a
// codec.Schema for the given schema version (Table Revision).
func (s *Store) addDerivedSchema(view *index.View, snapshot uint64, ts *tableSchemas, tableRec *metadata.Record, memo map[metaRecKey]*metadata.Record) error {
	version := tableRec.Revision
	if version == 0 {
		return nil
	}
	if existing, ok := ts.byVer[version]; ok {
		// Idempotent: same version already derived; verify consistency later.
		_ = existing
		return nil
	}
	name := fieldString(tableRec, metadata.TableTableName)
	colIDs := view.MetadataByType(snapshot, uint32(fileformat.RecordColumn))
	schema := &codec.Schema{TableID: mustTableID(tableRec.ObjectID), Version: version, Name: name}
	var derived []derivedColumn
	for _, cid := range colIDs {
		loc := view.Metadata(snapshot, cid)
		if loc == nil || loc.Operation == fileformat.OperationDelete {
			continue
		}
		rec, err := s.readMetadataRecordMemo(view, snapshot, cid, memo)
		if err != nil {
			return err
		}
		// The column must belong to this table (ParentID == table object) and
		// be a Column (not a VirtualColumn) record.
		if rec.ParentID != tableRec.ObjectID {
			continue
		}
		if loc.RecordType != uint32(fileformat.RecordColumn) {
			continue
		}
		col, err := deriveColumn(rec)
		if err != nil {
			return fmt.Errorf("rowpack: derive column %d: %w", cid, err)
		}
		derived = append(derived, col)
	}
	// Sort columns by column ID (field 10).
	sort.SliceStable(derived, func(i, j int) bool {
		return derived[i].columnID < derived[j].columnID
	})
	for _, c := range derived {
		schema.Columns = append(schema.Columns, c.codecColumn)
	}
	if err := schema.Validate(codec.DefaultLimits()); err != nil {
		return err
	}
	ts.byVer[version] = schema
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
	return derivedColumn{
		columnID: fieldSint(rec, metadata.ColColumnID),
		codecColumn: codec.Column{
			Name:     fieldString(rec, metadata.ColColumnName),
			Type:     t,
			Nullable: isNullableString(nullableStr),
			Scale:    scale,
		},
	}, nil
}

func mustTableID(oid uint64) uint32 {
	id, _ := metadata.TableID(oid)
	return id
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

// columnType resolves the engine-interpreted TypedTuple type of a Column
// record's canonical type string. Only the strings written by DefineSchema
// are understood; unknown type strings mark the record as plain stored data
// (their table is skipped in the schema index, never failing the open).
func columnType(t string) (codec.Type, error) {
	switch t {
	case "bool":
		return codec.TypeBool, nil
	case "int8":
		return codec.TypeInt8, nil
	case "int16":
		return codec.TypeInt16, nil
	case "int32":
		return codec.TypeInt32, nil
	case "int64":
		return codec.TypeInt64, nil
	case "uint8":
		return codec.TypeUint8, nil
	case "uint16":
		return codec.TypeUint16, nil
	case "uint32":
		return codec.TypeUint32, nil
	case "uint64":
		return codec.TypeUint64, nil
	case "float32":
		return codec.TypeFloat32, nil
	case "float64":
		return codec.TypeFloat64, nil
	case "string":
		return codec.TypeString, nil
	case "bytes":
		return codec.TypeBytes, nil
	case "date":
		return codec.TypeDate, nil
	case "time":
		return codec.TypeTime, nil
	case "datetime":
		return codec.TypeDateTime, nil
	case "decimal":
		return codec.TypeDecimal, nil
	}
	return 0, fmt.Errorf("%w: %q", errUnknownColumnType, t)
}

// typeName maps a codec type to its canonical type string (the inverse of
// columnType).
func typeName(t codec.Type) string {
	switch t {
	case codec.TypeBool:
		return "bool"
	case codec.TypeInt8:
		return "int8"
	case codec.TypeInt16:
		return "int16"
	case codec.TypeInt32:
		return "int32"
	case codec.TypeInt64:
		return "int64"
	case codec.TypeUint8:
		return "uint8"
	case codec.TypeUint16:
		return "uint16"
	case codec.TypeUint32:
		return "uint32"
	case codec.TypeUint64:
		return "uint64"
	case codec.TypeFloat32:
		return "float32"
	case codec.TypeFloat64:
		return "float64"
	case codec.TypeString:
		return "string"
	case codec.TypeBytes:
		return "bytes"
	case codec.TypeDate:
		return "date"
	case codec.TypeTime:
		return "time"
	case codec.TypeDateTime:
		return "datetime"
	case codec.TypeDecimal:
		return "decimal"
	}
	return "unknown"
}

// nullString encodes nullable as the canonical YES/NO text written by DefineSchema.
func nullString(nullable bool) string {
	if nullable {
		return "YES"
	}
	return "NO"
}

// metaRecKey identifies a metadata record by its physical block slot; the
// decoded content of a slot never changes, so it memoizes safely across
// snapshots (the record is treated as read-only by all callers).
type metaRecKey struct {
	blockID uint64
	ordinal uint32
}

// readMetadataRecord reads and decodes one metadata record from the data file
// via its index location, resolving along the parent chain.
func (s *Store) readMetadataRecord(view *index.View, snapshot, objectID uint64) (*metadata.Record, error) {
	return s.readMetadataRecordMemo(view, snapshot, objectID, nil)
}

// readMetadataRecordMemo is readMetadataRecord with an optional decode memo
// keyed by physical block slot (nil memo decodes every time).
func (s *Store) readMetadataRecordMemo(view *index.View, snapshot, objectID uint64, memo map[metaRecKey]*metadata.Record) (*metadata.Record, error) {
	cur := snapshot
	for {
		loc := view.Metadata(cur, objectID)
		if loc != nil {
			if loc.Operation == fileformat.OperationDelete {
				return nil, fmt.Errorf("%w: metadata object %d deleted", ErrNotFound, objectID)
			}
			if memo != nil {
				key := metaRecKey{blockID: loc.BlockID, ordinal: loc.ItemOrdinal}
				if rec, ok := memo[key]; ok {
					return rec, nil
				}
				rec, err := s.decodeMetadataRecord(view, loc, objectID)
				if err != nil {
					return nil, err
				}
				memo[key] = rec
				return rec, nil
			}
			return s.decodeMetadataRecord(view, loc, objectID)
		}
		sm := view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			return nil, fmt.Errorf("%w: metadata object %d in snapshot %d", ErrNotFound, objectID, snapshot)
		}
		cur = sm.Parent
	}
}

// decodeMetadataRecord loads, parses and decodes the metadata record at loc.
func (s *Store) decodeMetadataRecord(view *index.View, loc *index.MetadataLoc, objectID uint64) (*metadata.Record, error) {
	bl := view.Block(loc.BlockID)
	if bl == nil {
		return nil, fmt.Errorf("rowpack: metadata object %d block %d missing", objectID, loc.BlockID)
	}
	blk, err := s.reader.ReadAtBlock(int64(bl.DataOffset))
	if err != nil {
		return nil, err
	}
	payload, err := metadata.Parse(blk.Raw)
	if err != nil {
		return nil, err
	}
	if int(loc.ItemOrdinal) >= len(payload.Records) {
		return nil, fmt.Errorf("rowpack: metadata object %d ordinal %d out of range", objectID, loc.ItemOrdinal)
	}
	rec := &metadata.Record{}
	if err := rec.Decode(payload.Records[loc.ItemOrdinal], metadata.CoreFieldSchemas[rec.RecordType]); err != nil {
		return nil, err
	}
	return rec, nil
}
