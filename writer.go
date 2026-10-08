package rowpack

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/codeforgee/rowpack/internal/fault"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/metadata"
	"github.com/codeforgee/rowpack/internal/seal"
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

	// tableIDs resolves table addresses to internal table IDs for this
	// transaction (tables created here and tables resolved from the chain).
	// Keys are addresses (Qualify(ns, name)).
	tableIDs map[string]TableID
	// tableNS remembers the ns of every table created in this transaction, so
	// a second DefineTable can detect an ns conflict without consulting the
	// committed chain.
	tableNS map[TableID]string
	// nextTableID is the lowest free internal table ID: one past the highest
	// table ID in the committed view, raised by every CreateTable.
	nextTableID uint32

	// meta is this snapshot's pending meta value: one opaque blob published
	// as its own BlockKindSnapshotMeta block by Commit. Empty means "no
	// meta", which is also how a snapshot that never called SetMeta is
	// represented on disk, so clearing and never setting are the same state.
	meta []byte

	// per-table rows block builders
	rowBuilders map[TableID]*block.RowsBuilder
	// metadata block builder
	metaBuilder *block.MetadataBuilder

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

	allocator *metadata.IDAllocator
	maxObject uint64

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
	header  format.BlockHeader
	payload []byte
	offset  int64 // .rpk offset assigned at commit
	// rowsDir is the rows-block directory in record order (rows blocks
	// only, nil otherwise). The block builder transfers ownership of the
	// slice at flush time, so no per-row copy exists on the commit path;
	// RowIndexEntries are built straight into the pre-reserved txn builder
	// at commit.
	rowsDir []format.RowDirectoryEntry
	meta    []format.MetadataIndexEntry
}

// newWriter constructs the single active writer. A Store allows at most one
// active writer; a concurrent begin returns ErrWriterBusy.
func (s *Store) newWriter(typ SnapshotType, parent SnapshotID) (*Writer, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, ErrReadOnly
	}
	if s.mustReopen.Load() {
		return nil, fmt.Errorf("%w: an earlier commit failed with unknown outcome; close and open %q to recover", ErrMustReopen, s.basePath)
	}
	w := &Writer{
		store:       s,
		typ:         typ,
		parent:      parent,
		created:     effectiveNow(),
		state:       writerOpen,
		rowBuilders: make(map[TableID]*block.RowsBuilder),
		schemas:     make(map[schemaKey]*codec.Schema),
		seenRows:    make(map[TableID]*rowIDSet),
		tableIDs:    make(map[string]TableID),
		tableNS:     make(map[TableID]string),
		allocator:   metadata.NewIDAllocator(),
	}
	if !s.writer.CompareAndSwap(nil, w) {
		return nil, ErrWriterBusy
	}
	// Only the winner performs history-dependent initialization and consumes
	// a snapshot ID. Losing concurrent Begin calls are cheap and leave no ID
	// holes.
	w.id = SnapshotID(s.lastSnapshotID.Add(1))
	if maxObject := s.maxObjectID.Load(); maxObject >= metadata.TableSpaceEnd {
		w.allocator.Force(maxObject, "reserved")
		w.maxObject = maxObject
	}
	w.nextTableID = s.maxTableID.Load() + 1
	return w, nil
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

// parentOf returns the snapshot this writer extends (for schema
// resolution).
func (w *Writer) parentOf() uint64 {
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

// SetMeta sets this snapshot's meta value: exactly one opaque blob per
// snapshot, materialized as a BlockKindSnapshotMeta block and published by
// Commit together with the rows and schema changes. The engine never
// interprets the content — it validates the length (bounded by
// Limits.MaxRawBlockBytes, because the value becomes one block payload),
// CRC-protects it and reproduces the bytes verbatim at read time. The value
// therefore carries whatever the caller wants it to: application version,
// capture parameters, an external manifest, a human-readable log.
//
// Repeated calls replace the pending value (last write wins); nil or an empty
// slice clears it, so a snapshot never carries an empty block. SetMeta copies
// its argument, so the caller may reuse the slice afterwards. Commit stores
// the value block-compressed; Meta() returns it decompressed, byte for byte.
func (w *Writer) SetMeta(value []byte) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if len(value) > int(w.store.opts.Limits.MaxRawBlockBytes) {
		return fmt.Errorf("%w: meta of %d bytes exceeds limit %d",
			ErrInvalidArgument, len(value), w.store.opts.Limits.MaxRawBlockBytes)
	}
	w.meta = append(w.meta[:0], value...)
	return nil
}

