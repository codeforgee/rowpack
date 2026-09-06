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
}

// Builder assembles one index transaction for a snapshot.
type Builder struct {
	sequence uint64
	snapshot *fileformat.SnapshotIndexEntry
	metadata []fileformat.MetadataIndexEntry
	blocks   []fileformat.BlockIndexEntry
	rows     []fileformat.RowIndexEntry
	seen     map[[2]uint64]struct{} // (tableID, rowID) uniqueness
}

// NewBuilder creates a txn builder with the next sequence number.
func NewBuilder(sequence uint64) *Builder {
	return &Builder{sequence: sequence, seen: make(map[[2]uint64]struct{})}
}

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
	key := [2]uint64{uint64(e.TableID), e.RowID}
	if _, dup := b.seen[key]; dup {
		return fmt.Errorf("rowpack: duplicate (table %d, row %d) in snapshot %d", e.TableID, e.RowID, b.snapshot.SnapshotID)
	}
	b.seen[key] = struct{}{}
	b.rows = append(b.rows, e)
	return nil
}

// Counts returns the entry counts.
func (b *Builder) Counts() (meta, blocks uint32, rows uint64) {
	return uint32(len(b.metadata)), uint32(len(b.blocks)), uint64(len(b.rows))
}

// Build serializes the complete index transaction bytes for the .rpi file.
// dataSnapshotStart/End locate the snapshot in the .rpk file; dataFooterCRC is
// the data SnapshotFooter's FooterCRC32C used for cross-file verification.
// The returned bytes start with the IndexTxnHeader and end after the footer
// (caller appends padding).
func (b *Builder) Build(dataSnapshotStart, dataSnapshotEnd uint64, dataFooterCRC uint32, txnStart, txnEnd int64) ([]byte, *Txn, error) {
	if b.snapshot == nil {
		return nil, nil, errors.New("rowpack: no snapshot entry to build")
	}
	var snapshotEntry [fileformat.SnapshotIndexEntrySize]byte
	if err := b.snapshot.MarshalTo(snapshotEntry[:]); err != nil {
		return nil, nil, err
	}
	// Pre-allocate the body: counts are known, so append never reallocates.
	body := make([]byte, 0, fileformat.SnapshotIndexEntrySize+
		len(b.metadata)*fileformat.MetadataIndexEntrySize+
		len(b.blocks)*fileformat.BlockIndexEntrySize+
		len(b.rows)*fileformat.RowIndexEntrySize)
	body = append(body, snapshotEntry[:]...)
	for i := range b.metadata {
		var e [fileformat.MetadataIndexEntrySize]byte
		if err := b.metadata[i].MarshalTo(e[:]); err != nil {
			return nil, nil, err
		}
		body = append(body, e[:]...)
	}
	for i := range b.blocks {
		var e [fileformat.BlockIndexEntrySize]byte
		if err := b.blocks[i].MarshalTo(e[:]); err != nil {
			return nil, nil, err
		}
		body = append(body, e[:]...)
	}
	for i := range b.rows {
		var e [fileformat.RowIndexEntrySize]byte
		if err := b.rows[i].MarshalTo(e[:]); err != nil {
			return nil, nil, err
		}
		body = append(body, e[:]...)
	}
	bodyCRC := fileformat.CRC32C(body)

	h := fileformat.IndexTxnHeader{
		TxnSequence:        b.sequence,
		SnapshotID:         b.snapshot.SnapshotID,
		DataSnapshotStart:  dataSnapshotStart,
		DataSnapshotEnd:    dataSnapshotEnd,
		MetadataEntryCount: uint32(len(b.metadata)),
		BlockEntryCount:    uint32(len(b.blocks)),
		RowEntryCount:      uint64(len(b.rows)),
		BodyBytes:          uint64(len(body)),
	}
	var hdr [fileformat.IndexTxnHeaderSize]byte
	if err := h.MarshalTo(hdr[:]); err != nil {
		return nil, nil, err
	}
	f := fileformat.IndexTxnFooter{
		TxnSequence:      b.sequence,
		SnapshotID:       b.snapshot.SnapshotID,
		TxnStartOffset:   uint64(txnStart),
		TxnEndOffset:     uint64(txnEnd),
		DataSnapshotEnd:  dataSnapshotEnd,
		BodyCRC32C:       bodyCRC,
		DataFooterCRC32C: dataFooterCRC,
	}
	var ftr [fileformat.IndexTxnFooterSize]byte
	if err := f.MarshalTo(ftr[:]); err != nil {
		return nil, nil, err
	}
	out := make([]byte, 0, len(hdr)+len(body)+len(ftr))
	out = append(out, hdr[:]...)
	out = append(out, body...)
	out = append(out, ftr[:]...)
	txn := &Txn{Header: h, Snapshot: *b.snapshot, Footer: f}
	txn.Metadata = b.metadata
	txn.Blocks = b.blocks
	txn.Rows = b.rows
	return out, txn, nil
}

