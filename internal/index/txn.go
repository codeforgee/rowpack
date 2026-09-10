package index

import (
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Txn is one parsed index transaction: the per-snapshot increment of
// snapshot/block/metadata/row entries plus its footer.
type Txn struct {
	Header   fileformat.IndexTxnHeader
	Snapshot fileformat.SnapshotIndexEntry
	Metadata []fileformat.MetadataIndexEntry
	Blocks   []fileformat.BlockIndexEntry
	Rows     []fileformat.RowIndexEntry
	Footer   fileformat.IndexTxnFooter

	// Resolved layout bounds (BuildStored/BuildStoredBody only; zero for
	// parsed txns). dataStart/dataEnd feed the snapshot entry, txnStart/
	// txnEnd feed the footer; commit captures them to place the footer and
	// the outer SnapshotFooter.
	dataStart, dataEnd uint64
	txnStart, txnEnd   int64
}

// Builder assembles one index transaction for a snapshot.
type Builder struct {
	sequence uint64
	snapshot *fileformat.SnapshotIndexEntry
	metadata []fileformat.MetadataIndexEntry
	blocks   []fileformat.BlockIndexEntry
	rows     []fileformat.RowIndexEntry
	// pageCount is the number of Row Index Pages produced by buildPages
	// (single-table pages; 0 when there are no rows). header() uses it so a
	// multi-table snapshot's RowIndexPageCount matches the fence directory.
	pageCount uint32
	seen      map[[2]uint64]struct{} // (tableID, rowID) uniqueness
	// dedupRows rejects duplicate (table, row) pairs in AddRow. On by
	// default; callers that already guarantee uniqueness (the commit path
	// rejects duplicates at Insert time and View.Apply re-validates the
	// built shards) disable it to skip the per-row dedup map.
	dedupRows bool
}

// NewBuilder creates a txn builder with the next sequence number.
func NewBuilder(sequence uint64) *Builder {
	return &Builder{sequence: sequence, dedupRows: true, seen: make(map[[2]uint64]struct{})}
}

// SetRowDedup enables or disables duplicate (table, row) rejection in AddRow.
// Dedup is on by default. Disabling it drops the per-row dedup map, which
// removes ~100 B of map overhead per row from the commit peak; correctness is
// preserved by the caller's own uniqueness guarantee plus View.Apply's shard
// duplicate check. Must be called before the first AddRow.
func (b *Builder) SetRowDedup(enabled bool) { b.dedupRows = enabled }

// SetSnapshot sets the snapshot summary entry.
func (b *Builder) SetSnapshot(e fileformat.SnapshotIndexEntry) error {
	if b.snapshot != nil {
		return errors.New("rowpack: snapshot entry already set")
	}
	if e.SnapshotID == 0 {
		return errors.New("rowpack: snapshot id is zero")
	}
	if e.SnapshotType != fileformat.SnapshotFull && e.SnapshotType != fileformat.SnapshotDelta {
		return fmt.Errorf("rowpack: snapshot %d bad type %d", e.SnapshotID, e.SnapshotType)
	}
	b.snapshot = &e
	return nil
}

// AddMetadata appends a metadata entry.
func (b *Builder) AddMetadata(e fileformat.MetadataIndexEntry) error {
	if b.snapshot == nil {
		return errors.New("rowpack: set snapshot before adding entries")
	}
	if e.SnapshotID != b.snapshot.SnapshotID {
		return errors.New("rowpack: metadata entry snapshot mismatch")
	}
	b.metadata = append(b.metadata, e)
	return nil
}

// AddBlock appends a block entry.
func (b *Builder) AddBlock(e fileformat.BlockIndexEntry) error {
	if b.snapshot == nil {
		return errors.New("rowpack: set snapshot before adding entries")
	}
	if e.SnapshotID != b.snapshot.SnapshotID {
		return errors.New("rowpack: block entry snapshot mismatch")
	}
	b.blocks = append(b.blocks, e)
	return nil
}

// AddRow appends a row entry, rejecting duplicate (table, row) within the
// snapshot (v1 forbids duplicate RowKeys in one snapshot).
func (b *Builder) AddRow(e fileformat.RowIndexEntry) error {
	if b.snapshot == nil {
		return errors.New("rowpack: set snapshot before adding entries")
	}
	if e.SnapshotID != b.snapshot.SnapshotID {
		return errors.New("rowpack: row entry snapshot mismatch")
	}
	if b.dedupRows {
		key := [2]uint64{uint64(e.TableID), e.RowID}
		if _, dup := b.seen[key]; dup {
			return fmt.Errorf("rowpack: duplicate (table %d, row %d) in snapshot %d", e.TableID, e.RowID, b.snapshot.SnapshotID)
		}
		b.seen[key] = struct{}{}
	}
	b.rows = append(b.rows, e)
	return nil
}

// Counts returns the entry counts.
func (b *Builder) Counts() (meta, blocks uint32, rows uint64) {
	return uint32(len(b.metadata)), uint32(len(b.blocks)), uint64(len(b.rows))
}

// Reserve pre-allocates the metadata/block/row slices and the row-dedup map,
// so streaming Add* calls never reallocate. Call once after SetSnapshot and
// before the first Add* when the totals are known (e.g. from already-flushed
// blocks).
func (b *Builder) Reserve(meta, blocks, rows int) {
	if len(b.metadata) == 0 && b.metadata == nil {
		b.metadata = make([]fileformat.MetadataIndexEntry, 0, meta)
	}
	if len(b.blocks) == 0 && b.blocks == nil {
		b.blocks = make([]fileformat.BlockIndexEntry, 0, blocks)
	}
	if len(b.rows) == 0 && b.rows == nil {
		b.rows = make([]fileformat.RowIndexEntry, 0, rows)
	}
	if b.dedupRows && len(b.seen) == 0 {
		b.seen = make(map[[2]uint64]struct{}, rows)
	}
}

// Build serializes the complete index transaction (chunked body layout:
// chunk headers + payloads + directory) wrapped in the IndexTxnHeader and
// IndexTxnFooter. Plain stores only — encrypted writers go through
// BuildStored with a ChunkCrypto. bounds carries the snapshot's data range
// (from the entry passed to SetSnapshot) and the txn's own file offsets, which
// only fill the header/footer fields.
func (b *Builder) Build(bounds BodyBounds, dataFooterCRC uint32) ([]byte, *Txn, error) {
	body, plainCRC, txn, err := b.BuildStoredBody(nil, 0, func(int) BodyBounds { return bounds })
	if err != nil {
		return nil, nil, err
	}
	h := b.header(bounds.DataStart, bounds.DataEnd)
	f := b.footer(bounds, dataFooterCRC, plainCRC)
	out, err := AssembleTxn(h, 0, body, f)
	if err != nil {
		return nil, nil, err
	}
	txn.Header = h
	txn.Footer = f
	return out, txn, nil
}

// header assembles the IndexTxnHeader fields from the builder state.
// RowIndexPageCount is the count of sorted Row Index Pages produced by
// buildPages (single-table pages, so the count equals the fence
// directory size; 0 when there are no row entries).
func (b *Builder) header(dataSnapshotStart, dataSnapshotEnd uint64) fileformat.IndexTxnHeader {
	n := len(b.rows)
	pages := b.pageCount
	return fileformat.IndexTxnHeader{
		TxnSequence:        b.sequence,
		SnapshotID:         b.snapshot.SnapshotID,
		DataSnapshotStart:  dataSnapshotStart,
		DataSnapshotEnd:    dataSnapshotEnd,
		MetadataEntryCount: uint32(len(b.metadata)),
		BlockEntryCount:    uint32(len(b.blocks)),
		RowEntryCount:      uint64(n),
		RowIndexPageCount:  pages,
	}
}

// footer assembles the IndexTxnFooter fields from the builder state and the
// txn's resolved bounds.
func (b *Builder) footer(bounds BodyBounds, dataFooterCRC, plainCRC uint32) fileformat.IndexTxnFooter {
	return fileformat.IndexTxnFooter{
		TxnSequence:      b.sequence,
		SnapshotID:       b.snapshot.SnapshotID,
		TxnStartOffset:   uint64(bounds.TxnStart),
		TxnEndOffset:     uint64(bounds.TxnEnd),
		DataSnapshotEnd:  bounds.DataEnd,
		BodyCRC32C:       plainCRC,
		DataFooterCRC32C: dataFooterCRC,
	}
}

// BuildStored serializes the chunked index transaction with an optional
// chunk-crypto context (nil = plain). resolveBounds is called with the final
// stored body length to fix the snapshot entry's bounds (the snapshot chunk's
// stored size is content-independent, so one pass suffices); nil passes the
// entry's own values through. keyEpoch is stamped into the header reserved word
// for encrypted stores.
func (b *Builder) BuildStored(crypto *ChunkCrypto, level int, resolveBounds BoundsResolver,
	dataFooterCRC uint32, keyEpoch uint32,
) ([]byte, *Txn, error) {
	body, plainCRC, txn, err := b.BuildStoredBody(crypto, level, resolveBounds)
	if err != nil {
		return nil, nil, err
	}
	h := b.header(txn.dataStart, txn.dataEnd)
	f := b.footer(BodyBounds{DataStart: txn.dataStart, DataEnd: txn.dataEnd, TxnStart: txn.txnStart, TxnEnd: txn.txnEnd}, dataFooterCRC, plainCRC)
	out, err := AssembleTxn(h, keyEpoch, body, f)
	if err != nil {
		return nil, nil, err
	}
	txn.Header = h
	txn.Footer = f
	return out, txn, nil
}

// ParseTxn parses and validates one index transaction from data, which must
// contain exactly one txn (header + chunked body + footer). It verifies
// header/footer magic, sizes, CRCs, the chunk directory, per-chunk payload
// CRCs, and entry decoding. crypto must be non-nil iff the txn's chunks are
// encrypted; all entries are materialized into the returned Txn.
func ParseTxn(data []byte, crypto *ChunkCrypto) (*Txn, error) {
	return parseTxnChunked(data, crypto, nil)
}

// parseStream parses like ParseTxn but hands every entry to sink as it is
// decoded instead of buffering them in Txn.Rows. Row-count validation still
// runs against the header, so a sink that drops entries cannot forge a valid
// txn. Use this on the Open path to build final structures directly and skip
// the ~40 B/row intermediate slice. It is package-internal: only
// View.ApplyStreaming drives it.
func parseStream(data []byte, crypto *ChunkCrypto, sink TxnSink) (*Txn, error) {
	return parseTxnChunked(data, crypto, sink)
}

func parseTxnChunked(data []byte, crypto *ChunkCrypto, sink TxnSink) (*Txn, error) {
	if len(data) < fileformat.IndexTxnHeaderSize+fileformat.IndexTxnFooterSize {
		return nil, errors.New("rowpack: index txn too short")
	}
	var h fileformat.IndexTxnHeader
	if err := h.Unmarshal(data); err != nil {
		return nil, err
	}
	if h.BodyBytes > uint64(len(data)-fileformat.IndexTxnHeaderSize-fileformat.IndexTxnFooterSize) {
		return nil, errors.New("rowpack: index txn body exceeds input")
	}
	region := data[fileformat.IndexTxnHeaderSize : fileformat.IndexTxnHeaderSize+int(h.BodyBytes)]
	if uint64(len(region)) != h.BodyBytes {
		return nil, errors.New("rowpack: index txn body length mismatch")
	}
	ftrOff := fileformat.IndexTxnHeaderSize + len(region)
	if len(data) != ftrOff+fileformat.IndexTxnFooterSize {
		return nil, errors.New("rowpack: index txn trailing bytes")
	}
	var f fileformat.IndexTxnFooter
	if err := f.Unmarshal(data[ftrOff:]); err != nil {
		return nil, err
	}
	if f.SnapshotID != h.SnapshotID || f.TxnSequence != h.TxnSequence {
		return nil, errors.New("rowpack: index txn header/footer id mismatch")
	}
	if f.DataSnapshotEnd != h.DataSnapshotEnd {
		return nil, errors.New("rowpack: index txn data end mismatch")
	}
	// Counts are untrusted until all chunks have been checked. Use them only
	// as bounded capacity hints so a forged header cannot force an enormous
	// allocation before payload validation.
	const maxPreallocBytes = uint64(64 << 20)
	boundedCap := func(count uint64, entrySize int) int {
		limit := maxPreallocBytes / uint64(entrySize)
		if count > limit {
			count = limit
		}
		return int(count)
	}
	if sink != nil {
		if hs, ok := sink.(RowHintSink); ok {
			hs.ReserveRows(boundedCap(h.RowEntryCount, fileformat.RowIndexEntrySize))
		}
	}
	sb, err := (&bodyParser{
		region:            region,
		snapshotID:        h.SnapshotID,
		metadataCount:     boundedCap(uint64(h.MetadataEntryCount), fileformat.MetadataIndexEntrySize),
		blockCount:        boundedCap(uint64(h.BlockEntryCount), fileformat.BlockIndexEntrySize),
		rowCount:          boundedCap(h.RowEntryCount, fileformat.RowIndexEntrySize),
		rowIndexPageCount: h.RowIndexPageCount,
		crypto:            crypto,
		sink:              sink,
	}).parse()
	if err != nil {
		return nil, err
	}
	if f.BodyCRC32C != sb.plainCRC {
		return nil, errors.New("rowpack: index txn body CRC mismatch")
	}
	if !sb.hasSnap {
		return nil, errors.New("rowpack: index txn has no snapshot chunk")
	}
	if sb.snapshot.SnapshotID != h.SnapshotID {
		return nil, errors.New("rowpack: index txn snapshot id mismatch")
	}
	if sb.metaCount != h.MetadataEntryCount {
		return nil, fmt.Errorf("rowpack: index txn %d metadata entries, header says %d", sb.metaCount, h.MetadataEntryCount)
	}
	if sb.blockCount != h.BlockEntryCount {
		return nil, fmt.Errorf("rowpack: index txn %d block entries, header says %d", sb.blockCount, h.BlockEntryCount)
	}
	if sb.rowCount != h.RowEntryCount {
		return nil, fmt.Errorf("rowpack: index txn %d row entries, header says %d", sb.rowCount, h.RowEntryCount)
	}
	return &Txn{Header: h, Footer: f, Snapshot: sb.snapshot, Metadata: sb.metadata, Blocks: sb.blocks, Rows: sb.rows}, nil
}
