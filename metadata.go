package rowpack

import (
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/metadata"
)

// MetadataWireType is the TLV wire type of a metadata field value.
type MetadataWireType uint8

const (
	WireBool          MetadataWireType = MetadataWireType(fileformat.WireBool)
	WireUint          MetadataWireType = MetadataWireType(fileformat.WireUint)
	WireSint          MetadataWireType = MetadataWireType(fileformat.WireSint)
	WireString        MetadataWireType = MetadataWireType(fileformat.WireString)
	WireBytes         MetadataWireType = MetadataWireType(fileformat.WireBytes)
	WireObjectRef     MetadataWireType = MetadataWireType(fileformat.WireObjectRef)
	WireStringList    MetadataWireType = MetadataWireType(fileformat.WireStringList)
	WireObjectRefList MetadataWireType = MetadataWireType(fileformat.WireObjectRefList)
	WireExpression    MetadataWireType = MetadataWireType(fileformat.WireExpression)
	WireFieldSet      MetadataWireType = MetadataWireType(fileformat.WireFieldSet)
)

// MetadataField is one TLV field of a metadata record. Value holds the
// decoded Go value matching the wire type (bool, uint64, int64, string,
// []byte, []string, []uint64, or a nested []MetadataField).
type MetadataField struct {
	ID       uint16
	WireType MetadataWireType
	Critical bool
	Repeated bool
	Value    any
}

// MetadataRecord is the generic metadata record envelope.
type MetadataRecord struct {
	ObjectID    MetadataID
	ParentID    MetadataID
	Revision    uint32
	Type        MetadataType
	Namespace   string
	ExternalKey string
	Critical    bool
	Fields      []MetadataField
}

// MetadataQuery filters ListMetadata.
type MetadataQuery struct {
	Type      MetadataType // 0 = all
	ParentID  MetadataID   // 0 = no filter
	Namespace string       // "" = no filter
}

// PutMetadata writes a generic metadata record into this snapshot. Unknown
// non-critical fields are preserved verbatim; unknown critical fields are
// rejected.
func (w *SnapshotWriter) PutMetadata(record MetadataRecord) error {
	if err := w.checkState(); err != nil {
		return err
	}
	rec := &metadata.Record{
		RecordType:  uint32(record.Type),
		ObjectID:    record.ObjectID,
		ParentID:    record.ParentID,
		Revision:    record.Revision,
		Critical:    record.Critical,
		Namespace:   record.Namespace,
		ExternalKey: record.ExternalKey,
	}
	for _, f := range record.Fields {
		rec.Fields = append(rec.Fields, metadata.Field{
			ID:       f.ID,
			WireType: fileformat.WireType(f.WireType),
			Critical: f.Critical,
			Repeated: f.Repeated,
			Value:    f.Value,
		})
	}
	return w.writeMetadata(rec)
}

// DeleteMetadata marks a metadata object deleted in this snapshot. The
// deletion is a directory-only entry; the object's prior record remains
// readable in ancestor snapshots.
func (w *SnapshotWriter) DeleteMetadata(id MetadataID, typ MetadataType) error {
	if err := w.checkState(); err != nil {
		return err
	}
	entry := metadata.DirectoryEntry{
		ObjectID:   id,
		RecordType: uint32(typ),
		Operation:  fileformat.OperationDelete,
	}
	return w.ensureMetaBuilder().Add(entry, nil)
}
