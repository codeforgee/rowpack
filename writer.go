package rowpack

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"time"

	"github.com/rowpack/rowpack/internal/fault"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/metadata"
)

// SnapshotType identifies FULL and DELTA snapshots.
type SnapshotType uint8

const (
	SnapshotFull SnapshotType = iota + 1
	SnapshotDelta
)

// SnapshotInfo is the immutable summary of a committed snapshot.
type SnapshotInfo struct {
	ID          SnapshotID
	Type        SnapshotType
	Parent      SnapshotID
	CreatedAt   time.Time
	BlockCount  uint32
	ChangeCount uint64
	RawBytes    uint64
	StoredBytes uint64
}

// ChangeType is the per-record change kind.
type ChangeType uint8

const (
	ChangeInsert ChangeType = iota + 1
	ChangeUpdate
	ChangeDelete
)

// writerState is the Writer lifecycle state.
type writerState uint8

const (
	writerOpen writerState = iota
	writerCommitted
	writerAborted
	writerFailed
)

// Writer writes one snapshot. It is not safe for concurrent use.
type Writer struct {
	store   *Store
	id      SnapshotID
	typ     SnapshotType
	parent  SnapshotID
	created int64

	state writerState

	// tableIDs resolves table names to internal table IDs for this
	// transaction (tables created here and tables resolved from the chain).
	tableIDs map[string]TableID
	// nextTableID is the lowest free internal table ID: one past the highest
	// table ID in the committed view, raised by every CreateTable.
	nextTableID uint32

	// per-table rows block builders
	rowBuilders map[TableID]*block.RowsBlockBuilder
	// metadata block builder
	metaBuilder *block.MetadataBlockBuilder

	// schemas defined in this snapshot: (table, version) -> schema
	schemas map[schemaKey]*codec.Schema

	// encBuf is reused across row encodes to cut per-row allocation.
	encBuf []byte
	// seenRows rejects duplicate (table, row) pairs within the snapshot:
	// one packed rowIDSet per table (see rowset.go; ~11 B/row vs ~90 B/row
	// for a Go map).
	seenRows map[TableID]*rowIDSet

	// pending blocks in flush order (rows and metadata interleaved)
	pending []*pendingBlock
	// metadata records written in this snapshot (for schema index build)
	metaRecords []*metadata.Record

	allocator *metadata.ObjectIDAllocator

	rowRecordCount uint64
	rawBytes       uint64
}

// schemaKey identifies a schema version.
type schemaKey struct {
	Table   TableID
	Version SchemaVersion
}

// pendingBlock is one buffered block awaiting commit-time write.
type pendingBlock struct {
	header  fileformat.BlockHeader
	payload []byte
	offset  int64 // .rpk offset assigned at commit
	// rowsDir is the rows-block directory in record order (rows blocks
	// only, nil otherwise). The block builder transfers ownership of the
	// slice at flush time, so no per-row copy exists on the commit path;
	// RowIndexEntries are built straight into the pre-reserved txn builder
	// at commit.
	rowsDir []fileformat.RowDirectoryEntry
	meta    []fileformat.MetadataIndexEntry
}

// BeginFull starts a new FULL snapshot: a complete baseline that may be
// committed at any time (checkpointing resets the chain depth).
func (s *Store) BeginFull(ctx context.Context) (*Writer, error) {
	return s.newWriter(ctx, SnapshotFull, 0)
}

// BeginDelta starts a DELTA snapshot on top of a committed parent.
func (s *Store) BeginDelta(ctx context.Context, parent SnapshotID) (*Writer, error) {
	if parent == 0 {
		return nil, fmt.Errorf("%w: DELTA snapshot needs a parent", ErrInvalidParent)
	}
	st := s.state.Load()
	if st == nil || st.view.Snapshot(uint64(parent)) == nil {
		return nil, fmt.Errorf("%w: DELTA parent %d not committed", ErrInvalidParent, parent)
	}
	return s.newWriter(ctx, SnapshotDelta, parent)
}

// newWriter constructs the single active writer. A Store allows at most one
// active writer; a concurrent begin returns ErrWriterBusy.
func (s *Store) newWriter(ctx context.Context, typ SnapshotType, parent SnapshotID) (*Writer, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if s.writer.Load() != nil {
		return nil, ErrWriterBusy
	}
	id := s.lastSnapshotID.Add(1)
	// v2 checkpoint semantics (R7): every FULL takes the next global ID — the
	// first snapshot of an empty store is a FULL with id 1 naturally, and a
	// later FULL checkpoint continues the counter (Depth resets to 1 in
	// View.Apply; its visibility no longer follows any ancestor chain).
	w := &Writer{
		store:       s,
		id:          id,
		typ:         typ,
		parent:      parent,
		created:     effectiveNow(),
		state:       writerOpen,
		rowBuilders: make(map[TableID]*block.RowsBlockBuilder),
		schemas:     make(map[schemaKey]*codec.Schema),
		seenRows:    make(map[TableID]*rowIDSet),
		tableIDs:    make(map[string]TableID),
		allocator:   metadata.NewObjectIDAllocator(),
	}
	w.allocator = s.seedAllocator()
	w.nextTableID = s.maxCommittedTableID() + 1
	if !s.writer.CompareAndSwap(nil, w) {
		return nil, ErrWriterBusy
	}
	return w, nil
}

