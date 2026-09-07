package rowpack

import (
	"context"
	"crypto/rand"
	"errors"
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

// SnapshotOptions configures a new snapshot.
type SnapshotOptions struct {
	Parent     SnapshotID
	AllowEmpty bool
	CreatedAt  time.Time // zero value uses current UTC time
}

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

// Change is one row change to apply.
type Change struct {
	Type          ChangeType
	TableID       TableID
	RowID         RowID
	SchemaVersion SchemaVersion
	Row           Row
}

// writerState is the SnapshotWriter lifecycle state.
type writerState uint8

const (
	writerOpen writerState = iota
	writerCommitted
	writerAborted
	writerFailed
)

// SnapshotWriter writes one snapshot. It is not safe for concurrent use.
type SnapshotWriter struct {
	store      *Store
	id         SnapshotID
	typ        SnapshotType
	parent     SnapshotID
	created    int64
	allowEmpty bool

	state writerState

	// per-table rows block builders
	rowBuilders map[TableID]*block.RowsBlockBuilder
	// metadata block builder
	metaBuilder *block.MetadataBlockBuilder

	// schemas defined in this snapshot: (table, version) -> schema
	schemas map[schemaKey]*codec.Schema

	// encBuf is reused across row encodes to cut per-row allocation.
	encBuf []byte
	// seen row keys to reject duplicates
	seenRows map[rowKey]struct{}

	// pending blocks in flush order (rows and metadata interleaved)
	pending []*pendingBlock
	// metadata records written in this snapshot (for schema index build)
	metaRecords []*metadata.Record

	nextMetaBlockSeq int
	allocator        *metadata.ObjectIDAllocator

	rowRecordCount uint64
	rawBytes       uint64
}

// schemaKey identifies a schema version.
type schemaKey struct {
	Table   TableID
	Version SchemaVersion
}

// rowKey identifies a row within a table.
type rowKey struct {
	Table TableID
	Row   RowID
}

// pendingBlock is one buffered block awaiting commit-time write.
type pendingBlock struct {
	header  fileformat.BlockHeader
	payload []byte
	offset  int64 // .rpk offset assigned at commit
	rows    []fileformat.RowIndexEntry
	meta    []fileformat.MetadataIndexEntry
}

// BeginSnapshot starts a new snapshot. A Store allows at most one active
// writer; a concurrent Begin returns ErrWriterBusy.
func (s *Store) BeginSnapshot(ctx context.Context, typ SnapshotType, opts SnapshotOptions) (*SnapshotWriter, error) {
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if s.writer.Load() != nil {
		return nil, ErrWriterBusy
	}
	if typ != SnapshotFull && typ != SnapshotDelta {
		return nil, fmt.Errorf("%w: snapshot type %d", ErrInvalidArgument, typ)
	}
	// Validate parent for the snapshot type.
	if typ == SnapshotFull && opts.Parent != 0 {
		return nil, fmt.Errorf("%w: FULL snapshot has parent %d", ErrInvalidParent, opts.Parent)
	}
	if typ == SnapshotDelta {
		st := s.state.Load()
		if st == nil || st.view.Snapshot(opts.Parent) == nil {
			return nil, fmt.Errorf("%w: DELTA parent %d not committed", ErrInvalidParent, opts.Parent)
		}
		if opts.Parent == 0 {
			return nil, fmt.Errorf("%w: DELTA snapshot needs a parent", ErrInvalidParent)
		}
	}
	id := s.lastSnapshotID.Add(1)
	if typ == SnapshotFull {
		id = 1 // first snapshot is FULL and its ID is 1
		s.lastSnapshotID.Store(1)
	}
	created := opts.CreatedAt
	if created.IsZero() {
		created = time.Unix(0, effectiveNow()).UTC()
	}
	w := &SnapshotWriter{
		store:       s,
		id:          id,
		typ:         typ,
		parent:      opts.Parent,
		created:     created.UnixNano(),
		allowEmpty:  opts.AllowEmpty,
		state:       writerOpen,
		rowBuilders: make(map[TableID]*block.RowsBlockBuilder),
		schemas:     make(map[schemaKey]*codec.Schema),
		seenRows:    make(map[rowKey]struct{}),
		allocator:   metadata.NewObjectIDAllocator(),
	}
	w.allocator = s.seedAllocator()
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
func (w *SnapshotWriter) ID() SnapshotID { return w.id }

// Parent returns the parent snapshot ID (0 for FULL).
func (w *SnapshotWriter) Parent() SnapshotID { return w.parent }

func (w *SnapshotWriter) checkState() error {
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

// DefineSchema records a table schema version in this snapshot. Re-defining
// an identical (table, version) is idempotent; a conflicting one fails with
// ErrSchemaConflict; a version not greater than the table's latest fails with
// ErrSchemaConflict.
func (w *SnapshotWriter) DefineSchema(schema codec.Schema) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if err := schema.Validate(w.store.opts.codecLimits()); err != nil {
		return err
	}
	key := schemaKey{Table: schema.TableID, Version: schema.Version}
	if existing, ok := w.schemas[key]; ok {
		if !schemaEqual(existing, &schema) {
			return fmt.Errorf("%w: schema %d v%d differs from existing definition", ErrSchemaConflict, schema.TableID, schema.Version)
		}
		return nil
	}
	// Check parent chain monotonicity.
	st := w.store.state.Load()
	if st != nil {
		if existing := st.schemas.schema(w.parentSnapshotFor(), schema.TableID, schema.Version); existing != nil {
			if !schemaEqual(existing, &schema) {
				return fmt.Errorf("%w: schema %d v%d conflicts with committed definition", ErrSchemaConflict, schema.TableID, schema.Version)
			}
			return nil
		}
		latest := st.schemas.latest(w.parentSnapshotFor(), schema.TableID)
		if schema.Version <= latest {
			return fmt.Errorf("%w: schema %d version %d not greater than latest %d", ErrSchemaConflict, schema.TableID, schema.Version, latest)
		}
	}

	// Build metadata records.
	tableOID := uint64(schema.TableID)
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
	w.schemas[key] = schema.Clone()
	return nil
}

// parentSnapshotFor returns the snapshot this writer extends (for schema
// resolution).
func (w *SnapshotWriter) parentSnapshotFor() uint64 {
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
func (w *SnapshotWriter) writeMetadata(rec *metadata.Record) error {
	body, err := rec.Encode(metadata.CoreFieldSchemas[rec.RecordType])
	if err != nil {
		return err
	}
	w.metaRecords = append(w.metaRecords, rec)
	return w.metaBuilderAdd(rec, body)
}

func (w *SnapshotWriter) ensureMetaBuilder() *block.MetadataBlockBuilder {
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

func (w *SnapshotWriter) metaBuilderAdd(rec *metadata.Record, body []byte) error {
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
func (w *SnapshotWriter) metaFlush(fb *block.FlushedBlock) error {
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

// rowsFlush captures one completed rows block and its row index entries.
func (w *SnapshotWriter) rowsFlush(table TableID) func(*block.FlushedBlock) error {
	return func(fb *block.FlushedBlock) error {
		blk := &pendingBlock{header: fb.Header, payload: fb.Stored}
		blk.rows = make([]fileformat.RowIndexEntry, 0, len(fb.Rows))
		for i := range fb.Rows {
			blk.rows = append(blk.rows, fileformat.RowIndexEntry{
				SnapshotID:  w.id,
				TableID:     table,
				ChangeType:  fb.Rows[i].ChangeType,
				RowID:       fb.Rows[i].RowID,
				ItemOrdinal: uint32(i),
			})
		}
		w.pending = append(w.pending, blk)
		return nil
	}
}

// rowBuilder returns (creating if needed) the rows builder for a table.
func (w *SnapshotWriter) rowBuilder(table TableID) *block.RowsBlockBuilder {
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
func (w *SnapshotWriter) attachZstdEncoder(setter interface{ SetZstdEncoder(*block.ZstdEncoder) }) {
	if w.store.opts.diskCompression() != fileformat.CompressionZstd {
		return
	}
	setter.SetZstdEncoder(w.store.zstdEncoder())
}

// Insert appends an INSERT change.
func (w *SnapshotWriter) Insert(ctx context.Context, table TableID, rowID RowID, schemaVersion SchemaVersion, row Row) error {
	return w.put(ctx, ChangeInsert, table, rowID, schemaVersion, row)
}

// Update appends an UPDATE change (DELTA snapshots only).
func (w *SnapshotWriter) Update(ctx context.Context, table TableID, rowID RowID, schemaVersion SchemaVersion, row Row) error {
	return w.put(ctx, ChangeUpdate, table, rowID, schemaVersion, row)
}

// Delete appends a DELETE tombstone (DELTA snapshots only).
func (w *SnapshotWriter) Delete(ctx context.Context, table TableID, rowID RowID) error {
	return w.put(ctx, ChangeDelete, table, rowID, 0, nil)
}

func (w *SnapshotWriter) put(ctx context.Context, typ ChangeType, table TableID, rowID RowID, schemaVersion SchemaVersion, row Row) error {
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
	key := rowKey{Table: table, Row: rowID}
	if _, dup := w.seenRows[key]; dup {
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
	w.seenRows[key] = struct{}{}
	w.rowRecordCount++
	w.rawBytes += uint64(fileformat.RowRecordHeaderSize + len(encoded))
	return nil
}

func (w *SnapshotWriter) resolveSchema(table TableID, version SchemaVersion) (*codec.Schema, error) {
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
func (w *SnapshotWriter) checkStrictParent(table TableID, rowID RowID, typ ChangeType) error {
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

// Apply consumes a channel of changes until closed, cancelled or an error.
func (w *SnapshotWriter) Apply(ctx context.Context, changes <-chan Change) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch, ok := <-changes:
			if !ok {
				return nil
			}
			if err := w.put(ctx, ch.Type, ch.TableID, ch.RowID, ch.SchemaVersion, ch.Row); err != nil {
				return err
			}
		}
	}
}

// Abort discards the snapshot. It is idempotent; Abort on a committed writer
// returns ErrSnapshotCommitted.
func (w *SnapshotWriter) Abort() error {
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
func (w *SnapshotWriter) abort() error {
	if w.state == writerCommitted {
		return ErrSnapshotCommitted
	}
	w.state = writerAborted
	w.store.writer.CompareAndSwap(w, nil)
	return nil
}

// Commit persists the snapshot and atomically publishes it to readers.
func (w *SnapshotWriter) Commit(ctx context.Context) (SnapshotInfo, error) {
	if err := w.checkState(); err != nil {
		return SnapshotInfo{}, err
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return SnapshotInfo{}, ctx.Err()
		default:
		}
	}
	w.store.writeMu.Lock()
	defer w.store.writeMu.Unlock()
	if err := w.store.checkOpen(); err != nil {
		w.state = writerFailed
		return SnapshotInfo{}, err
	}
	info, commitErr := w.commitLocked(ctx)
	if commitErr != nil {
		w.state = writerFailed
	}
	return info, commitErr
}

func (w *SnapshotWriter) commitLocked(ctx context.Context) (SnapshotInfo, error) {
	if err := w.flushAll(); err != nil {
		return SnapshotInfo{}, err
	}
	if len(w.pending) == 0 && !w.allowEmpty && w.typ == SnapshotFull {
		return SnapshotInfo{}, fmt.Errorf("%w: empty FULL snapshot", ErrInvalidArgument)
	}
	// Fault injection points (test-only): crash between these positions.
	fault.Check("commit.data-header.before")

	// Assign block IDs first so the snapshot header can record FirstBlockID.
	for _, blk := range w.pending {
		blk.header.BlockID = w.store.lastBlockID.Add(1)
	}
	// Assign offsets and write to .rpk.
	startOffset := w.store.data.Offset()
	snapStart := startOffset
	var sh fileformat.SnapshotHeader
	sh.SnapshotType = fileformat.SnapshotType(w.typ)
	sh.AllowEmpty = w.allowEmpty
	sh.SnapshotID = w.id
	sh.ParentSnapshotID = w.parent
	sh.CreatedUnixNano = w.created
	sh.WriterNonce = randUint64()
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

	snapEnd := w.store.data.Offset()
	// Footer.
	var ftr fileformat.SnapshotFooter
	ftr.SnapshotType = fileformat.SnapshotType(w.typ)
	ftr.SnapshotID = w.id
	ftr.ParentSnapshotID = w.parent
	ftr.SnapshotStartOffset = uint64(snapStart)
	ftr.SnapshotEndOffset = uint64(snapEnd + fileformat.SnapshotFooterSize)
	ftr.FirstBlockID = sh.FirstBlockID
	ftr.BlockCount = blockCount
	ftr.MetadataBlockCount = metaBlockCount
	ftr.RowRecordCount = w.rowRecordCount
	ftr.RawBytes = rawBytes
	ftr.BlocksCRC32C = fileformat.CRC32C(blockCRCs)
	var fb [fileformat.SnapshotFooterSize]byte
	if err := ftr.MarshalTo(fb[:]); err != nil {
		return SnapshotInfo{}, err
	}
	if _, err := w.store.data.Append(fb[:]); err != nil {
		return SnapshotInfo{}, err
	}
	fault.Check("commit.data-footer.after")
	dataEnd := w.store.data.Offset()

	// Durability: data sync.
	unknown := false
	fault.Check("commit.data-sync.before")
	if w.store.opts.Durability == SyncCommit {
		if err := w.store.data.Sync(); err != nil {
			return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: true, Err: err}
		}
	}
	fault.Check("commit.data-sync.after")
	// After the data sync, failures are "outcome unknown".
	unknown = true

	// Build and append the index transaction.
	txnBuilder := index.NewBuilder(w.store.txnSeq.Add(1))
	// Pre-allocate for all flushed blocks.
	var rBlocks, rMeta, rRows int
	for _, blk := range w.pending {
		rBlocks++
		rMeta += len(blk.meta)
		rRows += len(blk.rows)
	}
	txnBuilder.Reserve(rMeta, rBlocks, rRows)
	snapEntry := fileformat.SnapshotIndexEntry{
		SnapshotID:       w.id,
		ParentSnapshotID: w.parent,
		SnapshotType:     fileformat.SnapshotType(w.typ),
		BlockCount:       blockCount,
		RowRecordCount:   w.rowRecordCount,
		DataStart:        uint64(snapStart),
		DataEnd:          uint64(dataEnd),
		CreatedUnixNano:  w.created,
		DataFooterCRC32C: footerCRCValue(fb[:]),
	}
	if err := txnBuilder.SetSnapshot(snapEntry); err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
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
			return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
		}
		for i := range blk.meta {
			blk.meta[i].BlockID = blk.header.BlockID
			if err := txnBuilder.AddMetadata(blk.meta[i]); err != nil {
				return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
			}
		}
		for i := range blk.rows {
			blk.rows[i].BlockID = blk.header.BlockID
			if err := txnBuilder.AddRow(blk.rows[i]); err != nil {
				return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
			}
		}
	}
	fault.Check("commit.index.before")
	txnStart := w.store.index.Offset()
	txnEnd := txnStart + int64(0)
	txnBytes, txn, err := txnBuilder.Build(uint64(snapStart), uint64(dataEnd), footerCRCValue(fb[:]), txnStart, txnEnd+int64(0))
	if err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
	}
	txnEnd = txnStart + int64(len(txnBytes))
	if _, err := w.store.index.Append(txnBytes); err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
	}
	fault.Check("commit.index-sync.before")
	if w.store.opts.Durability == SyncCommit {
		if err := w.store.index.Sync(); err != nil {
			return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
		}
	}
	fault.Check("commit.index-sync.after")

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
		StoredBytes: uint64(w.store.data.Offset() - startOffset),
	}, nil
}

// buildNewSchemas derives the schema index for the new snapshot only (the
// parent snapshots' schemas are reused from the old index).
func (w *SnapshotWriter) buildNewSchemas(newView *index.View) (*schemaIndex, error) {
	base := w.store.state.Load()
	si := &schemaIndex{bySnapshot: make(map[uint64]map[uint32]*tableSchemas)}
	if base != nil && base.schemas != nil {
		for snap, tables := range base.schemas.bySnapshot {
			si.bySnapshot[snap] = tables
		}
	}
	tables, err := w.store.deriveTables(newView, w.id)
	if err != nil {
		return nil, err
	}
	if len(tables) > 0 {
		si.bySnapshot[w.id] = tables
	}
	return si, nil
}

// flushAll flushes every builder into pending blocks.
func (w *SnapshotWriter) flushAll() error {
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

func schemaEqual(a, b *codec.Schema) bool {
	if a.TableID != b.TableID || a.Version != b.Version || a.Name != b.Name || len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		ac, bc := a.Columns[i], b.Columns[i]
		if ac.Name != bc.Name || ac.Type != bc.Type || ac.Nullable != bc.Nullable || ac.Scale != bc.Scale {
			return false
		}
	}
	return true
}

var _ = errors.New
var _ = context.Canceled
