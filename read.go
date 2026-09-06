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
// parent chain). A DELETE tombstone or an absent row returns ErrNotFound.
func (s *Store) Get(ctx context.Context, snapshot SnapshotID, table TableID, rowID RowID) (Row, error) {
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
	row, _, err := s.readRow(view, st.schemas, loc)
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

// readRow reads and decodes one row from its block, returning the Row and the
// schema version used.
func (s *Store) readRow(view *index.View, si *SchemaIndex, loc *index.RowLoc) (Row, SchemaVersion, error) {
	bl := view.Block(loc.BlockID)
	if bl == nil {
		return nil, 0, fmt.Errorf("rowpack: block %d missing from view", loc.BlockID)
	}
	blk, err := s.reader.ReadAtBlock(int64(bl.DataOffset))
	if err != nil {
		return nil, 0, err
	}
	rp, err := block.ParseRowsPayload(blk.Raw, bl.ItemCount)
	if err != nil {
		return nil, 0, err
	}
	if int(loc.ItemOrdinal) >= len(rp.Entries) {
		return nil, 0, fmt.Errorf("rowpack: row ordinal %d out of range in block %d", loc.ItemOrdinal, loc.BlockID)
	}
	ent := &rp.Entries[loc.ItemOrdinal]
	// Verify the row CRC for local diagnostics.
	if ent.ChangeType != fileformat.ChangeDelete {
		body := rp.RowBytes(int(loc.ItemOrdinal))
		if got := fileformat.CRC32C(body); got != rp.RowCRC(int(loc.ItemOrdinal)) {
			return nil, 0, fmt.Errorf("rowpack: row CRC mismatch in block %d", loc.BlockID)
		}
	}
	schema := si.Schema(bl.SnapshotID, bl.TableID, ent.SchemaVersion)
	if schema == nil {
		// Try resolving from the block's snapshot with the derived index.
		return nil, 0, fmt.Errorf("%w: schema for table %d version %d not found", ErrSchemaMismatch, bl.TableID, ent.SchemaVersion)
	}
	row, err := codec.Decode(rp.RowBytes(int(loc.ItemOrdinal)), schema, s.opts.codecLimits())
	if err != nil {
		return nil, 0, err
	}
	return row, ent.SchemaVersion, nil
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
	schema := st.schemas.Schema(snapshot, table, version)
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
	versions := st.schemas.Versions(snapshot, table)
	if len(versions) == 0 {
		return Schema{}, fmt.Errorf("%w: table %d in snapshot %d", ErrNotFound, table, snapshot)
	}
	latest := versions[len(versions)-1]
	return s.Schema(ctx, snapshot, table, latest)
}

// Tables lists the tables visible at a snapshot.
func (s *Store) Tables(ctx context.Context, snapshot SnapshotID) ([]TableInfo, error) {
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
	out := make([]TableInfo, 0, len(tableIDs))
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
		latest := st.schemas.Latest(snapshot, tid)
		out = append(out, TableInfo{ID: tid, Name: fieldString(rec, metadata.TableTableName), LatestVersion: latest})
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