// seedAllocator seeds the writer's object allocator with every object ID
// already present in the committed view, so new IDs never collide.
func (s *Store) seedAllocator() *metadata.ObjectIDAllocator {
	alloc := metadata.NewObjectIDAllocator()
	if st := s.state.Load(); st != nil {
		for _, sm := range st.view.Snapshots() {
			for _, t := range st.view.MetadataByType(sm.ID, uint32(fileformat.RecordTable)) {
				alloc.Force(t, "table")
			}
			for _, c := range st.view.MetadataByType(sm.ID, uint32(fileformat.RecordColumn)) {
				alloc.Force(c, "column")
			}
		}
	}
	return alloc
}

// ID returns the assigned snapshot ID.
func (w *Writer) ID() SnapshotID { return w.id }

// Parent returns the parent snapshot ID (0 for FULL).
func (w *Writer) Parent() SnapshotID { return w.parent }

func (w *Writer) checkState() error {
	switch w.state {
	case writerCommitted:
		return ErrSnapshotCommitted
	case writerAborted:
		return ErrSnapshotAborted
	case writerFailed:
		return ErrSnapshotFailed
	}
	return nil
}

// parentSnapshotFor returns the snapshot this writer extends (for schema
// resolution).
func (w *Writer) parentSnapshotFor() uint64 {
	if w.parent != 0 {
		return w.parent
	}
	if st := w.store.state.Load(); st != nil {
		if latest := st.view.LatestSnapshot(); latest != nil {
			return latest.ID
		}
	}
	return 0
}

// writeMetadata encodes and buffers a metadata record into the metadata block
// builder.
func (w *Writer) writeMetadata(rec *metadata.Record) error {
	body, err := rec.Encode(metadata.CoreFieldSchemas[rec.RecordType])
	if err != nil {
		return err
	}
	w.metaRecords = append(w.metaRecords, rec)
	return w.metaBuilderAdd(rec, body)
}

func (w *Writer) ensureMetaBuilder() *block.MetadataBlockBuilder {
	if w.metaBuilder == nil {
		w.metaBuilder = block.NewMetadataBlockBuilder(
			w.id, 0, w.store.opts.BlockSize, w.store.opts.diskCompression(), w.store.opts.CompressionLevel,
			block.Limits{
				MaxRawBytes:    w.store.opts.Limits.MaxRawBlockBytes,
				MaxStoredBytes: w.store.opts.Limits.MaxStoredBlockBytes,
			}, w.metaFlush)
		w.attachZstdEncoder(w.metaBuilder)
	}
	return w.metaBuilder
}

func (w *Writer) metaBuilderAdd(rec *metadata.Record, body []byte) error {
	entry := metadata.DirectoryEntry{
		ObjectID:   rec.ObjectID,
		Revision:   rec.Revision,
		RecordType: rec.RecordType,
		Operation:  fileformat.OperationUpsert,
		Critical:   rec.Critical,
	}
	return w.ensureMetaBuilder().Add(entry, body)
}

// metaFlush captures one completed metadata block. The builder supplies the
// directory entries directly, so no payload re-parse is needed.
func (w *Writer) metaFlush(fb *block.FlushedBlock) error {
	blk := &pendingBlock{header: fb.Header, payload: fb.Stored}
	blk.meta = make([]fileformat.MetadataIndexEntry, 0, len(fb.Meta))
	for i := range fb.Meta {
		blk.meta = append(blk.meta, fileformat.MetadataIndexEntry{
			SnapshotID:  w.id,
			ObjectID:    fb.Meta[i].ObjectID,
			Revision:    fb.Meta[i].Revision,
			RecordType:  fb.Meta[i].RecordType,
			ItemOrdinal: uint32(i),
			Operation:   fb.Meta[i].Operation,
			Critical:    fb.Meta[i].Critical,
		})
	}
	w.pending = append(w.pending, blk)
	return nil
}

