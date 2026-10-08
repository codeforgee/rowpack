package rowpack

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/codeforgee/rowpack/internal/fault"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/seal"
)

// commit.go is the durable commit pipeline: it writes one buffered snapshot
// transaction to the single data file in the frozen on-disk order —
// SnapshotHeader -> Blocks -> IndexTxn -> SnapshotFooter -> one Sync — and
// then publishes the new index view atomically (BINARY_FORMAT_V1 §8).
//
// The pipeline is a sequence of phases threaded through a commitLayout:
//
//	validate   flushAll, checkSchemaCoverage (before the first byte)
//	writeBlocks     header + seal + append every pending block
//	writeIndexTxn   build, seal and append the IndexTxn
//	writeFooter     the commit authority binding all offsets and CRCs
//	sync + publish  one fsync, then the atomic view swap
//
// Failures before the sync are a known torn commit (plain errors); failures
// at or after it are outcome-unknown (CommitError with Unknown set), which
// latches the store's must-reopen state.

// commitLayout carries the byte-level bookkeeping of one commit as it moves
// through the pipeline phases: file offsets resolved in earlier phases feed
// the footer, and the accumulated counters fill both the footer and the
// returned SnapshotInfo.
type commitLayout struct {
	snapStart int64  // SnapshotHeader offset
	blocksEnd int64  // first byte after the last block payload
	txnStart  int64  // IndexTxn offset
	txnEnd    int64  // first byte after the IndexTxn
	snapEnd   int64  // first byte after the SnapshotFooter (commit extent)
	stored    []byte // serialized IndexTxn bytes (the footer CRCs them)

	sh             format.SnapshotHeader
	blockCount     uint32
	metaBlockCount uint32
	rawBytes       uint64
	blockCRCs      []byte // concatenation of the blocks' RawCRC32C header fields
}

