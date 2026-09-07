package rowpack

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// captureState loads the current published state once; the returned snapshot
// is immutable for the duration of the operation.
func (s *Store) captureState() (*publishedState, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	st := s.state.Load()
	if st == nil {
		return nil, ErrClosed
	}
	return st, nil
}

// Snapshot returns the metadata of a committed snapshot.
func (s *Store) Snapshot(ctx context.Context, id SnapshotID) (SnapshotInfo, error) {
	st, err := s.captureState()
	if err != nil {
		return SnapshotInfo{}, err
	}
	sm := st.view.Snapshot(id)
	if sm == nil {
		return SnapshotInfo{}, fmt.Errorf("%w: snapshot %d", ErrNotFound, id)
	}
	return snapshotInfoFromMeta(sm), nil
}

// LatestSnapshot returns the highest committed snapshot.
func (s *Store) LatestSnapshot(ctx context.Context) (SnapshotInfo, error) {
	st, err := s.captureState()
	if err != nil {
		return SnapshotInfo{}, err
	}
	sm := st.view.LatestSnapshot()
	if sm == nil {
		return SnapshotInfo{}, ErrNotFound
	}
	return snapshotInfoFromMeta(sm), nil
}

// ListSnapshots returns committed snapshots sorted by ID.
func (s *Store) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	metas := st.view.Snapshots()
	out := make([]SnapshotInfo, 0, len(metas))
	for _, sm := range metas {
		out = append(out, snapshotInfoFromMeta(sm))
	}
	return out, nil
}

func snapshotInfoFromMeta(sm *index.SnapshotMeta) SnapshotInfo {
	return SnapshotInfo{
		ID:          sm.ID,
		Type:        SnapshotType(sm.Type),
		Parent:      sm.Parent,
		CreatedAt:   time.Unix(0, sm.CreatedAtUnixNano).UTC(),
		BlockCount:  sm.BlockCount,
		ChangeCount: sm.RowRecordCount,
		RawBytes:    sm.RawBytes,
		StoredBytes: sm.StoredBytes,
	}
}

// Get returns the row visible at the given snapshot (resolved along the
// parent chain), decoding it into dst: the returned Row aliases dst and is
// overwritten by the next Get call on the same dst, reusing dst's backing
// array and any Decimal big.Int already held there. A nil dst allocates; keep
// the returned Row as the next dst to preserve the reuse. Getters of
// String/Bytes/Decimal return copies, so reading through them is always
// safe, but retained Value structs may observe overwritten Decimals after the
// next call. The contents of dst are unspecified if an error is returned.
// A DELETE tombstone or an absent row returns ErrNotFound.
func (s *Store) Get(ctx context.Context, snapshot SnapshotID, table TableID, rowID RowID, dst Row) (Row, error) {
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	loc := view.ResolveRow(snapshot, table, rowID)
	if loc == nil {
		return nil, fmt.Errorf("%w: (table %d, row %d) in snapshot %d", ErrNotFound, table, rowID, snapshot)
	}
	if loc.ChangeType == fileformat.ChangeDelete {
		return nil, fmt.Errorf("%w: (table %d, row %d) deleted in snapshot %d", ErrNotFound, table, rowID, snapshot)
	}
	row, _, err := s.readRowInto(view, st.schemas, loc, dst)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// Exists reports whether a row is visible (not deleted) at the snapshot.
// It resolves only the index/tombstone chain and does not read a block.
func (s *Store) Exists(ctx context.Context, snapshot SnapshotID, table TableID, rowID RowID) (bool, error) {
	st, err := s.captureState()
	if err != nil {
		return false, err
	}
	view := st.view
	if view.Snapshot(snapshot) == nil {
		return false, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	loc := view.ResolveRow(snapshot, table, rowID)
	if loc == nil || loc.ChangeType == fileformat.ChangeDelete {
		return false, nil
	}
	return true, nil
}

// readRowInto reads and decodes a single row into dst from its block via
// ParseRowAt, avoiding a full block directory parse for random reads.
func (s *Store) readRowInto(view *index.View, si *schemaIndex, loc *index.RowLoc, dst Row) (Row, SchemaVersion, error) {
	bl := view.Block(loc.BlockID)
	if bl == nil {
		return nil, 0, fmt.Errorf("rowpack: block %d missing from view", loc.BlockID)
	}
	blk, err := s.loader.Load(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return nil, 0, err
	}
	ref, err := block.ParseRowAt(blk.Raw, bl.ItemCount, loc.ItemOrdinal)
	if err != nil {
		return nil, 0, err
	}
	// Verify the row CRC for local diagnostics.
	if ref.Entry.ChangeType != fileformat.ChangeDelete {
		if got := fileformat.CRC32C(ref.Row); got != ref.Header.RowCRC32C {
			return nil, 0, fmt.Errorf("rowpack: row CRC mismatch in block %d", bl.BlockID)
		}
	}
	row, err := s.decodeRowInto(ref, bl, si, dst)
	return row, ref.Entry.SchemaVersion, err
}

// rowFromPayloadInto decodes the record at ordinal from an already-built rows
// directory into dst (used by Scan's block cursor). Callers must already have
// filtered tombstones. sink materializes String payloads (nil = fresh copy
// per value).
func (s *Store) rowFromPayloadInto(rp *block.RowsIndex, bl *index.BlockLoc, loc *index.RowLoc, si *schemaIndex, dst Row, sink codec.StringSink) (Row, error) {
	if int(loc.ItemOrdinal) >= len(rp.Entries) {
		return nil, fmt.Errorf("rowpack: row ordinal %d out of range in block %d", loc.ItemOrdinal, loc.BlockID)
	}
	ent := &rp.Entries[loc.ItemOrdinal]
	schema := si.schema(bl.SnapshotID, bl.TableID, ent.SchemaVersion)
	if schema == nil {
		return nil, fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, ent.SchemaVersion)
	}
	return codec.DecodeInto(dst, rp.RowBytes(int(loc.ItemOrdinal)), schema, s.opts.codecLimits(), sink)
}

// decodeRowInto decodes a located row into dst against its schema.
func (s *Store) decodeRowInto(ref *block.RowRef, bl *index.BlockLoc, si *schemaIndex, dst Row) (Row, error) {
	schema := si.schema(bl.SnapshotID, bl.TableID, ref.Entry.SchemaVersion)
	if schema == nil {
		return nil, fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, ref.Entry.SchemaVersion)
	}
	return codec.DecodeInto(dst, ref.Row, schema, s.opts.codecLimits(), nil)
}