// rowsFlush captures one completed rows block. The builder hands over
// ownership of its directory slice (it allocates a fresh one for its next
// block), so the pending block references it without a per-row copy;
// ItemOrdinal is the directory position.
func (w *Writer) rowsFlush(table TableID) func(*block.FlushedBlock) error {
	return func(fb *block.FlushedBlock) error {
		blk := &pendingBlock{header: fb.Header, payload: fb.Stored, rowsDir: fb.Rows}
		w.pending = append(w.pending, blk)
		return nil
	}
}

// rowBuilder returns (creating if needed) the rows builder for a table.
func (w *Writer) rowBuilder(table TableID) *block.RowsBlockBuilder {
	if b := w.rowBuilders[table]; b != nil {
		return b
	}
	b := block.NewRowsBlockBuilder(w.id, table, w.store.opts.BlockSize, w.store.opts.diskCompression(), w.store.opts.CompressionLevel, block.Limits{
		MaxRawBytes:    w.store.opts.Limits.MaxRawBlockBytes,
		MaxStoredBytes: w.store.opts.Limits.MaxStoredBlockBytes,
	}, w.rowsFlush(table))
	w.attachZstdEncoder(b)
	w.rowBuilders[table] = b
	return b
}

// attachZstdEncoder hands the store's persistent zstd encoder to a block
// builder when the disk compression is zstd. The store owns the encoder (one
// writer at a time, sequential flushes), so its ~1 MiB histogram is allocated
// once per store instead of once per pool-recreating GC cycle.
func (w *Writer) attachZstdEncoder(setter interface{ SetZstdEncoder(*block.ZstdEncoder) }) {
	if w.store.opts.diskCompression() != fileformat.CompressionZstd {
		return
	}
	setter.SetZstdEncoder(w.store.zstdEncoder())
}

// Insert appends an INSERT change.
// CreateTable creates a table in this snapshot. The table name is the
// caller-facing identity; the internal table ID and schema version are
// assigned by the engine and never surface in the API.
//
//   - same name + same columns within this transaction ⇒ idempotent no-op;
//   - same name + different columns ⇒ ErrSchemaConflict;
//   - a table of the same name on the parent chain resolves to the existing
//     internal ID: same columns ⇒ no-op, different columns ⇒ ErrSchemaConflict.
func (w *Writer) CreateTable(name string, columns []Column) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("%w: table name is empty", ErrInvalidArgument)
	}
	// Idempotence / conflict against this transaction's own definitions.
	if tid, ok := w.tableIDs[name]; ok {
		if w.columnsEqual(tid, columns) {
			return nil
		}
		return fmt.Errorf("%w: table %q already defined with different columns", ErrSchemaConflict, name)
	}
	// Resolve against the committed parent chain.
	st := w.store.state.Load()
	if st != nil {
		if chainTID, ok := st.schemas.tableIDByName(uint64(w.parentSnapshotFor()), name); ok {
			if !w.chainColumnsEqual(uint64(w.parentSnapshotFor()), chainTID, columns) {
				return fmt.Errorf("%w: table %q already exists with different columns", ErrSchemaConflict, name)
			}
			if w.typ == SnapshotDelta {
				// DELTA: readers resolve the schema along the parent chain, so
				// an identical re-definition needs no metadata of its own.
				w.tableIDs[name] = chainTID
				return nil
			}
			// FULL checkpoint (parent 0): always write the snapshot's own
			// metadata layer, even when identical to the ancestor's (R7) —
			// its visibility no longer follows any ancestor chain.
			latest := st.schemas.latest(uint64(w.parentSnapshotFor()), uint32(chainTID))
			if latest == 0 {
				latest = 1
			}
			if err := w.writeTableRecords(uint32(chainTID), latest, name, columns); err != nil {
				return err
			}
			w.tableIDs[name] = chainTID
			return nil
		}
	}
	// New table: allocate the internal ID and define version 1.
	tid := w.nextTableID
	if tid == 0 || tid == ^uint32(0) {
		return fmt.Errorf("%w: table id space exhausted", ErrInvalidArgument)
	}
	if err := w.writeTableRecords(tid, 1, name, columns); err != nil {
		return err
	}
	w.nextTableID++
	w.tableIDs[name] = tid
	return nil
}

