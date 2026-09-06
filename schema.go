package rowpack

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// SchemaIndex maps (SnapshotID, TableID) -> ordered schema versions and the
// resolved codec.Schema per version. It is derived from core Table+Column
// metadata records and is immutable once built.
type SchemaIndex struct {
	bySnapshot map[uint64]map[uint32]*tableSchemas
}

type tableSchemas struct {
	versions []uint32 // sorted ascending
	byVer    map[uint32]*codec.Schema
}

// Schema returns the schema for (snapshot, table, version), or nil.
func (si *SchemaIndex) Schema(snapshot uint64, table uint32, version uint32) *codec.Schema {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil {
		return nil
	}
	return ts.byVer[version]
}

// Versions returns the sorted schema versions of (snapshot, table).
func (si *SchemaIndex) Versions(snapshot uint64, table uint32) []uint32 {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil {
		return nil
	}
	out := make([]uint32, len(ts.versions))
	copy(out, ts.versions)
	return out
}

// Latest returns the highest schema version of (snapshot, table).
func (si *SchemaIndex) Latest(snapshot uint64, table uint32) uint32 {
	ts := si.bySnapshot[snapshot][table]
	if ts == nil || len(ts.versions) == 0 {
		return 0
	}
	return ts.versions[len(ts.versions)-1]
}

// buildSchemaIndex derives schemas for every snapshot in the view by reading
// its Table and Column metadata records (resolved along the parent chain).
func (s *Store) buildSchemaIndex(view *index.View) (*SchemaIndex, error) {
	si := &SchemaIndex{bySnapshot: make(map[uint64]map[uint32]*tableSchemas)}
	for _, sm := range view.Snapshots() {
		tables, err := s.deriveTables(view, sm.ID)
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
func (s *Store) deriveTables(view *index.View, snapshot uint64) (map[uint32]*tableSchemas, error) {
	result := make(map[uint32]*tableSchemas)
	walk := func(snap uint64) error {
		// Gather table + column records of this snapshot layer.
		tableIDs := view.MetadataByType(snap, uint32(fileformat.RecordTable))
		for _, oid := range tableIDs {
			loc := view.Metadata(snap, oid)
			if loc == nil || loc.Operation == fileformat.OperationDelete {
				continue
			}
			rec, err := s.readMetadataRecord(view, snap, oid)
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
			if err := s.addDerivedSchema(view, snap, ts, rec); err != nil {
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
func (s *Store) addDerivedSchema(view *index.View, snapshot uint64, ts *tableSchemas, tableRec *metadata.Record) error {
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
		rec, err := s.readMetadataRecord(view, snapshot, cid)
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
	dataType := fieldString(rec, metadata.ColDataType)
	colType := fieldString(rec, metadata.ColColumnType)
	nullableStr := fieldString(rec, metadata.ColNullable)
	t, err := dialectType(dataType, colType)
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
	// SafeString 原文：NO / no / N 视为不可空，其余（YES、空、NULL、1…）视为可空。
	switch s {
	case "NO", "No", "no", "N", "0", "FALSE", "false":
		return false
	}
	return true
}

// dialectType maps the built-in canonical DataType/ColumnType strings to the
// TypedTuple type. Real database dialects register their own adapters (M9);
// the built-in dialect accepts exactly the canonical strings written by
// DefineSchema.
func dialectType(dataType, columnType string) (codec.Type, error) {
	t := columnType
	if t == "" {
		t = dataType
	}
	t = lowerAscii(t)
	switch t {
	case "bool", "boolean":
		return codec.TypeBool, nil
	case "int8", "tinyint":
		return codec.TypeInt8, nil
	case "int16", "smallint":
		return codec.TypeInt16, nil
	case "int32", "int", "integer":
		return codec.TypeInt32, nil
	case "int64", "bigint":
		return codec.TypeInt64, nil
	case "uint8":
		return codec.TypeUint8, nil
	case "uint16":
		return codec.TypeUint16, nil
	case "uint32":
		return codec.TypeUint32, nil
	case "uint64":
		return codec.TypeUint64, nil
	case "float32", "float", "real":
		return codec.TypeFloat32, nil
	case "float64", "double":
		return codec.TypeFloat64, nil
	case "string", "varchar", "text", "char":
		return codec.TypeString, nil
	case "bytes", "blob", "binary":
		return codec.TypeBytes, nil
	case "date":
		return codec.TypeDate, nil
	case "time":
		return codec.TypeTime, nil
	case "datetime", "timestamp":
		return codec.TypeDateTime, nil
	case "decimal", "numeric":
		return codec.TypeDecimal, nil
	}
	return 0, fmt.Errorf("rowpack: unknown column type %q", t)
}

// dialectName maps a codec type to the built-in canonical dialect string
// (the inverse of dialectType).
func dialectName(t codec.Type) string {
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

// nullString maps nullable to the SafeString-preserving YES/NO text.
func nullString(nullable bool) string {
	if nullable {
		return "YES"
	}
	return "NO"
}

func lowerAscii(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// readMetadataRecord reads and decodes one metadata record from the data file
// via its index location, resolving along the parent chain.
func (s *Store) readMetadataRecord(view *index.View, snapshot, objectID uint64) (*metadata.Record, error) {
	loc := view.Metadata(snapshot, objectID)
	if loc == nil {
		// Walk the parent chain for the record.
		sm := view.Snapshot(snapshot)
		if sm == nil || sm.Parent == 0 {
			return nil, fmt.Errorf("%w: metadata object %d in snapshot %d", ErrNotFound, objectID, snapshot)
		}
		return s.readMetadataRecord(view, sm.Parent, objectID)
	}
	if loc.Operation == fileformat.OperationDelete {
		return nil, fmt.Errorf("%w: metadata object %d deleted", ErrNotFound, objectID)
	}
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

var _ = errors.New
var _ = strconv.Itoa