// Schema returns the schema of a table version at a snapshot.
func (s *Store) Schema(ctx context.Context, snapshot SnapshotID, table TableID, version SchemaVersion) (Schema, error) {
	st, err := s.captureState()
	if err != nil {
		return Schema{}, err
	}
	if st.view.Snapshot(snapshot) == nil {
		return Schema{}, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	schema := st.schemas.schema(snapshot, table, version)
	if schema == nil {
		return Schema{}, fmt.Errorf("%w: schema for table %d version %d", ErrSchemaMismatch, table, version)
	}
	return *schema, nil
}

// LatestSchema returns the highest schema version of a table at a snapshot.
func (s *Store) LatestSchema(ctx context.Context, snapshot SnapshotID, table TableID) (Schema, error) {
	st, err := s.captureState()
	if err != nil {
		return Schema{}, err
	}
	if st.view.Snapshot(snapshot) == nil {
		return Schema{}, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	versions := st.schemas.versions(snapshot, table)
	if len(versions) == 0 {
		return Schema{}, fmt.Errorf("%w: table %d in snapshot %d", ErrNotFound, table, snapshot)
	}
	latest := versions[len(versions)-1]
	return s.Schema(ctx, snapshot, table, latest)
}

// Tables lists the tables visible at a snapshot.
func (s *Store) Tables(ctx context.Context, snapshot SnapshotID) ([]Table, error) {
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if st.view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tableIDs := st.view.MetadataByType(snapshot, uint32(fileformat.RecordTable))
	// Resolve along parent chain for tables defined in ancestors.
	seen := make(map[TableID]bool)
	out := make([]Table, 0, len(tableIDs))
	for _, oid := range tableIDs {
		tid := TableID(oid)
		seen[tid] = true
	}
	// Include ancestor tables not overridden.
	for _, sm := range st.view.Snapshots() {
		if sm.ID > snapshot {
			continue
		}
		ids := st.view.MetadataByType(sm.ID, uint32(fileformat.RecordTable))
		for _, oid := range ids {
			tid := TableID(oid)
			if seen[tid] {
				continue
			}
			seen[tid] = true
		}
	}
	for tid := range seen {
		rec, err := s.tableRecord(st.view, snapshot, uint64(tid))
		if err != nil {
			continue
		}
		latest := st.schemas.latest(snapshot, tid)
		out = append(out, Table{ID: tid, Name: fieldString(rec, metadata.TableTableName), LatestVersion: latest})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// tableRecord returns the Table metadata record visible at a snapshot.
func (s *Store) tableRecord(view *index.View, snapshot uint64, tableOID uint64) (*metadata.Record, error) {
	// Find the table in the chain.
	cur := snapshot
	for {
		if loc := view.Metadata(cur, tableOID); loc != nil {
			return s.readMetadataRecord(view, cur, tableOID)
		}
		sm := view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			return nil, fmt.Errorf("%w: table object %d", ErrNotFound, tableOID)
		}
		cur = sm.Parent
	}
}