// writeTableRecords writes the Table + Column metadata records of one table
// version and records the resolved schema in the writer's txn map.
func (w *Writer) writeTableRecords(tid uint32, version uint32, name string, columns []Column) error {
	schema := codec.Schema{TableID: tid, Version: version, Name: name, Columns: columns}
	if err := schema.Validate(w.store.opts.codecLimits()); err != nil {
		return err
	}
	tableOID := uint64(tid)
	tableRec := &metadata.Record{
		RecordType:  uint32(fileformat.RecordTable),
		ObjectID:    tableOID,
		Revision:    schema.Version,
		Namespace:   fileformat.NamespaceCore,
		ExternalKey: schema.Name,
		Fields: []metadata.Field{
			{ID: metadata.TableTableName, WireType: fileformat.WireString, Value: schema.Name},
		},
	}
	if err := w.writeMetadata(tableRec); err != nil {
		return err
	}
	for i, col := range schema.Columns {
		colRec := &metadata.Record{
			RecordType: uint32(fileformat.RecordColumn),
			ObjectID:   w.allocator.Alloc(fileformat.NamespaceCore, fmt.Sprintf("%s:%d:%s", schema.Name, schema.Version, col.Name)),
			ParentID:   tableOID,
			Revision:   1,
			Namespace:  fileformat.NamespaceCore,
			Fields: []metadata.Field{
				{ID: metadata.ColColumnID, WireType: fileformat.WireSint, Value: int64(i + 1)},
				{ID: metadata.ColColumnName, WireType: fileformat.WireString, Value: col.Name},
				{ID: metadata.ColColumnType, WireType: fileformat.WireString, Value: typeName(col.Type)},
				{ID: metadata.ColNullable, WireType: fileformat.WireString, Value: nullString(col.Nullable)},
				{ID: metadata.ColDataScale, WireType: fileformat.WireSint, Value: int64(col.Scale)},
			},
		}
		if err := w.writeMetadata(colRec); err != nil {
			return err
		}
	}
	w.schemas[schemaKey{Table: tid, Version: schema.Version}] = schema.Clone()
	return nil
}

// columnsEqual compares the columns of the latest schema version defined for
// tid within this transaction against columns.
func (w *Writer) columnsEqual(tid TableID, columns []Column) bool {
	var latest uint32
	for key := range w.schemas {
		if key.Table == tid && key.Version > latest {
			latest = key.Version
		}
	}
	if latest == 0 {
		return false
	}
	s := w.schemas[schemaKey{Table: tid, Version: latest}]
	return schemaColumnsEqual(s, columns)
}

// chainColumnsEqual compares the latest committed schema of (snapshot, tid)
// against columns.
func (w *Writer) chainColumnsEqual(snapshot uint64, tid TableID, columns []Column) bool {
	st := w.store.state.Load()
	if st == nil {
		return false
	}
	latest := st.schemas.latest(snapshot, uint32(tid))
	if latest == 0 {
		return false
	}
	return schemaColumnsEqual(st.schemas.schema(snapshot, uint32(tid), latest), columns)
}

func schemaColumnsEqual(s *codec.Schema, columns []Column) bool {
	if s == nil || len(s.Columns) != len(columns) {
		return false
	}
	for i := range columns {
		if s.Columns[i] != columns[i] {
			return false
		}
	}
	return true
}

// maxCommittedTableID returns the highest internal table ID in the committed
// view (0 when the store is empty).
func (s *Store) maxCommittedTableID() uint32 {
	st := s.state.Load()
	if st == nil {
		return 0
	}
	var maxID uint32
	for _, sm := range st.view.Snapshots() {
		for _, oid := range st.view.MetadataByType(sm.ID, uint32(fileformat.RecordTable)) {
			if tid, err := metadata.TableID(oid); err == nil && tid > maxID {
				maxID = tid
			}
		}
	}
	return maxID
}

// resolveTableForWrite resolves a table name to (internal ID, schema
// version) for a write: tables created in this transaction first, then the
// committed parent chain; the schema version is the table's latest — the
// newest version defined in this transaction, or the committed latest when
// the table was defined by an ancestor snapshot.
func (w *Writer) resolveTableForWrite(table string) (TableID, SchemaVersion, error) {
	tid, cached := w.tableIDs[table]
	if !cached {
		st := w.store.state.Load()
		if st == nil {
			return 0, 0, fmt.Errorf("%w: table %q", ErrNotFound, table)
		}
		var ok bool
		if tid, ok = st.schemas.tableIDByName(uint64(w.parentSnapshotFor()), table); !ok {
			return 0, 0, fmt.Errorf("%w: table %q", ErrNotFound, table)
		}
		w.tableIDs[table] = tid // cache for subsequent writes
	}
	ver := w.latestTxnVersion(tid)
	if ver == 0 {
		if st := w.store.state.Load(); st != nil {
			ver = SchemaVersion(st.schemas.latest(uint64(w.parentSnapshotFor()), uint32(tid)))
		}
	}
	return tid, ver, nil
}

func (w *Writer) latestTxnVersion(tid TableID) SchemaVersion {
	var latest SchemaVersion
	for key := range w.schemas {
		if key.Table == tid && key.Version > latest {
			latest = key.Version
		}
	}
	return latest
}