// writeMetadata encodes and buffers a metadata record into the metadata block
// builder.
func (w *Writer) writeMetadata(rec *metadata.Record) error {
	body, err := rec.Encode(metadata.CoreFieldSchemas[rec.RecordType])
	if err != nil {
		return err
	}
	w.metaRecords = append(w.metaRecords, rec)
	return w.addMetadata(rec, body)
}

func (w *Writer) ensureMetadata() *block.MetadataBuilder {
	if w.metaBuilder == nil {
		w.metaBuilder = block.NewMetadataBuilder(w.id, 0, block.Config{
			BlockSize:   w.store.opts.BlockSize,
			Compression: w.store.opts.diskCompression(),
			Level:       w.store.opts.CompressionLevel,
			Limits: block.Limits{
				MaxRawBytes:    w.store.opts.Limits.MaxRawBlockBytes,
				MaxStoredBytes: w.store.opts.Limits.MaxStoredBlockBytes,
			},
			OnFlush: w.metaFlush,
		})
		w.setEncoder(w.metaBuilder)
	}
	return w.metaBuilder
}

func (w *Writer) addMetadata(rec *metadata.Record, body []byte) error {
	entry := metadata.DirectoryEntry{
		ObjectID:   rec.ObjectID,
		Revision:   rec.Revision,
		RecordType: rec.RecordType,
		Operation:  format.OperationUpsert,
		Critical:   rec.Critical,
	}
	return w.ensureMetadata().Add(entry, body)
}

