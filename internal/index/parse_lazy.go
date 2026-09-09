package index

import (
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// LazyTxn is the parsed extract of a lazily-opened IndexTxn: the snapshot
// summary, the metadata/block entries needed to build the view's non-row maps,
// and the per-snapshot Row Index Fence Directory. Row page payloads are NOT
// decoded — only the fence (52 B/page) becomes resident.
type LazyTxn struct {
	Header   fileformat.IndexTxnHeader
	Snapshot fileformat.SnapshotIndexEntry
	Metadata []fileformat.MetadataIndexEntry
	Blocks   []fileformat.BlockIndexEntry
	Fences   []fileformat.RowIndexFenceEntry
	Footer   fileformat.IndexTxnFooter
	// Seq is the chunk count of the txn: the chunk sequence of page i (in the
	// fence) is Seq+i, matching parseRowIndexPages' page OPEN.
	Seq uint32
}

// lazySink is the TxnSink behind ParseTxnLazy: it buffers the few metadata and
// block entries and captures the fence directory, but never receives decoded
// page rows (parseRowIndexPages short-circuits via FenceCaptureSink).
type lazySink struct {
	snapID   uint64
	snapshot fileformat.SnapshotIndexEntry
	hasSnap  bool
	metadata []fileformat.MetadataIndexEntry
	blocks   []fileformat.BlockIndexEntry
	fences   []fileformat.RowIndexFenceEntry
}

func (s *lazySink) SetSnapshot(e fileformat.SnapshotIndexEntry) error {
	if s.hasSnap {
		return fmt.Errorf("rowpack: duplicate snapshot chunk")
	}
	s.snapshot = e
	s.hasSnap = true
	return nil
}

func (s *lazySink) AddMetadata(e fileformat.MetadataIndexEntry) error {
	s.metadata = append(s.metadata, e)
	return nil
}

func (s *lazySink) AddBlock(e fileformat.BlockIndexEntry) error {
	s.blocks = append(s.blocks, e)
	return nil
}

func (s *lazySink) AddRows(batch []fileformat.RowIndexEntry) error {
	// Rows are never decoded in Lazy mode; reachable only if a sink that is not
	// a FenceCaptureSink is used, which is not the case here.
	return nil
}

func (s *lazySink) SetRowIndexFences(fences []fileformat.RowIndexFenceEntry) error {
	s.fences = fences
	return nil
}

// ParseTxnLazy parses and validates one IndexTxn, returning the fence directory
// without decoding any page payload. It validates header/footer consistency,
// chunk integrity, snapshot/metadata/block counts, and the fence cohesion, but
// deliberately skips the plaintext-body CRC check (which covers the raw page
// bytes) — integrity is assured by the stored-byte CRC verified by the caller
// plus each page's PageCRC32C / AEAD, checked on first lazy load.
func ParseTxnLazy(data []byte, crypto *ChunkCrypto) (*LazyTxn, error) {
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
	const maxPreallocBytes = uint64(64 << 20)
	boundedCap := func(count uint64, entrySize int) int {
		limit := maxPreallocBytes / uint64(entrySize)
		if count > limit {
			count = limit
		}
		return int(count)
	}
	sink := &lazySink{snapID: h.SnapshotID}
	sb, err := parseStoredBody(region, h.SnapshotID,
		boundedCap(uint64(h.MetadataEntryCount), fileformat.MetadataIndexEntrySize),
		boundedCap(uint64(h.BlockEntryCount), fileformat.BlockIndexEntrySize),
		boundedCap(h.RowEntryCount, fileformat.RowIndexEntrySize), h.RowIndexPageCount, crypto, sink)
	if err != nil {
		return nil, err
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
	if uint32(len(sink.fences)) != h.RowIndexPageCount {
		return nil, fmt.Errorf("rowpack: index txn %d fence entries, header says %d", len(sink.fences), h.RowIndexPageCount)
	}
	return &LazyTxn{
		Header:   h,
		Snapshot: sb.snapshot,
		Metadata: sink.metadata,
		Blocks:   sink.blocks,
		Fences:   sink.fences,
		Footer:   f,
		Seq:      sb.chunkCount,
	}, nil
}

// ApplyLazy returns a NEW immutable view that installs a lazily-parsed txn's
// snapshot/metadata/block entries and its Row Index Fence Directory (without
// materializing any row shard). The receiver is not modified; the new view
// shares the immutable block/metadata maps and gets a copy-on-write lazy index
// carrying both the existing fences and this snapshot's fence. source decodes
// a page on demand. The returned view's resident size is the non-row maps plus
// the fence bytes (no row bytes).
func (v *View) ApplyLazy(t *LazyTxn, maxDepth uint32, source LazySource) (*View, error) {
	if t == nil {
		return nil, fmt.Errorf("rowpack: nil lazy txn")
	}
	nv, meta, err := v.beginApply(t.Snapshot, maxDepth)
	if err != nil {
		return nil, err
	}
	if err := applyBlocks(v, nv, meta, t.Snapshot.SnapshotID, t.Blocks); err != nil {
		return nil, err
	}
	if err := applyMetadata(nv, t.Snapshot.SnapshotID, t.Metadata); err != nil {
		return nil, err
	}
	fenceBytes := uint64(len(t.Fences)) * fileformat.IndexFenceEntrySize
	// Copy-on-write the lazy index so installing the new snapshot's fence never
	// mutates a shared view (views are immutable and concurrently read).
	if nv.lazy == nil {
		nv.lazy = &lazyIndex{source: source, snapshots: make(map[uint64]*lazySnapshot)}
	} else {
		cp := make(map[uint64]*lazySnapshot, len(nv.lazy.snapshots)+1)
		for k, vv := range nv.lazy.snapshots {
			cp[k] = vv
		}
		nv.lazy = &lazyIndex{source: source, snapshots: cp, fenceBytes: nv.lazy.fenceBytes, err: nv.lazy.err}
	}
	nv.lazy.snapshots[t.Snapshot.SnapshotID] = &lazySnapshot{fences: t.Fences}
	nv.lazy.fenceBytes += fenceBytes
	// Resident memory: block/metadata map cells + fence bytes (no row shards).
	nv.memoryBytes = v.memoryBytes
	nv.memoryBytes += 64 + uint64(len(t.Metadata))*56 + uint64(len(t.Blocks))*72
	nv.memoryBytes += fenceBytes
	return nv, nil
}