// Insert appends an INSERT change. The table is addressed by name; the row
// is encoded against the table's latest schema.
func (w *Writer) Insert(ctx context.Context, table string, rowID RowID, row Row) error {
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, ver, err := w.resolveTableForWrite(table)
	if err != nil {
		return err
	}
	return w.put(ctx, ChangeInsert, tid, rowID, ver, row)
}

// Update appends an UPDATE change (DELTA snapshots only).
func (w *Writer) Update(ctx context.Context, table string, rowID RowID, row Row) error {
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, ver, err := w.resolveTableForWrite(table)
	if err != nil {
		return err
	}
	return w.put(ctx, ChangeUpdate, tid, rowID, ver, row)
}

// Delete appends a DELETE tombstone (DELTA snapshots only).
func (w *Writer) Delete(ctx context.Context, table string, rowID RowID) error {
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, _, err := w.resolveTableForWrite(table)
	if err != nil {
		return err
	}
	return w.put(ctx, ChangeDelete, tid, rowID, 0, nil)
}

func (w *Writer) put(ctx context.Context, typ ChangeType, table TableID, rowID RowID, schemaVersion SchemaVersion, row Row) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	if w.rowSeen(table, rowID) {
		return fmt.Errorf("%w: duplicate (table %d, row %d) in snapshot %d", ErrAlreadyExists, table, rowID, w.id)
	}
	if w.typ == SnapshotFull && typ != ChangeInsert {
		return fmt.Errorf("%w: FULL snapshot only allows INSERT", ErrInvalidArgument)
	}
	var encoded []byte
	if typ == ChangeDelete {
		// Tombstone: no row payload.
	} else {
		schema, err := w.resolveSchema(table, schemaVersion)
		if err != nil {
			return err
		}
		w.encBuf, err = codec.EncodeInto(schema, row, w.store.opts.codecLimits(), w.encBuf)
		if err != nil {
			return err
		}
		encoded = w.encBuf
		if err := w.checkStrictParent(table, rowID, typ); err != nil {
			return err
		}
	}
	if err := w.rowBuilder(table).Add(rowID, schemaVersion, fileformat.ChangeType(typ), encoded); err != nil {
		return err
	}
	w.rememberRow(table, rowID)
	w.rowRecordCount++
	w.rawBytes += uint64(fileformat.RowRecordHeaderSize + len(encoded))
	return nil
}

// rowSeen reports whether (table, rowID) was already written to this
// snapshot.
func (w *Writer) rowSeen(table TableID, rowID RowID) bool {
	return w.seenRows[table].Contains(rowID)
}

// rememberRow records (table, rowID) as written. put() calls it only after
// the row builder accepted the record, so failed inserts never pollute the
// set. Nil sets are created lazily: most writers touch few tables.
func (w *Writer) rememberRow(table TableID, rowID RowID) {
	set := w.seenRows[table]
	if set == nil {
		set = &rowIDSet{}
		w.seenRows[table] = set
	}
	set.Insert(rowID)
}

func (w *Writer) resolveSchema(table TableID, version SchemaVersion) (*codec.Schema, error) {
	if schema, ok := w.schemas[schemaKey{Table: table, Version: version}]; ok {
		return schema, nil
	}
	st := w.store.state.Load()
	if st != nil {
		if schema := st.schemas.schema(w.parentSnapshotFor(), table, version); schema != nil {
			return schema, nil
		}
	}
	return nil, fmt.Errorf("%w: schema for table %d version %d not defined", ErrSchemaMismatch, table, version)
}

// checkStrictParent enforces parent-view existence for DELTA changes.
func (w *Writer) checkStrictParent(table TableID, rowID RowID, typ ChangeType) error {
	if w.typ != SnapshotDelta || w.store.opts.Validation == ValidationNone {
		return nil
	}
	st := w.store.state.Load()
	if st == nil {
		return nil
	}
	// Parent-view existence is resolved along the whole parent chain, not just
	// the immediate parent layer.
	loc := st.view.ResolveRow(w.parent, table, rowID)
	exists := loc != nil && loc.ChangeType != fileformat.ChangeDelete
	switch typ {
	case ChangeInsert:
		if exists {
			return fmt.Errorf("%w: row (table %d, row %d) already exists in parent", ErrAlreadyExists, table, rowID)
		}
	case ChangeUpdate, ChangeDelete:
		if !exists {
			return fmt.Errorf("%w: row (table %d, row %d) missing in parent", ErrNotFound, table, rowID)
		}
	}
	return nil
}

// Abort discards the snapshot. It is idempotent; Abort on a committed writer
// returns ErrSnapshotCommitted.
func (w *Writer) Abort() error {
	switch w.state {
	case writerCommitted:
		return ErrSnapshotCommitted
	case writerAborted:
		return nil
	}
	w.state = writerAborted
	w.store.writer.CompareAndSwap(w, nil)
	return nil
}