// metaFlush captures one completed metadata block. The builder supplies the
// directory entries directly, so no payload re-parse is needed.
func (w *Writer) metaFlush(fb *block.FlushedBlock) error {
	blk := &pendingBlock{header: fb.Header, payload: fb.Stored}
	blk.meta = make([]format.MetadataIndexEntry, 0, len(fb.Meta))
	for i := range fb.Meta {
		blk.meta = append(blk.meta, format.MetadataIndexEntry{
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

// flushMetaBlock materializes the pending meta value as one snapshot-meta
// block. It is appended last so setting a meta never reorders the rows and
// metadata blocks of an existing snapshot.
//
// The value is the whole payload: no envelope, no directory, no records.
// ItemCount is 1 (one blob) and RawCRC32C covers the value itself, which is
// all the length validation the read path needs. The value is always stored
// compressed with the store's block algorithm — Meta() decompresses on read —
// with no expansion fallback: a near-limit incompressible value whose
// compressed form exceeds MaxStoredBlockBytes is rejected here instead.
func (w *Writer) flushMetaBlock() error {
	if len(w.meta) == 0 {
		return nil
	}
	comp := w.store.opts.diskCompression()
	stored, err := block.Compress(comp, w.store.opts.CompressionLevel, w.meta)
	if err != nil {
		return err
	}
	if uint64(len(stored)) > uint64(w.store.opts.Limits.MaxStoredBlockBytes) {
		return fmt.Errorf("%w: meta block of %d stored bytes exceeds limit %d",
			ErrInvalidArgument, len(stored), w.store.opts.Limits.MaxStoredBlockBytes)
	}
	w.pending = append(w.pending, &pendingBlock{
		header: format.BlockHeader{
			BlockKind:   format.BlockKindSnapshotMeta,
			Compression: comp,
			SnapshotID:  w.id,
			ItemCount:   1,
			RawSize:     uint32(len(w.meta)),
			StoredSize:  uint32(len(stored)),
			RawCRC32C:   format.CRC32C(w.meta),
		},
		payload: stored,
	})
	return nil
}

// rowsFlush captures one completed rows block. The builder hands over
// ownership of its directory slice (it allocates a fresh one for its next
// block), so the pending block references it without a per-row copy;
// ItemOrdinal is the directory position.
func (w *Writer) rowsFlush() func(*block.FlushedBlock) error {
	return func(fb *block.FlushedBlock) error {
		blk := &pendingBlock{header: fb.Header, payload: fb.Stored, rowsDir: fb.Rows}
		if fb.OversizedPages > 0 {
			w.store.oversizedPages.Add(uint64(fb.OversizedPages))
		}
		w.pending = append(w.pending, blk)
		return nil
	}
}

// rowBuilder returns (creating if needed) the rows builder for a table.
func (w *Writer) rowBuilder(table TableID) *block.RowsBuilder {
	if b := w.rowBuilders[table]; b != nil {
		return b
	}
	b := block.NewRowsBuilder(w.id, table, block.Config{
		BlockSize:   w.store.opts.BlockSize,
		Compression: w.store.opts.diskCompression(),
		Level:       w.store.opts.CompressionLevel,
		Limits: block.Limits{
			MaxRawBytes:    w.store.opts.Limits.MaxRawBlockBytes,
			MaxStoredBytes: w.store.opts.Limits.MaxStoredBlockBytes,
		},
		OnFlush: w.rowsFlush(),
	})
	b.SetPageSize(w.store.opts.PageSize)
	w.setEncoder(b)
	w.rowBuilders[table] = b
	return b
}

// setEncoder hands the store's persistent zstd encoder to a block
// builder when the disk compression is zstd. The store owns the encoder (one
// writer at a time, sequential flushes), so its ~1 MiB histogram is allocated
// once per store instead of once per pool-recreating GC cycle.
func (w *Writer) setEncoder(setter interface{ SetZstdEncoder(*block.ZstdEncoder) }) {
	if w.store.opts.diskCompression() != format.CompressionZstd {
		return
	}
	setter.SetZstdEncoder(w.store.zstdEncoder())
}

// createTable creates a table in this snapshot. The address (ns + name) is the
// caller-facing identity; the internal table ID and schema version are
// assigned by the engine and never surface in the API.
//
//   - same address + same columns within this transaction => idempotent no-op;
//   - same address + different columns => ErrSchemaConflict;
//   - a table of the same address on the parent chain resolves to the existing
//     internal ID: same columns => no-op for a DELTA, the snapshot's own
//     metadata layer for a FULL; different columns => ErrSchemaConflict;
//   - the ns is part of the identity, so the same name in another ns is a
//     different table and never conflicts.
func (w *Writer) createTable(ns, name string, columns []Column) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if name == "" {
		return fmt.Errorf("%w: table name is empty", ErrInvalidArgument)
	}
	if ns == "" {
		return fmt.Errorf("%w: ns is empty", ErrInvalidArgument)
	}
	// No character is forbidden in ns or name: the address is the key, and a
	// name may contain the separator because Qualify/SplitAddress resolve at the
	// first one. Two different (ns, name) pairs can still produce the same
	// address -- a dotted name in NSUser versus a ns named after its prefix --
	// so the address is checked for ownership rather than the characters.
	address := Qualify(ns, name)
	// Only a table this transaction has defined has a txn schema version. A table
	// merely cached by tableForWrite falls through to the chain branch, which
	// keeps DELTA and FULL apart (a FULL writes its own metadata layer).
	if tid, ok := w.tableIDs[address]; ok && w.latestVersion(tid) != 0 {
		if prev := w.nsOf(tid); prev != ns || w.nameOf(tid) != name {
			return fmt.Errorf("%w: address %q already names table %q in ns %q",
				ErrSchemaConflict, address, w.nameOf(tid), prev)
		}
		if w.txnColumnsEqual(tid, columns) {
			return nil
		}
		return fmt.Errorf("%w: table %q already defined with different columns", ErrSchemaConflict, address)
	}
	// Resolve against the committed parent chain.
	st := w.store.state.Load()
	if st != nil {
		if chainTID, ok := st.schemas.tableID(uint64(w.parentOf()), address); ok {
			if prev := st.schemas.nsOf(uint64(w.parentOf()), uint32(chainTID)); prev != ns ||
				st.schemas.nameOf(uint64(w.parentOf()), uint32(chainTID)) != name {
				return fmt.Errorf("%w: address %q already names table %q in ns %q",
					ErrSchemaConflict, address, st.schemas.nameOf(uint64(w.parentOf()), uint32(chainTID)), prev)
			}
			if !w.columnsEqual(uint64(w.parentOf()), chainTID, columns) {
				return fmt.Errorf("%w: table %q already exists with different columns", ErrSchemaConflict, address)
			}
			if w.typ == SnapshotDelta {
				// DELTA: readers resolve the schema along the parent chain, so
				// an identical re-definition needs no metadata of its own.
				w.tableIDs[address] = chainTID
				w.tableNS[chainTID] = ns
				return nil
			}
			// FULL checkpoint (parent 0): always write the snapshot's own
			// metadata layer, even when identical to the ancestor's --
			// its visibility no longer follows any ancestor chain.
			latest := st.schemas.latest(uint64(w.parentOf()), uint32(chainTID))
			if latest == 0 {
				latest = 1
			}
			if err := w.writeRecords(tableDef{
				ID: uint32(chainTID), Version: latest, Name: name,
				NS: ns, Address: address, Columns: columns,
			}); err != nil {
				return err
			}
			w.tableIDs[address] = chainTID
			w.tableNS[chainTID] = ns
			return nil
		}
	}
	// New table: allocate the internal ID and define version 1.
	tid := w.nextTableID
	if tid == 0 || tid == ^uint32(0) {
		return fmt.Errorf("%w: table id space exhausted", ErrInvalidArgument)
	}
	if err := w.writeRecords(tableDef{
		ID: tid, Version: 1, Name: name,
		NS: ns, Address: address, Columns: columns,
	}); err != nil {
		return err
	}
	w.nextTableID++
	w.tableIDs[address] = tid
	w.tableNS[tid] = ns
	return nil
}

// nsOf returns the ns a table resolves to in this transaction:
// its own definition first, then the committed chain (the default ns when the
// table carries no ns).
func (w *Writer) nsOf(tid TableID) string {
	if ns, ok := w.tableNS[tid]; ok {
		return ns
	}
	st := w.store.state.Load()
	if st == nil {
		return NSUser
	}
	return st.schemas.nsOf(uint64(w.parentOf()), uint32(tid))
}

// nameOf returns the bare name of a table defined in this transaction, or "".
// createTable consults it only for tables with a txn schema version.
func (w *Writer) nameOf(tid TableID) string {
	if sch := w.schemas[schemaKey{Table: tid, Version: w.latestVersion(tid)}]; sch != nil {
		return sch.Name
	}
	return ""
}

// tableDef is one table definition to write into this snapshot's metadata
// layer.
type tableDef struct {
	ID      uint32
	Version uint32
	Name    string
	NS      string
	Address string
	Columns []Column
}

// writeRecords writes the Table + Column metadata records of one table version
// and records the resolved schema in the writer's txn map.
func (w *Writer) writeRecords(def tableDef) error {
	schema := codec.Schema{TableID: def.ID, Version: def.Version, Name: def.Name, Columns: def.Columns}
	if err := schema.Validate(w.store.rowCodec().Limits); err != nil {
		return err
	}
	tableOID := uint64(def.ID)
	// The ns field is omitted for the default ns, so that case stays
	// byte-identical to stores written before ns existed.
	fields := []metadata.Field{
		{ID: metadata.TableName, WireType: format.WireString, Value: schema.Name},
	}
	if def.NS != NSUser {
		fields = append(fields, metadata.Field{
			ID: metadata.TableNS, WireType: format.WireString, Value: def.NS,
		})
	}
	tableRec := &metadata.Record{
		RecordType:  uint32(format.RecordTable),
		ObjectID:    tableOID,
		Revision:    schema.Version,
		Namespace:   format.NamespaceCore,
		ExternalKey: schema.Name,
		Fields:      fields,
	}
	if err := w.writeMetadata(tableRec); err != nil {
		return err
	}
	for i, col := range schema.Columns {
		objectID := w.allocator.Alloc(format.NamespaceCore,
			fmt.Sprintf("%s:%d:%s", def.Address, schema.Version, col.Name))
		if objectID > w.maxObject {
			w.maxObject = objectID
		}
		colRec := &metadata.Record{
			RecordType: uint32(format.RecordColumn),
			ObjectID:   objectID,
			ParentID:   tableOID,
			Revision:   1,
			Namespace:  format.NamespaceCore,
			Fields: []metadata.Field{
				{ID: metadata.ColColumnID, WireType: format.WireSint, Value: int64(i + 1)},
				{ID: metadata.ColColumnName, WireType: format.WireString, Value: col.Name},
				{ID: metadata.ColColumnType, WireType: format.WireString, Value: typeName(col.Type)},
				{ID: metadata.ColNullable, WireType: format.WireString, Value: nullString(col.Nullable)},
				{ID: metadata.ColDataScale, WireType: format.WireSint, Value: int64(col.Scale)},
			},
		}
		if col.PrimaryKey {
			colRec.Fields = append(colRec.Fields, metadata.Field{ID: metadata.ColPrimaryKey, WireType: format.WireSint, Value: int64(1)})
		}
		if err := w.writeMetadata(colRec); err != nil {
			return err
		}
	}
	w.schemas[schemaKey{Table: def.ID, Version: schema.Version}] = schema.Clone()
	return nil
}

// txnColumnsEqual compares columns against the latest schema version defined in
// this transaction; false when it defined none (the chain comparison belongs to
// createTable's chain branch).
func (w *Writer) txnColumnsEqual(tid TableID, columns []Column) bool {
	latest := w.latestVersion(tid)
	if latest == 0 {
		return false
	}
	s := w.schemas[schemaKey{Table: tid, Version: latest}]
	return schemaColumnsEqual(s, columns)
}

// columnsEqual compares the latest committed schema of (snapshot, tid)
// against columns.
func (w *Writer) columnsEqual(snapshot uint64, tid TableID, columns []Column) bool {
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

// tableForWrite resolves a table address to (internal ID, schema
// version) for a write: tables created in this transaction first, then the
// committed parent chain; the schema version is the table's latest — the
// newest version defined in this transaction, or the committed latest when
// the table was defined by an ancestor snapshot.
func (w *Writer) tableForWrite(table string) (TableID, SchemaVersion, error) {
	tid, cached := w.tableIDs[table]
	if !cached {
		st := w.store.state.Load()
		if st == nil {
			return 0, 0, fmt.Errorf("%w: table %q", ErrNotFound, table)
		}
		var ok bool
		if tid, ok = st.schemas.tableID(uint64(w.parentOf()), table); !ok {
			return 0, 0, fmt.Errorf("%w: table %q", ErrNotFound, table)
		}
		w.tableIDs[table] = tid // cache for subsequent writes
	}
	ver := w.latestVersion(tid)
	if ver == 0 {
		if st := w.store.state.Load(); st != nil {
			ver = SchemaVersion(st.schemas.latest(uint64(w.parentOf()), uint32(tid)))
		}
	}
	return tid, ver, nil
}

func (w *Writer) latestVersion(tid TableID) SchemaVersion {
	var latest SchemaVersion
	for key := range w.schemas {
		if key.Table == tid && key.Version > latest {
			latest = key.Version
		}
	}
	return latest
}

// Insert appends an INSERT change. The table address is resolved against the
// committed parent chain and this transaction's definitions; the row is encoded
// against the table's latest schema.
func (w *Writer) Insert(ctx context.Context, table string, rowID RowID, row Row) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, ver, err := w.tableForWrite(table)
	if err != nil {
		return err
	}
	return w.put(ctx, rowChange{typ: ChangeInsert, table: tid, rowID: rowID, schemaVersion: ver, row: row})
}

// Update appends an UPDATE change (DELTA snapshots only).
func (w *Writer) Update(ctx context.Context, table string, rowID RowID, row Row) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, ver, err := w.tableForWrite(table)
	if err != nil {
		return err
	}
	return w.put(ctx, rowChange{typ: ChangeUpdate, table: tid, rowID: rowID, schemaVersion: ver, row: row})
}

// Delete appends a DELETE tombstone (DELTA snapshots only).
func (w *Writer) Delete(ctx context.Context, table string, rowID RowID) error {
	if err := w.checkState(); err != nil {
		return err
	}
	if rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, _, err := w.tableForWrite(table)
	if err != nil {
		return err
	}
	return w.put(ctx, rowChange{typ: ChangeDelete, table: tid, rowID: rowID})
}

// rowChange is the internal, already-resolved form of one row mutation: the
// public Change with its table address resolved to a TableID and its schema
// version pinned. Insert/Update/Delete resolve the public arguments once and
// hand put a single semantic value.
type rowChange struct {
	typ           ChangeType
	table         TableID
	rowID         RowID
	schemaVersion SchemaVersion
	row           Row
}

func (w *Writer) put(ctx context.Context, c rowChange) error {
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
	if c.rowID == 0 {
		return fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	if w.rowSeen(c.table, c.rowID) {
		return fmt.Errorf("%w: duplicate (table %d, row %d) in snapshot %d", ErrAlreadyExists, c.table, c.rowID, w.id)
	}
	if w.typ == SnapshotFull && c.typ != ChangeInsert {
		return fmt.Errorf("%w: FULL snapshot only allows INSERT", ErrInvalidArgument)
	}
	var encoded []byte
	if c.typ == ChangeDelete {
		// Tombstone: no row payload. The strict parent-existence check still
		// applies: deleting a row that does not exist in the parent view is
		// reported like an UPDATE of a missing row (checkParent handles
		// ChangeDelete explicitly).
		if err := w.checkParent(c.table, c.rowID, c.typ); err != nil {
			return err
		}
	} else {
		if err := w.checkParent(c.table, c.rowID, c.typ); err != nil {
			return err
		}
		schema, err := w.resolveSchema(c.table, c.schemaVersion)
		if err != nil {
			return err
		}
		// Body-only TypedTuple: the Rows Page layout carries ColumnCount and
		// NullBitmapBytes out of band (resolved from the schema), so the page
		// record drops the 8-byte tuple header.
		w.encBuf, err = w.store.rowCodec().EncodeInto(schema, c.row, w.encBuf)
		if err != nil {
			return err
		}
		encoded = w.encBuf
	}
	rb := w.rowBuilder(c.table)
	if err := rb.Add(c.rowID, c.schemaVersion, format.ChangeType(c.typ), encoded); err != nil {
		return err
	}
	w.rememberRow(c.table, c.rowID)
	w.rowRecordCount++
	w.rawBytes += uint64(len(encoded))
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
		if schema := st.schemas.schema(w.parentOf(), table, version); schema != nil {
			return schema, nil
		}
	}
	return nil, fmt.Errorf("%w: schema for table %d version %d not defined", ErrSchemaMismatch, table, version)
}

// checkParent enforces parent-view existence for DELTA changes.
func (w *Writer) checkParent(table TableID, rowID RowID, typ ChangeType) error {
	if w.typ != SnapshotDelta || w.store.opts.Validation == ValidationNone {
		return nil
	}
	st := w.store.state.Load()
	if st == nil {
		return nil
	}
	// Parent-view existence is resolved along the whole parent chain, not just
	// the immediate parent layer.
	loc, ok := st.view.ResolveRow(w.parent, table, rowID)
	exists := ok && loc.ChangeType != format.ChangeDelete
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
	w.store.writeMu.Lock()
	defer w.store.writeMu.Unlock()
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
	info, commitErr := w.commitLocked()
	if commitErr != nil {
		w.state = writerFailed
		w.store.writer.CompareAndSwap(w, nil)
		var ce *CommitError
		if errors.As(commitErr, &ce) && ce.Unknown {
			// The snapshot may or may not be durably committed: the in-memory
			// view can no longer be trusted for writes. Refuse new writers
			// until the store is reopened and recovery aligns the view with
			// the file.
			w.store.mustReopen.Store(true)
		}
	}
	return info.ID, commitErr
}

func (w *Writer) commitLocked() (SnapshotInfo, error) {
	if err := w.flushAll(); err != nil {
		return SnapshotInfo{}, err
	}
	if len(w.pending) == 0 && w.typ == SnapshotFull {
		return SnapshotInfo{}, fmt.Errorf("%w: empty FULL snapshot", ErrInvalidArgument)
	}
	// A table with rows but no visible schema would commit undecodable rows.
	// Checked before the first byte is written: after the sync, it is durable.
	if err := w.checkSchemaCoverage(); err != nil {
		return SnapshotInfo{}, err
	}
	// Fault injection points (test-only): crash between these positions.
	fault.Check("commit.header.before")

	// Assign block IDs first so the snapshot header can record FirstBlockID.
	for _, blk := range w.pending {
		blk.header.BlockID = w.store.lastBlockID.Add(1)
	}
	// Single-file commit order: SnapshotHeader -> Blocks -> IndexTxn ->
	// SnapshotFooter, then exactly one Sync (BINARY_FORMAT_V1 §8).
	startOffset := w.store.data.Offset()
	snapStart := startOffset
	sh, err := w.writeHeader()
	if err != nil {
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
		if err := w.sealPendingBlock(blk); err != nil {
			return SnapshotInfo{}, err
		}
		hb, err := w.writePendingBlock(blk)
		if err != nil {
			return SnapshotInfo{}, err
		}
		blockCount++
		if blk.header.BlockKind == format.BlockKindMetadata {
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
	snapEntry := format.SnapshotIndexEntry{
		SnapshotID:       w.id,
		ParentSnapshotID: w.parent,
		SnapshotType:     format.SnapshotType(w.typ),
		BlockCount:       blockCount,
		RowRecordCount:   w.rowRecordCount,
		DataStart:        uint64(snapStart),
		DataEnd:          uint64(snapStart), // resolved by BuildStored
		CreatedUnixNano:  w.created,
	}
	if err := txnBuilder.SetSnapshot(snapEntry); err != nil {
		return SnapshotInfo{}, err
	}
	if err := w.addBlocksToTxn(txnBuilder); err != nil {
		return SnapshotInfo{}, err
	}
	fault.Check("commit.txn.before")
	// After the single sync below, failures are "outcome unknown"; before it,
	// a failure is a known torn commit.
	unknown := false
	// Index-domain chunk sealing: each chunk is sealed under its own
	// HMAC-derived nonce (the NonceIndex 96-bit space is full) and AAD bound
	// to store/txn/chunk identity and lengths. Header, chunk headers and the
	// directory stay plaintext — the scanner walks the txn by magic +
	// BodyBytes + footer magic without a key — while every payload is
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
				storedBytes := uint32(len(stored)) + format.AESGCMTagLen
				return c.SealIndexChunk(seal.ChunkContext{
					UUID:          uuid,
					TxnSequence:   txnSeq,
					SnapshotID:    uint64(w.id),
					ChunkSequence: chunkSeq,
					FirstOrdinal:  firstOrdinal,
					RawBytes:      uint32(rawBytes),
					StoredBytes:   storedBytes,
					Kind:          kind,
					Epoch:         epoch,
				}, stored)
			},
		}
	}
	var txnStartResolved, txnEndResolved, snapEndResolved int64
	stored, txn, err := txnBuilder.BuildStored(crypto, w.store.opts.CompressionLevel,
		func(bodyLen int) index.BodyBounds {
			l := int64(format.IndexTxnHeaderSize + bodyLen + format.IndexTxnFooterSize)
			ts := blocksEnd
			te := ts + l
			txnStartResolved, txnEndResolved, snapEndResolved = ts, te, te+format.SnapshotFooterSize
			return index.BodyBounds{DataStart: uint64(snapStart), DataEnd: uint64(snapEndResolved), TxnStart: ts, TxnEnd: te}
		}, 0, 0)
	if err != nil {
		return SnapshotInfo{}, err
	}
	txnStart, txnEnd, snapEnd := txnStartResolved, txnEndResolved, snapEndResolved
	if _, err := w.store.data.Append(stored); err != nil {
		return SnapshotInfo{}, err
	}

	// Footer: the commit authority; also binds the IndexTxn bytes.
	fault.Check("commit.footer.before")
	var ftr format.SnapshotFooter
	ftr.SnapshotType = format.SnapshotType(w.typ)
	ftr.SnapshotID = w.id
	ftr.ParentSnapshotID = w.parent
	ftr.PreviousFooterOffset = w.store.lastFooterOffset
	ftr.SnapshotStartOffset = uint64(snapStart)
	ftr.BlocksStartOffset = uint64(snapStart) + format.SnapshotHeaderSize
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
	ftr.BlocksCRC32C = format.CRC32C(blockCRCs)
	ftr.IndexTxnCRC32C = format.CRC32C(stored)
	var fb [format.SnapshotFooterSize]byte
	_ = ftr.MarshalTo(fb[:]) // exact-size buffer: cannot fail
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
	newSchemas, err := w.buildSchemas(newView)
	if err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: unknown, Err: err}
	}
	fault.Check("commit.publish.before")
	w.store.state.Store(&publishedState{view: newView, schemas: newSchemas})
	w.store.maxTableID.Store(w.nextTableID - 1)
	w.store.maxObjectID.Store(w.maxObject)
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

