package rowpack

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"slices"
	"time"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/metadata"
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

// writerState is the writer lifecycle state.
type writerState uint8

const (
	writerOpen writerState = iota
	writerCommitted
	writerAborted
	writerFailed
)

// writer buffers one snapshot transaction and commits it durably. It is
// created only through Store.Begin (as Tx's engine side) and is not safe for
// concurrent use.
type writer struct {
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
	// latestVer tracks the highest schema version defined per table in this
	// snapshot, so the write path (tableForWrite runs per Insert/Update)
	// resolves it in O(1) instead of scanning the schemas map.
	latestVer map[TableID]SchemaVersion

	// encBuf is reused across row encodes to cut per-row allocation.
	encBuf []byte
	// seenRows rejects duplicate (table, row) pairs within the snapshot:
	// one packed rowIDSet per table (see rowset.go; ~11 B/row vs ~90 B/row
	// for a Go map).
	seenRows map[TableID]*rowIDSet

	// pending blocks in flush order (rows and metadata interleaved)
	pending []*pendingBlock
	// metaCounts counts the metadata records accepted per record type in this
	// transaction (encoded and handed to the builder). The builder owns the
	// buffered bytes, so the writer retains only this small counter instead
	// of every record.
	metaCounts map[format.RecordType]int

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
func (s *Store) newWriter(typ SnapshotType, parent SnapshotID) (*writer, error) {
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
	w := &writer{
		store:       s,
		typ:         typ,
		parent:      parent,
		created:     effectiveNow(),
		state:       writerOpen,
		rowBuilders: make(map[TableID]*block.RowsBuilder),
		schemas:     make(map[schemaKey]*codec.Schema),
		latestVer:   make(map[TableID]SchemaVersion),
		seenRows:    make(map[TableID]*rowIDSet),
		tableIDs:    make(map[string]TableID),
		tableNS:     make(map[TableID]string),
		metaCounts:  make(map[format.RecordType]int),
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

// snapshotID returns the assigned snapshot ID.
func (w *writer) snapshotID() SnapshotID { return w.id }

// parentID returns the parent snapshot ID (0 for FULL).
func (w *writer) parentID() SnapshotID { return w.parent }

func (w *writer) checkState() error {
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
func (w *writer) parentOf() uint64 {
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

// setMeta sets this snapshot's meta value: exactly one opaque blob per
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
func (w *writer) setMeta(value []byte) error {
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
func (w *writer) writeMetadata(rec *metadata.Record) error {
	body, err := rec.Encode(metadata.CoreFieldSchemas[rec.RecordType])
	if err != nil {
		return err
	}
	w.metaCounts[format.RecordType(rec.RecordType)]++
	return w.addMetadata(rec, body)
}

func (w *writer) ensureMetadata() *block.MetadataBuilder {
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

func (w *writer) addMetadata(rec *metadata.Record, body []byte) error {
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
func (w *writer) metaFlush(fb *block.FlushedBlock) error {
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
func (w *writer) flushMetaBlock() error {
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
func (w *writer) rowsFlush() func(*block.FlushedBlock) error {
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
func (w *writer) rowBuilder(table TableID) *block.RowsBuilder {
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
func (w *writer) setEncoder(setter interface{ SetZstdEncoder(*block.ZstdEncoder) }) {
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
func (w *writer) createTable(ns, name string, columns []Column) error {
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
func (w *writer) nsOf(tid TableID) string {
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
func (w *writer) nameOf(tid TableID) string {
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
func (w *writer) writeRecords(def tableDef) error {
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
		// The canonical YES/NO nullable marker (see isNullableString on the
		// read side).
		nullable := "NO"
		if col.Nullable {
			nullable = "YES"
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
				{ID: metadata.ColNullable, WireType: format.WireString, Value: nullable},
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
	if SchemaVersion(schema.Version) > w.latestVer[def.ID] {
		w.latestVer[def.ID] = SchemaVersion(schema.Version)
	}
	return nil
}

// txnColumnsEqual compares columns against the latest schema version defined in
// this transaction; false when it defined none (the chain comparison belongs to
// createTable's chain branch).
func (w *writer) txnColumnsEqual(tid TableID, columns []Column) bool {
	latest := w.latestVersion(tid)
	if latest == 0 {
		return false
	}
	s := w.schemas[schemaKey{Table: tid, Version: latest}]
	return schemaColumnsEqual(s, columns)
}

// columnsEqual compares the latest committed schema of (snapshot, tid)
// against columns.
func (w *writer) columnsEqual(snapshot uint64, tid TableID, columns []Column) bool {
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
func (w *writer) tableForWrite(table string) (TableID, SchemaVersion, error) {
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

// latestVersion returns the highest schema version this transaction defined
// for the table (0 when none), via the latestVer index maintained by
// writeRecords.
func (w *writer) latestVersion(tid TableID) SchemaVersion {
	return w.latestVer[tid]
}

// resolveWrite validates the writer state and the public arguments and
// resolves the table address to its internal form: the single preamble shared
// by Insert/Update/Delete. A DELETE tombstone pins no schema (version 0):
// it carries no payload, matching the historical on-disk form.
func (w *writer) resolveWrite(table string, rowID RowID, typ ChangeType) (rowChange, error) {
	if err := w.checkState(); err != nil {
		return rowChange{}, err
	}
	if rowID == 0 {
		return rowChange{}, fmt.Errorf("%w: row id is zero", ErrInvalidArgument)
	}
	tid, ver, err := w.tableForWrite(table)
	if err != nil {
		return rowChange{}, err
	}
	if typ == ChangeDelete {
		ver = 0
	}
	return rowChange{typ: typ, table: tid, rowID: rowID, schemaVersion: ver}, nil
}

// Insert appends an INSERT change. The table address is resolved against the
// committed parent chain and this transaction's definitions; the row is encoded
// against the table's latest schema.
func (w *writer) insert(ctx context.Context, table string, rowID RowID, row Row) error {
	c, err := w.resolveWrite(table, rowID, ChangeInsert)
	if err != nil {
		return err
	}
	c.row = row
	return w.put(ctx, c)
}

// Update appends an UPDATE change (DELTA snapshots only).
func (w *writer) update(ctx context.Context, table string, rowID RowID, row Row) error {
	c, err := w.resolveWrite(table, rowID, ChangeUpdate)
	if err != nil {
		return err
	}
	c.row = row
	return w.put(ctx, c)
}

// Delete appends a DELETE tombstone (DELTA snapshots only).
func (w *writer) delete(ctx context.Context, table string, rowID RowID) error {
	c, err := w.resolveWrite(table, rowID, ChangeDelete)
	if err != nil {
		return err
	}
	return w.put(ctx, c)
}

// rowChange is the internal, already-resolved form of one row mutation: the
// public Change with its table address resolved to a TableID and its schema
// version pinned. resolveWrite produces it once per public call.
type rowChange struct {
	typ           ChangeType
	table         TableID
	rowID         RowID
	schemaVersion SchemaVersion
	row           Row
}

func (w *writer) put(ctx context.Context, c rowChange) error {
	// Belt and braces: put is also called directly (tests, future callers),
	// so it re-checks the invariants resolveWrite established.
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
func (w *writer) rowSeen(table TableID, rowID RowID) bool {
	return w.seenRows[table].Contains(rowID)
}

// rememberRow records (table, rowID) as written. put() calls it only after
// the row builder accepted the record, so failed inserts never pollute the
// set. Nil sets are created lazily: most writers touch few tables.
func (w *writer) rememberRow(table TableID, rowID RowID) {
	set := w.seenRows[table]
	if set == nil {
		set = &rowIDSet{}
		w.seenRows[table] = set
	}
	set.Insert(rowID)
}

func (w *writer) resolveSchema(table TableID, version SchemaVersion) (*codec.Schema, error) {
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
func (w *writer) checkParent(table TableID, rowID RowID, typ ChangeType) error {
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

// abort discards the snapshot. It is idempotent; abort on a committed
// writer returns ErrSnapshotCommitted. Tx.Rollback's engine side.
func (w *writer) abort() error {
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

// abortLocked is the Close path: the caller already holds writeMu.
func (w *writer) abortLocked() error {
	if w.state == writerCommitted {
		return ErrSnapshotCommitted
	}
	w.state = writerAborted
	w.store.writer.CompareAndSwap(w, nil)
	return nil
}

// flushAll flushes every builder into pending blocks.
func (w *writer) flushAll() error {
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
	slices.Sort(tables)
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
	return binary.LittleEndian.Uint64(b[:])
}