// abort is the internal close path (no committed check).
func (w *Writer) abort() error {
	if w.state == writerCommitted {
		return ErrSnapshotCommitted
	}
	w.state = writerAborted
	w.store.writer.CompareAndSwap(w, nil)
	return nil
}

// Commit persists the snapshot, atomically publishes it to readers and
// returns the new snapshot's ID (the read path's only credential).
func (w *Writer) Commit(ctx context.Context) (SnapshotID, error) {
	if err := w.checkState(); err != nil {
		return 0, err
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
	}
	w.store.writeMu.Lock()
	defer w.store.writeMu.Unlock()
	if err := w.store.checkOpen(); err != nil {
		w.state = writerFailed
		return 0, err
	}
	info, commitErr := w.commitLocked(ctx)
	if commitErr != nil {
		w.state = writerFailed
	}
	return info.ID, commitErr
}

func (w *Writer) commitLocked(ctx context.Context) (SnapshotInfo, error) {
	if err := w.flushAll(); err != nil {
		return SnapshotInfo{}, err
	}
	if len(w.pending) == 0 && w.typ == SnapshotFull {
		return SnapshotInfo{}, fmt.Errorf("%w: empty FULL snapshot", ErrInvalidArgument)
	}
	// Fault injection points (test-only): crash between these positions.
	fault.Check("commit.header.before")

	// Assign block IDs first so the snapshot header can record FirstBlockID.
	for _, blk := range w.pending {
		blk.header.BlockID = w.store.lastBlockID.Add(1)
	}
	// Single-file commit order: SnapshotHeader -> Blocks -> IndexTxn ->
	// SnapshotFooter, then exactly one Sync (BINARY_FORMAT_V2 §8).
	startOffset := w.store.data.Offset()
	snapStart := startOffset
	var sh fileformat.SnapshotHeader
	sh.SnapshotType = fileformat.SnapshotType(w.typ)
	sh.SnapshotID = w.id
	sh.ParentSnapshotID = w.parent
	sh.CreatedUnixNano = w.created
	sh.WriterNonce = effectiveWriterNonce()
	if len(w.pending) > 0 {
		sh.FirstBlockID = w.pending[0].header.BlockID
	}
	var shBuf [fileformat.SnapshotHeaderSize]byte
	if err := sh.MarshalTo(shBuf[:]); err != nil {
		return SnapshotInfo{}, err
	}
	if _, err := w.store.data.Append(shBuf[:]); err != nil {
		return SnapshotInfo{}, err
	}

	fault.Check("commit.block.before")
	var blockCount, metaBlockCount uint32
	var rawBytes uint64
	var blockCRCs []byte
	for _, blk := range w.pending {
		// Encrypt the stored payload after BlockID assignment and before the
		// header is marshalled: the AAD binds the final header fields. The
		// ciphertext length is known up front (plaintext + tag), and the AAD's
		// StoredSize is set to that exact value so read-time verification is
		// self-consistent. Only the payload and header change; RawSize and
		// RawCRC32C keep describing the uncompressed plaintext.
		if c := w.store.encCipher; c != nil {
			blk.header.Encrypted = true
			blk.header.KeyEpoch = 0
			blk.header.StoredSize = uint32(len(blk.payload)) + fileformat.AESGCMTagLen
			sealed, err := c.Seal(0, blk.header.BlockID, &w.store.uuid, &blk.header, blk.payload)
			if err != nil {
				return SnapshotInfo{}, err
			}
			blk.payload = sealed
		}
		var hb [fileformat.BlockHeaderSize]byte
		if err := blk.header.MarshalTo(hb[:]); err != nil {
			return SnapshotInfo{}, err
		}
		off, err := w.store.data.Append(hb[:])
		if err != nil {
			return SnapshotInfo{}, err
		}
		blk.offset = off
		if _, err := w.store.data.Append(blk.payload); err != nil {
			return SnapshotInfo{}, err
		}
		blockCount++
		if blk.header.BlockKind == fileformat.BlockKindMetadata {
			metaBlockCount++
		}
		rawBytes += uint64(blk.header.RawSize)
		blockCRCs = append(blockCRCs, hb[52:56]...)
	}
	// Note: block CRCs are computed from the header CRC fields (offset 52).

	blocksEnd := w.store.data.Offset()

	// The IndexTxn is built as compressed chunks: its stored length depends
	// on compression results, so the txn/footer offsets are resolved through
	// a callback once the body length is known (the snapshot chunk's stored
	// size is fixed at 72B, so one pass suffices). The footer binds the txn
	// by exact byte extent plus CRC over the stored bytes.
	txnBuilder := index.NewBuilder(w.store.txnSeq.Add(1))
	// Entry totals are known from the flushed blocks: pre-reserving removes
	// the slice-growth copies from the commit peak. Row dedup is skipped on
	// this path: put() already rejects duplicate (table, row) pairs via the
	// writer's packed seen-row set, and View.Apply re-validates the built
	// shards, so the builder's ~100 B/row dedup map is pure overhead here.
	totalMeta, totalRows := 0, 0
	for _, blk := range w.pending {
		totalMeta += len(blk.meta)
		totalRows += len(blk.rowsDir)
	}
	txnBuilder.SetRowDedup(false)
	txnBuilder.Reserve(totalMeta, len(w.pending), totalRows)
	snapEntry := fileformat.SnapshotIndexEntry{
		SnapshotID:       w.id,
		ParentSnapshotID: w.parent,
		SnapshotType:     fileformat.SnapshotType(w.typ),
		BlockCount:       blockCount,
		RowRecordCount:   w.rowRecordCount,
		DataStart:        uint64(snapStart),
		DataEnd:          uint64(snapStart), // resolved by BuildStored
		CreatedUnixNano:  w.created,
	}
	if err := txnBuilder.SetSnapshot(snapEntry); err != nil {
		return SnapshotInfo{}, err
	}
	for _, blk := range w.pending {
		if err := txnBuilder.AddBlock(fileformat.BlockIndexEntry{
			BlockID:     blk.header.BlockID,
			SnapshotID:  blk.header.SnapshotID,
			TableID:     blk.header.TableID,
			BlockKind:   blk.header.BlockKind,
			Compression: blk.header.Compression,
			DataOffset:  uint64(blk.offset),
			RawSize:     blk.header.RawSize,
			StoredSize:  blk.header.StoredSize,
			ItemCount:   blk.header.ItemCount,
			RawCRC32C:   blk.header.RawCRC32C,
		}); err != nil {
			return SnapshotInfo{}, err
		}
		for i := range blk.meta {
			blk.meta[i].BlockID = blk.header.BlockID
			if err := txnBuilder.AddMetadata(blk.meta[i]); err != nil {
				return SnapshotInfo{}, err
			}
		}
		for i := range blk.rowsDir {
			de := &blk.rowsDir[i]
			if err := txnBuilder.AddRow(fileformat.RowIndexEntry{
				SnapshotID:  w.id,
				TableID:     blk.header.TableID,
				ChangeType:  de.ChangeType,
				RowID:       de.RowID,
				BlockID:     blk.header.BlockID,
				ItemOrdinal: uint32(i),
			}); err != nil {
				return SnapshotInfo{}, err
			}
		}
	}
	fault.Check("commit.txn.before")
	// After the single sync below, failures are "outcome unknown"; before it,
	// a failure is a known torn commit.
	unknown := false
	// Index-domain chunk sealing (R11): each chunk is sealed under its own
	// HMAC-derived nonce (the NonceIndex 96-bit space is full) and AAD bound
	// to store/txn/chunk identity and lengths. Header, chunk headers and the
	// directory stay plaintext — the scanner walks the txn by magic +
	// BodyBytes + footer magic without a key (R1) — while every payload is
	// authenticated independently.
	var crypto *index.ChunkCrypto
	if c := w.store.encCipher; c != nil {
		const epoch = uint32(0)
		txnSeq := w.store.txnSeq.Load()
		uuid := &w.store.uuid
		crypto = &index.ChunkCrypto{
			TxnSequence: txnSeq,
			SnapshotID:  uint64(w.id),
			Epoch:       epoch,
			Seal: func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error) {
				// AAD binds the FINAL stored length (compressed + GCM tag);
				// the read side derives it from the chunk header.
				storedBytes := uint32(len(stored)) + fileformat.AESGCMTagLen
				return c.SealIndexChunk(uuid, txnSeq, uint64(w.id), chunkSeq, firstOrdinal, uint32(rawBytes), storedBytes, kind, epoch, stored)
			},
		}
	}
	var txnStartResolved, txnEndResolved, snapEndResolved int64
	stored, txn, err := txnBuilder.BuildStored(crypto, w.store.opts.CompressionLevel,
		func(bodyLen int) (uint64, uint64, int64, int64) {
			l := int64(fileformat.IndexTxnHeaderSize + bodyLen + fileformat.IndexTxnFooterSize)
			ts := blocksEnd
			te := ts + l
			txnStartResolved, txnEndResolved, snapEndResolved = ts, te, te+fileformat.SnapshotFooterSize
			return uint64(snapStart), uint64(snapEndResolved), ts, te
		}, 0, 0)
	if err != nil {
		return SnapshotInfo{}, err
	}
	txnStart, txnEnd, snapEnd := txnStartResolved, txnEndResolved, snapEndResolved
	if _, err := w.store.data.Append(stored); err != nil {
		return SnapshotInfo{}, err
	}

	// Footer: the commit authority; also binds the IndexTxn bytes.
	var ftr fileformat.SnapshotFooter
	ftr.SnapshotType = fileformat.SnapshotType(w.typ)
	ftr.SnapshotID = w.id
	ftr.ParentSnapshotID = w.parent
	ftr.PreviousFooterOffset = w.store.lastFooterOffset
	ftr.SnapshotStartOffset = uint64(snapStart)
	ftr.BlocksStartOffset = uint64(snapStart) + fileformat.SnapshotHeaderSize
	ftr.BlocksEndOffset = uint64(blocksEnd)
	ftr.IndexTxnStartOffset = uint64(txnStart)
	ftr.IndexTxnEndOffset = uint64(txnEnd)
	ftr.SnapshotEndOffset = uint64(snapEnd)
	ftr.FirstBlockID = sh.FirstBlockID
	ftr.BlockCount = blockCount
	ftr.MetadataBlockCount = metaBlockCount
	ftr.RowRecordCount = w.rowRecordCount
	ftr.RawBytes = rawBytes
	ftr.StoredBytes = uint64(snapEnd - snapStart)
	ftr.BlocksCRC32C = fileformat.CRC32C(blockCRCs)
	ftr.IndexTxnCRC32C = fileformat.CRC32C(stored)
	var fb [fileformat.SnapshotFooterSize]byte
	if err := ftr.MarshalTo(fb[:]); err != nil {
		return SnapshotInfo{}, err
	}
	if _, err := w.store.data.Append(fb[:]); err != nil {
		return SnapshotInfo{}, err
	}
	fault.Check("commit.footer.after")
	if w.store.data.Offset() != snapEnd {
		return SnapshotInfo{}, fmt.Errorf("rowpack: snapshot end %d != %d", w.store.data.Offset(), snapEnd)
	}

	// Durability: one sync for the whole transaction.
	fault.Check("commit.sync.before")
	if w.store.opts.Durability == SyncCommit {
		if err := w.store.data.Sync(); err != nil {
			return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: true, Err: err}
		}
	}
	fault.Check("commit.sync.after")
	// After the single sync, failures are "outcome unknown".
	unknown = true

	st := w.store.state.Load()
	newView, err := st.view.Apply(txn, w.store.opts.Limits.MaxSnapshotDepth)
	if err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
	}
	newSchemas, err := w.buildNewSchemas(newView)
	if err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
	}
	fault.Check("commit.publish.before")
	w.store.state.Store(&publishedState{view: newView, schemas: newSchemas})
	w.store.lastFooterOffset = uint64(txnEnd)
	fault.Check("commit.publish.after")
	w.state = writerCommitted
	w.store.writer.CompareAndSwap(w, nil)

	return SnapshotInfo{
		ID:          w.id,
		Type:        w.typ,
		Parent:      w.parent,
		CreatedAt:   time.Unix(0, w.created).UTC(),
		BlockCount:  blockCount,
		ChangeCount: w.rowRecordCount,
		RawBytes:    rawBytes,
		StoredBytes: uint64(snapEnd - snapStart),
	}, nil
}