// writeHeader assigns the snapshot header fields and appends it before
// any blocks. Keeping this boundary explicit makes the on-disk commit order
// easier to audit.
func (w *Writer) writeHeader() (format.SnapshotHeader, error) {
	var h format.SnapshotHeader
	h.SnapshotType = format.SnapshotType(w.typ)
	h.SnapshotID = w.id
	h.ParentSnapshotID = w.parent
	h.CreatedUnixNano = w.created
	h.WriterNonce = effectiveWriterNonce()
	if len(w.pending) > 0 {
		h.FirstBlockID = w.pending[0].header.BlockID
	}
	var buf [format.SnapshotHeaderSize]byte
	_ = h.MarshalTo(buf[:]) // exact-size buffer: cannot fail
	if _, err := w.store.data.Append(buf[:]); err != nil {
		return format.SnapshotHeader{}, err
	}
	return h, nil
}

// sealPendingBlock applies the block encryption policy after the final block
// ID has been assigned. Rows blocks use per-page sealing; metadata blocks are
// sealed as one container.
func (w *Writer) sealPendingBlock(blk *pendingBlock) error {
	c := w.store.encCipher
	if c == nil {
		return nil
	}
	blk.header.Encrypted = true
	blk.header.KeyEpoch = 0
	if blk.header.BlockKind == format.BlockKindRows {
		sealer := pageSealer{
			cipher: c,
			uuid:   &w.store.uuid,
			limits: block.Limits{
				MaxRawBytes:    w.store.opts.Limits.MaxRawBlockBytes,
				MaxStoredBytes: w.store.opts.Limits.MaxStoredBlockBytes,
			},
		}
		sealed, err := sealer.seal(&blk.header, blk.payload)
		if err != nil {
			return err
		}
		blk.payload = sealed
		return nil
	}
	blk.header.StoredSize = uint32(len(blk.payload)) + format.AESGCMTagLen
	sealed, err := c.Seal(&w.store.uuid, &blk.header, blk.payload)
	if err != nil {
		return err
	}
	blk.payload = sealed
	return nil
}