// commit persists the snapshot, atomically publishes it to readers and
// returns the new snapshot's ID (the read path's only credential).
func (w *writer) commit(ctx context.Context) (SnapshotID, error) {
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

// commitLocked runs the pipeline under writeMu. Phases follow the on-disk
// order; the fault injection points mark their boundaries.
func (w *writer) commitLocked() (SnapshotInfo, error) {
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
	fault.Check("commit.header.before")

	var cp commitLayout
	if err := w.writeBlocks(&cp); err != nil {
		return SnapshotInfo{}, err
	}
	txn, err := w.writeIndexTxn(&cp)
	if err != nil {
		return SnapshotInfo{}, err
	}
	if err := w.writeFooter(&cp); err != nil {
		return SnapshotInfo{}, err
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
	return w.publish(&cp, txn)
}

// writeBlocks assigns block IDs, writes the snapshot header, then seals and
// appends every pending block.
func (w *writer) writeBlocks(cp *commitLayout) error {
	// Assign block IDs first so the snapshot header can record FirstBlockID.
	for _, blk := range w.pending {
		blk.header.BlockID = w.store.lastBlockID.Add(1)
	}
	// Single-file commit order: SnapshotHeader -> Blocks -> IndexTxn ->
	// SnapshotFooter, then exactly one Sync (BINARY_FORMAT_V1 §8).
	cp.snapStart = w.store.data.Offset()
	sh, err := w.writeHeader()
	if err != nil {
		return err
	}
	cp.sh = sh

	fault.Check("commit.block.before")
	for _, blk := range w.pending {
		// Encrypt the stored payload after BlockID assignment and before the
		// header is marshalled: the AAD binds the final header fields. The
		// ciphertext length is known up front (plaintext + tag), and the AAD's
		// StoredSize is set to that exact value so read-time verification is
		// self-consistent. Only the payload and header change; RawSize and
		// RawCRC32C keep describing the uncompressed plaintext.
		if err := w.sealPendingBlock(blk); err != nil {
			return err
		}
		hb, err := w.writePendingBlock(blk)
		if err != nil {
			return err
		}
		cp.blockCount++
		if blk.header.BlockKind == format.BlockKindMetadata {
			cp.metaBlockCount++
		}
		cp.rawBytes += uint64(blk.header.RawSize)
		cp.blockCRCs = append(cp.blockCRCs, hb[52:56]...)
	}
	// Note: block CRCs are computed from the header CRC fields (offset 52).
	cp.blocksEnd = w.store.data.Offset()
	return nil
}

// writeIndexTxn builds the snapshot's IndexTxn from the flushed blocks and
// appends it. Its stored length depends on compression results, so the
// txn/footer offsets are resolved through the BuildStored bounds callback
// once the body length is known (the snapshot chunk's stored size is fixed
// at 72B, so one pass suffices). Returns the parsed txn for the publish
// phase.
func (w *writer) writeIndexTxn(cp *commitLayout) (*index.Txn, error) {
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
		BlockCount:       cp.blockCount,
		RowRecordCount:   w.rowRecordCount,
		DataStart:        uint64(cp.snapStart),
		DataEnd:          uint64(cp.snapStart), // resolved by BuildStored
		CreatedUnixNano:  w.created,
	}
	if err := txnBuilder.SetSnapshot(snapEntry); err != nil {
		return nil, err
	}
	if err := w.addBlocksToTxn(txnBuilder); err != nil {
		return nil, err
	}
	fault.Check("commit.txn.before")

	var txnStart, txnEnd, snapEnd int64
	stored, txn, err := txnBuilder.BuildStored(w.indexCrypto(), w.store.opts.CompressionLevel,
		func(bodyLen int) index.BodyBounds {
			l := int64(format.IndexTxnHeaderSize + bodyLen + format.IndexTxnFooterSize)
			ts := cp.blocksEnd
			te := ts + l
			txnStart, txnEnd, snapEnd = ts, te, te+format.SnapshotFooterSize
			return index.BodyBounds{DataStart: uint64(cp.snapStart), DataEnd: uint64(snapEnd), TxnStart: ts, TxnEnd: te}
		}, 0, 0)
	if err != nil {
		return nil, err
	}
	cp.txnStart, cp.txnEnd, cp.snapEnd = txnStart, txnEnd, snapEnd
	cp.stored = stored
	if _, err := w.store.data.Append(stored); err != nil {
		return nil, err
	}
	return txn, nil
}

// indexCrypto builds the index-chunk sealing policy for an encrypted store
// (nil for a plain one). Each chunk is sealed under its own HMAC-derived
// nonce (the NonceIndex 96-bit space is full) and AAD bound to store/txn/
// chunk identity and lengths. Header, chunk headers and the directory stay
// plaintext — the scanner walks the txn by magic + BodyBytes + footer magic
// without a key — while every payload is authenticated independently.
func (w *writer) indexCrypto() *index.ChunkCrypto {
	c := w.store.encCipher
	if c == nil {
		return nil
	}
	const epoch = uint32(0)
	txnSeq := w.store.txnSeq.Load()
	uuid := &w.store.uuid
	return &index.ChunkCrypto{
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

// writeFooter writes the snapshot footer: the commit authority that binds
// the snapshot's byte extent, block counters and CRCs.
func (w *writer) writeFooter(cp *commitLayout) error {
	fault.Check("commit.footer.before")
	ftr := format.SnapshotFooter{
		SnapshotType:         format.SnapshotType(w.typ),
		SnapshotID:           w.id,
		ParentSnapshotID:     w.parent,
		PreviousFooterOffset: w.store.lastFooterOffset,
		SnapshotStartOffset:  uint64(cp.snapStart),
		BlocksStartOffset:    uint64(cp.snapStart) + format.SnapshotHeaderSize,
		BlocksEndOffset:      uint64(cp.blocksEnd),
		IndexTxnStartOffset:  uint64(cp.txnStart),
		IndexTxnEndOffset:    uint64(cp.txnEnd),
		SnapshotEndOffset:    uint64(cp.snapEnd),
		FirstBlockID:         cp.sh.FirstBlockID,
		BlockCount:           cp.blockCount,
		MetadataBlockCount:   cp.metaBlockCount,
		RowRecordCount:       w.rowRecordCount,
		RawBytes:             cp.rawBytes,
		StoredBytes:          uint64(cp.snapEnd - cp.snapStart),
		BlocksCRC32C:         format.CRC32C(cp.blockCRCs),
		IndexTxnCRC32C:       format.CRC32C(cp.stored),
	}
	var fb [format.SnapshotFooterSize]byte
	_ = ftr.MarshalTo(fb[:]) // exact-size buffer: cannot fail
	if _, err := w.store.data.Append(fb[:]); err != nil {
		return err
	}
	fault.Check("commit.footer.after")
	if w.store.data.Offset() != cp.snapEnd {
		return fmt.Errorf("rowpack: snapshot end %d != %d", w.store.data.Offset(), cp.snapEnd)
	}
	return nil
}

// publish validates and installs the committed txn into a new immutable view
// and swaps it into the store. It runs only after the durability sync, so
// every failure is outcome-unknown.
func (w *writer) publish(cp *commitLayout, txn *index.Txn) (SnapshotInfo, error) {
	st := w.store.state.Load()
	newView, err := st.view.Apply(txn, w.store.opts.Limits.MaxSnapshotDepth)
	if err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: true, Err: err}
	}
	newSchemas, err := w.buildSchemas(newView)
	if err != nil {
		return SnapshotInfo{}, &CommitError{SnapshotID: w.id, Unknown: true, Err: err}
	}
	fault.Check("commit.publish.before")
	w.store.state.Store(&publishedState{view: newView, schemas: newSchemas})
	w.store.maxTableID.Store(w.nextTableID - 1)
	w.store.maxObjectID.Store(w.maxObject)
	w.store.lastFooterOffset = uint64(cp.txnEnd)
	fault.Check("commit.publish.after")
	w.state = writerCommitted
	w.store.writer.CompareAndSwap(w, nil)

	return SnapshotInfo{
		ID:          w.id,
		Type:        w.typ,
		Parent:      w.parent,
		CreatedAt:   time.Unix(0, w.created).UTC(),
		BlockCount:  cp.blockCount,
		ChangeCount: w.rowRecordCount,
		RawBytes:    cp.rawBytes,
		StoredBytes: uint64(cp.snapEnd - cp.snapStart),
	}, nil
}

// writeHeader assigns the snapshot header fields and appends it before
// any blocks. Keeping this boundary explicit makes the on-disk commit order
// easier to audit.
func (w *writer) writeHeader() (format.SnapshotHeader, error) {
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
func (w *writer) sealPendingBlock(blk *pendingBlock) error {
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

func (w *writer) addBlocksToTxn(builder *index.Builder) error {
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

func (w *writer) writePendingBlock(blk *pendingBlock) ([format.BlockHeaderSize]byte, error) {
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
func (w *writer) checkSchemaCoverage() error {
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

// addressOf returns the address a table was resolved under in this
// transaction, or "" (every written table is cached in tableIDs by
// tableForWrite first).
func (w *writer) addressOf(tid TableID) string {
	for addr, id := range w.tableIDs {
		if id == tid {
			return addr
		}
	}
	return ""
}

// buildSchemas derives the schema index for the new snapshot only (the
// parent snapshots' schemas are reused from the old index).
func (w *writer) buildSchemas(newView *index.View) (*schemaIndex, error) {
	base := w.store.state.Load()
	si := newSchemaIndex()
	if base != nil && base.schemas != nil {
		maps.Copy(si.bySnapshot, base.schemas.bySnapshot)
		maps.Copy(si.byAddress, base.schemas.byAddress)

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