// buildNewSchemas derives the schema index for the new snapshot only (the
// parent snapshots' schemas are reused from the old index).
func (w *Writer) buildNewSchemas(newView *index.View) (*schemaIndex, error) {
	base := w.store.state.Load()
	si := &schemaIndex{bySnapshot: make(map[uint64]map[uint32]*tableSchemas)}
	if base != nil && base.schemas != nil {
		for snap, tables := range base.schemas.bySnapshot {
			si.bySnapshot[snap] = tables
		}
	}
	tables, err := w.store.deriveTables(newView, w.id, nil)
	if err != nil {
		return nil, err
	}
	if len(tables) > 0 {
		si.bySnapshot[w.id] = tables
	}
	return si, nil
}

// flushAll flushes every builder into pending blocks.
func (w *Writer) flushAll() error {
	if w.metaBuilder != nil {
		if err := w.metaBuilder.Flush(); err != nil {
			return err
		}
	}
	// Deterministic table order for flushing.
	tables := make([]TableID, 0, len(w.rowBuilders))
	for t := range w.rowBuilders {
		tables = append(tables, t)
	}
	sortTableIDs(tables)
	for _, t := range tables {
		if err := w.rowBuilders[t].Flush(); err != nil {
			return err
		}
	}
	return nil
}

func randUint64() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return le64(b[:])
}

func le64(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func sortTableIDs(ids []TableID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}