func (w *Writer) addBlocksToTxn(builder *index.Builder) error {
	for _, blk := range w.pending {
		if err := builder.AddBlock(format.BlockIndexEntry{
			BlockID: blk.header.BlockID, SnapshotID: blk.header.SnapshotID,
			TableID: blk.header.TableID, BlockKind: blk.header.BlockKind,
			Compression: blk.header.Compression, DataOffset: uint64(blk.offset),
			RawSize: blk.header.RawSize, StoredSize: blk.header.StoredSize,
			ItemCount: blk.header.ItemCount, RawCRC32C: blk.header.RawCRC32C,
		}); err != nil {
			return err
		}
		for i := range blk.meta {
			blk.meta[i].BlockID = blk.header.BlockID
			if err := builder.AddMetadata(blk.meta[i]); err != nil {
				return err
			}
		}
		for i := range blk.rowsDir {
			de := &blk.rowsDir[i]
			if err := builder.AddRow(format.RowIndexEntry{
				SnapshotID: w.id, TableID: blk.header.TableID,
				ChangeType: de.ChangeType, RowID: de.RowID,
				BlockID: blk.header.BlockID, ItemOrdinal: uint32(i),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Writer) writePendingBlock(blk *pendingBlock) ([format.BlockHeaderSize]byte, error) {
	var hb [format.BlockHeaderSize]byte
	_ = blk.header.MarshalTo(hb[:]) // exact-size buffer: cannot fail
	off, err := w.store.data.Append(hb[:])
	if err != nil {
		return hb, err
	}
	blk.offset = off
	if _, err := w.store.data.Append(blk.payload); err != nil {
		return hb, err
	}
	return hb, nil
}

// checkSchemaCoverage rejects a snapshot that writes rows for a table with no
// schema visible at the snapshot: a FULL snapshot's metadata is not visible
// through any ancestor, so a chain table it only wrote to (tableForWrite
// resolved it, DefineTable never ran) would commit rows no reader can decode.
func (w *Writer) checkSchemaCoverage() error {
	checked := make(map[TableID]struct{})
	for _, blk := range w.pending {
		if len(blk.rowsDir) == 0 {
			continue
		}
		tid := TableID(blk.header.TableID)
		if _, ok := checked[tid]; ok {
			continue
		}
		checked[tid] = struct{}{}
		if w.latestVersion(tid) != 0 {
			continue // defined in this snapshot
		}
		if st := w.store.state.Load(); st != nil && w.typ == SnapshotDelta {
			if st.schemas.latest(uint64(w.parentOf()), uint32(tid)) != 0 {
				continue // inherited from the parent chain
			}
		}
		return fmt.Errorf("%w: table %q has rows but no schema in this snapshot; define it with DefineTable",
			ErrInvalidArgument, w.addressOf(tid))
	}
	return nil
}

// addressOf returns the address a table was resolved under in this transaction,
// or "" (every written table is cached in tableIDs by tableForWrite first).
func (w *Writer) addressOf(tid TableID) string {
	for addr, id := range w.tableIDs {
		if id == tid {
			return addr
		}
	}
	return ""
}

// buildSchemas derives the schema index for the new snapshot only (the
// parent snapshots' schemas are reused from the old index).
func (w *Writer) buildSchemas(newView *index.View) (*schemaIndex, error) {
	base := w.store.state.Load()
	si := newSchemaIndex()
	if base != nil && base.schemas != nil {
		for snap, tables := range base.schemas.bySnapshot {
			si.bySnapshot[snap] = tables
		}
		for snap, addrs := range base.schemas.byAddress {
			si.byAddress[snap] = addrs
		}

	}
	d, err := w.store.deriveTables(newView, w.id, nil)
	if err != nil {
		return nil, err
	}
	if len(d.tables) > 0 {
		si.bySnapshot[w.id] = d.tables
	}
	if len(d.byAddress) > 0 {
		si.byAddress[w.id] = d.byAddress
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
	// Last, so the rows/metadata block order above is unaffected by SetMeta.
	return w.flushMetaBlock()
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
