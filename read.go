package rowpack

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
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

// ListSnapshots returns committed snapshots sorted by ID.
func (s *Store) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	metas := st.view.Snapshots()
	out := make([]SnapshotInfo, 0, len(metas))
	for _, sm := range metas {
		out = append(out, snapshotInfo(sm))
	}
	return out, nil
}

func snapshotInfo(sm *index.SnapshotMeta) SnapshotInfo {
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
// array and any Decimal big.Int or Bytes buffer already held there. A nil dst
// allocates; keep the returned Row as the next dst to preserve the reuse.
// Getters of String/Bytes/Decimal return copies, so reading through them is
// always safe, but retained Value structs may observe overwritten
// Decimal/Bytes values after the next call. The contents of dst are
// unspecified if an error is returned. A DELETE tombstone or an absent row
// returns ErrNotFound.
func (s *Store) Get(ctx context.Context, snapshot SnapshotID, table string, rowID RowID, dst Row) (Row, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if st.view.Snapshot(uint64(snapshot)) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableID(uint64(snapshot), table)
	if !ok {
		return nil, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	view := st.view
	loc, ok := view.ResolveRow(uint64(snapshot), uint32(tid), uint64(rowID))
	if !ok {
		return nil, fmt.Errorf("%w: (table %d, row %d) in snapshot %d", ErrNotFound, tid, rowID, snapshot)
	}
	if loc.ChangeType == format.ChangeDelete {
		return nil, fmt.Errorf("%w: (table %d, row %d) deleted in snapshot %d", ErrNotFound, tid, rowID, snapshot)
	}
	row, _, err := s.readRowInto(view, st.schemas, snapshot, tid, loc, dst)
	if err != nil {
		return nil, err
	}
	return row, nil
}

// Exists reports whether a row is visible (not deleted) at the snapshot.
// It resolves only the index/tombstone chain and does not read a block.
// An unknown snapshot or unknown table is ErrNotFound, matching Get/Blocks;
// only a known row that is absent (or deleted) reports (false, nil).
func (s *Store) Exists(ctx context.Context, snapshot SnapshotID, table string, rowID RowID) (bool, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return false, err
	}
	if st.view.Snapshot(uint64(snapshot)) == nil {
		return false, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableID(uint64(snapshot), table)
	if !ok {
		return false, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	view := st.view
	loc, ok := view.ResolveRow(uint64(snapshot), uint32(tid), uint64(rowID))
	if !ok || loc.ChangeType == format.ChangeDelete {
		return false, nil
	}
	return true, nil
}

// readRowInto reads and decodes a single row into dst from its block via the
// page container: the block directory locates the page, only that page is
// decompressed, and the record is decoded against its schema version. A
// single-row read no longer decompresses the whole block. Any failure below
// the block layer is classified as a CorruptionError for (snapshot, table).
func (s *Store) readRowInto(view *index.View, si *schemaIndex, snap SnapshotID, table TableID, loc index.RowLoc, dst Row) (Row, SchemaVersion, error) {
	bl := view.Block(loc.BlockID)
	if bl == nil {
		return nil, 0, fmt.Errorf("rowpack: block %d missing from view", loc.BlockID)
	}
	rc, err := s.loader.LoadRows(int64(bl.DataOffset), bl.BlockID)
	if err != nil {
		return nil, 0, err
	}
	rec, release, err := rc.RecordAt(loc.ItemOrdinal)
	if err != nil {
		return nil, 0, s.recordError(bl, snap, table, err)
	}
	defer release()
	if rec.ChangeType == format.ChangeDelete {
		return nil, 0, s.recordError(bl, snap, table, fmt.Errorf("rowpack: row is a tombstone in block %d", bl.BlockID))
	}
	decoder, err := si.decoderFor(bl, rec.SchemaVersion)
	if err != nil {
		return nil, 0, s.recordError(bl, snap, table, err)
	}
	row, err := decoder.DecodeInto(dst, rec.Body, nil)
	if err != nil {
		err = s.recordError(bl, snap, table, err)
	}
	return row, rec.SchemaVersion, err
}

// rowCodec returns the store's row codec: the codec limits derived from opts.
// It is the single place the store turns Options into a codec policy; both the
// write path (EncodeInto) and the read paths (DecodeInto) go through it, so
// limits never travel as a loose argument.
func (s *Store) rowCodec() codec.Codec {
	return codec.Codec{Limits: s.opts.codecLimits()}
}

// Schema returns the schema of a table version at a snapshot. The table is
// addressed by table address, like the other read paths.
func (s *Store) Schema(ctx context.Context, snapshot SnapshotID, table string, version SchemaVersion) (Schema, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return Schema{}, err
	}
	if st.view.Snapshot(snapshot) == nil {
		return Schema{}, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	tid, ok := st.schemas.tableID(snapshot, table)
	if !ok {
		return Schema{}, fmt.Errorf("%w: table %q in snapshot %d", ErrNotFound, table, snapshot)
	}
	schema := st.schemas.schema(snapshot, uint32(tid), version)
	if schema == nil {
		return Schema{}, fmt.Errorf("%w: schema for table %d version %d", ErrSchemaMismatch, tid, version)
	}
	return *schema, nil
}

// Tables lists the tables visible at a snapshot, in every ns.
func (s *Store) Tables(ctx context.Context, snapshot SnapshotID) ([]Table, error) {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	if err != nil {
		return nil, err
	}
	if st.view.Snapshot(snapshot) == nil {
		return nil, fmt.Errorf("%w: snapshot %d", ErrNotFound, snapshot)
	}
	// Gather candidates strictly along the target's parent chain. tableRecord
	// below resolves deletes/overrides on that same chain.
	seen := make(map[TableID]bool)
	for cur := uint64(snapshot); ; {
		ids := st.view.MetadataByType(cur, uint32(format.RecordTable))
		for _, oid := range ids {
			seen[TableID(oid)] = true
		}
		sm := st.view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			break
		}
		cur = sm.Parent
	}
	out := make([]Table, 0, len(seen))
	for tid := range seen {
		rec, err := s.tableRecord(st.view, snapshot, uint64(tid))
		if err != nil {
			continue
		}
		latest := st.schemas.latest(snapshot, tid)
		out = append(out, Table{
			ID:            tid,
			Name:          fieldString(rec, metadata.TableName),
			LatestVersion: latest,
			NS:            st.schemas.nsOf(uint64(snapshot), uint32(tid)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// TablesIn lists the tables visible at a snapshot whose ns equals ns.
// A bare name resolves in NSUser, so this is a filter over Tables, not a second
// addressing dimension.
func (s *Store) TablesIn(ctx context.Context, snapshot SnapshotID, ns string) ([]Table, error) {
	all, err := s.Tables(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	out := make([]Table, 0, len(all))
	for _, t := range all {
		if t.NS == ns {
			out = append(out, t)
		}
	}
	return out, nil
}

// tableRecord returns the Table metadata record visible at a snapshot.
func (s *Store) tableRecord(view *index.View, snapshot uint64, tableOID uint64) (*metadata.Record, error) {
	// Find the table in the chain.
	cur := snapshot
	for {
		if loc := view.Metadata(cur, tableOID); loc != nil {
			return s.readMetadataCached(view, cur, tableOID, nil)
		}
		sm := view.Snapshot(cur)
		if sm == nil || sm.Parent == 0 {
			return nil, fmt.Errorf("%w: table object %d", ErrNotFound, tableOID)
		}
		cur = sm.Parent
	}
}