// ParseTxn parses and validates one index transaction from data, which must
// contain exactly one txn (header + body + footer). It verifies header/footer
// magic, sizes, CRCs, body CRC, and entry CRCs.
func ParseTxn(data []byte) (*Txn, error) {
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
	body := data[fileformat.IndexTxnHeaderSize : fileformat.IndexTxnHeaderSize+int(h.BodyBytes)]
	if len(body) != int(h.BodyBytes) {
		return nil, errors.New("rowpack: index txn body length mismatch")
	}
	ftrOff := fileformat.IndexTxnHeaderSize + len(body)
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
	if f.BodyCRC32C != fileformat.CRC32C(body) {
		return nil, errors.New("rowpack: index txn body CRC mismatch")
	}

	t := &Txn{Header: h, Footer: f}
	pos := 0
	// Snapshot entry.
	se := fileformat.SnapshotIndexEntry{}
	if err := se.Unmarshal(body[pos:]); err != nil {
		return nil, fmt.Errorf("rowpack: index txn snapshot entry: %w", err)
	}
	pos += fileformat.SnapshotIndexEntrySize
	if se.SnapshotID != h.SnapshotID {
		return nil, errors.New("rowpack: index txn snapshot id mismatch")
	}
	t.Snapshot = se

	need := func(n int) ([]byte, error) {
		if pos+n > len(body) {
			return nil, errors.New("rowpack: index txn entries exceed body")
		}
		s := body[pos : pos+n]
		pos += n
		return s, nil
	}
	for i := uint32(0); i < h.MetadataEntryCount; i++ {
		s, err := need(fileformat.MetadataIndexEntrySize)
		if err != nil {
			return nil, err
		}
		var e fileformat.MetadataIndexEntry
		if err := e.Unmarshal(s); err != nil {
			return nil, fmt.Errorf("rowpack: metadata entry %d: %w", i, err)
		}
		t.Metadata = append(t.Metadata, e)
	}
	for i := uint32(0); i < h.BlockEntryCount; i++ {
		s, err := need(fileformat.BlockIndexEntrySize)
		if err != nil {
			return nil, err
		}
		var e fileformat.BlockIndexEntry
		if err := e.Unmarshal(s); err != nil {
			return nil, fmt.Errorf("rowpack: block entry %d: %w", i, err)
		}
		t.Blocks = append(t.Blocks, e)
	}
	for i := uint64(0); i < h.RowEntryCount; i++ {
		s, err := need(fileformat.RowIndexEntrySize)
		if err != nil {
			return nil, err
		}
		var e fileformat.RowIndexEntry
		if err := e.Unmarshal(s); err != nil {
			return nil, fmt.Errorf("rowpack: row entry %d: %w", i, err)
		}
		t.Rows = append(t.Rows, e)
	}
	if pos != len(body) {
		return nil, errors.New("rowpack: index txn body has trailing bytes")
	}
	return t, nil
}
