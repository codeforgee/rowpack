package rowpack

import "github.com/codeforgee/rowpack/internal/index"

// CacheStats summarizes one decoded-block cache: value bytes against the
// configured capacity, plus the estimated management overhead (LRU list and
// map nodes) which is NOT counted against the capacity but reported so the
// total resident cost stays visible.
type CacheStats struct {
	CapacityBytes uint64
	UsedBytes     uint64
	OverheadBytes uint64 // estimated LRU list/map management memory
	Hits          uint64
	Misses        uint64
	Evictions     uint64
	Loads         uint64
}

// BatchStats summarizes ReadBatch activity: how many batches were served,
// how many rows were returned, how many distinct blocks were loaded, and how
// many raw bytes were decompressed/verified. Blocks <= Rows and the raw byte
// count quantify the batch aggregation effect: the same block is loaded and
// decompressed once per batch even when many rows come from it.
type BatchStats struct {
	Calls    uint64 // ReadBatch invocations
	Rows     uint64 // rows returned across all calls
	Blocks   uint64 // distinct blocks loaded across all calls
	RawBytes uint64 // decompressed payload bytes across all calls
}

// RecoveryStats summarizes the most recent open/recovery.
type RecoveryStats struct {
	Performed        bool
	DataTailIgnored  uint64
	IndexTailIgnored uint64
	SnapshotsRebuilt uint64
}

// ReadStats reports cumulative physical read amplification since Open.
// Every block read on the store flows through one reader, so these counters
// quantify what a cold access actually pulls:
//
//	ReadBytes         header + stored bytes pulled from the file
//	DecompressedBytes validated raw payload bytes produced
//
// Their ratio to the caller's logical bytes is the read amplification of a
// whole-block read (256 KiB raw block vs the accessed page).
type ReadStats struct {
	ReadBytes         uint64 // header + stored bytes pulled from the file
	DecompressedBytes uint64 // validated raw payload bytes produced
	PageLoads         uint64 // Rows-page decompressions on demand
	PageRawBytes      uint64 // validated raw page payload bytes
	PageStoredBytes   uint64 // stored page bytes pulled from the file
}

// Stats is an approximate read-only snapshot of store statistics. It does not
// establish a transaction barrier.
type Stats struct {
	Snapshots         uint64
	Tables            uint64
	Blocks            uint64
	LogicalRows       uint64
	DataFileBytes     int64
	RawBytes          uint64
	StoredBytes       uint64
	IndexMemoryBytes  uint64
	OversizedRowPages uint64     // pages holding a single record larger than the page target
	Cache             CacheStats // random-read decoded block cache
	ScanCache         CacheStats // bounded decoded scan window
	Read              ReadStats  // cumulative physical read amplification
	Batch             BatchStats
	Recovery          RecoveryStats
}

// IndexMemoryBreakdown returns the diagnostic split of the residential index
// estimate: how much of IndexMemoryBytes is columnar slices, how much is
// per-shard overhead, and how many entries those slices actually hold. Stats
// reports the total; this says what the total is made of.
func (s *Store) IndexMemoryBreakdown() index.MemoryBreakdown {
	st, err := s.captureState()
	if err != nil {
		return index.MemoryBreakdown{}
	}
	return st.view.MemoryBreakdown()
}

// IndexPackProfile reports how far the row-index columns would compress under
// frame-of-reference bit packing. Purely a measurement: it tells you the
// headroom before you commit to changing the layout. See index.PackProfile.
func (s *Store) IndexPackProfile() index.PackProfile {
	st, err := s.captureState()
	if err != nil {
		return index.PackProfile{}
	}
	return st.view.PackProfile()
}

// Stats returns a snapshot of the store's statistics.
func (s *Store) Stats() Stats {
	s.readMu.RLock()
	defer s.readMu.RUnlock()
	st, err := s.captureState()
	var stt Stats
	if err != nil {
		return stt
	}
	view := st.view
	stt.Snapshots = uint64(len(view.Snapshots()))
	stt.Blocks = uint64(len(view.Blocks()))
	for _, b := range view.Blocks() {
		stt.RawBytes += uint64(b.RawSize)
		stt.StoredBytes += uint64(b.StoredSize)
	}
	// Tables + logical rows at the latest snapshot. The table set is the catalog
	// visible at that snapshot (resolved along the parent chain by
	// deriveTables), NOT the tables whose rows this transaction wrote itself:
	// a DELTA that touches only one of five tables, or an empty checkpoint
	// DELTA, still carries every ancestor row, and summing only the touched
	// tables understates the store's logical row count (0 for an empty tail
	// snapshot).
	if latest := view.LatestSnapshot(); latest != nil {
		stt.Tables = uint64(len(st.schemas.bySnapshot[latest.ID]))
		for tid := range st.schemas.bySnapshot[latest.ID] {
			stt.LogicalRows += view.LogicalRowCount(latest.ID, tid)
		}
	}
	stt.IndexMemoryBytes = view.MemoryBytes()
	stt.OversizedRowPages = s.oversizedPages.Load()
	stt.DataFileBytes = s.data.Size()
	if l := s.loader; l != nil {
		stt.Cache.CapacityBytes, stt.Cache.UsedBytes, stt.Cache.Hits, stt.Cache.Misses, stt.Cache.Evictions, stt.Cache.Loads = l.cacheStats()
		stt.ScanCache.CapacityBytes, stt.ScanCache.UsedBytes, stt.ScanCache.Hits, stt.ScanCache.Misses, stt.ScanCache.Evictions, stt.ScanCache.Loads = l.scanStats()
		stt.Cache.OverheadBytes = l.cache.OverheadBytes()
		stt.ScanCache.OverheadBytes = l.scan.OverheadBytes()
		io := l.reader.Stats()
		stt.Read.ReadBytes = io.ReadBytes
		stt.Read.DecompressedBytes = io.DecompressedBytes
		stt.Read.PageLoads = io.PageLoads
		stt.Read.PageRawBytes = io.PageRawBytes
		stt.Read.PageStoredBytes = io.PageStoredBytes
	}
	stt.Batch.Calls = s.batchCalls.Load()
	stt.Batch.Rows = s.batchRows.Load()
	stt.Batch.Blocks = s.batchBlocks.Load()
	stt.Batch.RawBytes = s.batchRawBytes.Load()
	if v := s.recoveryStats.Load(); v != nil {
		r := v.(recoveryReport)
		stt.Recovery.Performed = r.performed
		stt.Recovery.DataTailIgnored = r.dataTailIgnored
		stt.Recovery.IndexTailIgnored = r.indexTailIgnored
		stt.Recovery.SnapshotsRebuilt = r.snapshotsRebuilt
	}
	return stt
}
