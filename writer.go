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
	"github.com/rowpack/rowpack/internal/seal"
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
	// seenRows rejects duplicate (table, row) pairs within the snapshot:
	// one packed rowIDSet per table (see rowset.go; ~11 B/row vs ~90 B/row
	// for a Go map).
	seenRows map[TableID]*rowIDSet

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
	// v2 checkpoint semantics (R7): every FULL takes the next global ID — the
	// first snapshot of an empty store is a FULL with id 1 naturally, and a
	// later FULL checkpoint continues the counter (Depth resets to 1 in
	// View.Apply; its visibility no longer follows any ancestor chain).
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
		seenRows:    make(map[TableID]*rowIDSet),
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
	// Check parent chain monotonicity. Only DELTAs extend an existing chain:
	// a FULL checkpoint (parent 0) always writes its own metadata layer, even
	// when the definition matches an ancestor's (R7 checkpoint semantics).
	if w.typ == SnapshotDelta {
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

// rowsFlush captures one completed rows block. The builder hands over
// ownership of its directory slice (it allocates a fresh one for its next
// block), so the pending block references it without a per-row copy;
// ItemOrdinal is the directory position.
func (w *SnapshotWriter) rowsFlush(table TableID) func(*block.FlushedBlock) error {
	return func(fb *block.FlushedBlock) error {
		blk := &pendingBlock{header: fb.Header, payload: fb.Stored, rowsDir: fb.Rows}
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
func (w *SnapshotWriter) rowSeen(table TableID, rowID RowID) bool {
	return w.seenRows[table].Contains(rowID)
}

// rememberRow records (table, rowID) as written. put() calls it only after
// the row builder accepted the record, so failed inserts never pollute the
// set. Nil sets are created lazily: most writers touch few tables.
func (w *SnapshotWriter) rememberRow(table TableID, rowID RowID) {
	set := w.seenRows[table]
	if set == nil {
		set = &rowIDSet{}
		w.seenRows[table] = set
	}
	set.Insert(rowID)
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
	sh.AllowEmpty = w.allowEmpty
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

	// The IndexTxn byte length is fully determined by the entry counts, so the
	// txn and footer offsets can be computed before serialization; the footer
	// binds the txn by exact byte extent plus CRC over the stored bytes. An
	// encrypted store seals the body+footer as one unit (plaintext header
	// stays scannable), adding exactly one AEAD tag to the stored extent.
	txnLen := int64(0)
	txnLen += fileformat.IndexTxnHeaderSize + fileformat.SnapshotIndexEntrySize + fileformat.IndexTxnFooterSize
	for _, blk := range w.pending {
		txnLen += int64(len(blk.meta))*fileformat.MetadataIndexEntrySize +
			fileformat.BlockIndexEntrySize + int64(len(blk.rowsDir))*fileformat.RowIndexEntrySize
	}
	if w.store.encCipher != nil {
		txnLen += fileformat.AESGCMTagLen
	}
	txnStart := blocksEnd
	txnEnd := txnStart + txnLen
	snapEnd := txnEnd + fileformat.SnapshotFooterSize

	// Build the embedded IndexTxn. DataEnd is the SnapshotFooter end (the
	// whole txn byte range, BINARY_FORMAT_V2 §6). DataFooterCRC32C is not
	// bound in v2: the footer (written later) carries the authoritative
	// IndexTxnCRC32C and the binding direction is footer -> txn.
	unknown := false
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
		DataEnd:          uint64(snapEnd),
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
	txnBytes, txn, err := txnBuilder.Build(uint64(snapStart), uint64(snapEnd), 0, txnStart, txnEnd)
	if err != nil {
		return SnapshotInfo{}, err
	}
	stored := txnBytes
	if w.store.encCipher != nil {
		// Index-domain sealing (R11): nonce = (epoch | domain bit) ‖ txn
		// sequence, never overlapping the block nonce space; AAD binds store,
		// snapshot and the exact stored extent (R8). Header and footer stay
		// plaintext — the scanner walks the txn by magic + BodyBytes + footer
		// magic without a key (R1) — so only the body is sealed, with
		// BodyBytes re-stamped to the ciphertext length (R12).
		const epoch = uint32(0)
		bodyLen := len(txnBytes) - fileformat.IndexTxnHeaderSize - fileformat.IndexTxnFooterSize
		nonce := seal.NonceIndex(epoch, txn.Header.TxnSequence)
		aad := seal.BuildAADIndex(&w.store.uuid, uint64(w.id), uint64(txnStart), uint64(txnEnd), epoch)
		ct := w.store.encCipher.SealWith(nonce, aad[:], txnBytes[fileformat.IndexTxnHeaderSize:fileformat.IndexTxnHeaderSize+bodyLen])
		stored = make([]byte, 0, fileformat.IndexTxnHeaderSize+len(ct)+fileformat.IndexTxnFooterSize)
		stored = append(stored, txnBytes[:fileformat.IndexTxnHeaderSize]...)
		stored = append(stored, ct...)
		stored = append(stored, txnBytes[len(txnBytes)-fileformat.IndexTxnFooterSize:]...)
		if err := fileformat.PatchIndexTxnHeaderForStorage(stored[:fileformat.IndexTxnHeaderSize], uint64(len(ct)), epoch); err != nil {
			return SnapshotInfo{}, err
		}
	}
	if int64(len(stored)) != txnLen {
		// Internal invariant: the stored extent must match the precomputed
		// boundary hop, otherwise the footer's extent binding would be wrong.
		return SnapshotInfo{}, fmt.Errorf("rowpack: index txn length %d != precomputed %d", len(stored), txnLen)
	}
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
func (w *SnapshotWriter) buildNewSchemas(newView *index.View) (*schemaIndex, error) {
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
