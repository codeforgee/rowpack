package rowpack

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
// Their ratio to the caller's logical bytes is the read amplification the
// page-format refactor targets (256 KiB raw blocks → 16–64 KiB pages).
type ReadStats struct {
	ReadBytes         uint64
	DecompressedBytes uint64
}

// Stats is an approximate read-only snapshot of store statistics. It does not
// establish a transaction barrier.
type Stats struct {
	Snapshots        uint64
	Tables           uint64
	Blocks           uint64
	LogicalRows      uint64
	DataFileBytes    int64
	RawBytes         uint64
	StoredBytes      uint64
	IndexMemoryBytes uint64
	IndexMode        IndexMode  // Eager (default) or Lazy
	IndexFenceBytes  uint64     // resident Row Index Fence bytes in Lazy mode
	Cache            CacheStats // random-read decoded block cache
	ScanCache        CacheStats // bounded decoded scan window
	IndexPageCache   CacheStats // Lazy decoded Row Index Page cache
	Read             ReadStats  // cumulative physical read amplification
	Batch            BatchStats
	Recovery         RecoveryStats
}

// Stats returns a snapshot of the store's statistics.
func (s *Store) Stats() Stats {
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
	// Tables + logical rows from the latest snapshot.
	if latest := view.LatestSnapshot(); latest != nil {
		stt.Tables = uint64(len(st.schemas.bySnapshot[latest.ID]))
		for _, tid := range view.RowTables(latest.ID) {
			stt.LogicalRows += view.LogicalRowCount(latest.ID, tid)
		}
	}
	stt.IndexMemoryBytes = view.MemoryBytes()
	stt.IndexMode = s.opts.IndexMode
	stt.IndexFenceBytes = view.IndexFenceBytes()
	if sz, err := s.data.Size(); err == nil {
		stt.DataFileBytes = sz
	}
	if l := s.loader; l != nil {
		stt.Cache.CapacityBytes, stt.Cache.UsedBytes, stt.Cache.Hits, stt.Cache.Misses, stt.Cache.Evictions, stt.Cache.Loads = l.cacheStats()
		stt.ScanCache.CapacityBytes, stt.ScanCache.UsedBytes, stt.ScanCache.Hits, stt.ScanCache.Misses, stt.ScanCache.Evictions, stt.ScanCache.Loads = l.scanStats()
		stt.IndexPageCache.CapacityBytes, stt.IndexPageCache.UsedBytes, stt.IndexPageCache.Hits, stt.IndexPageCache.Misses, stt.IndexPageCache.Evictions, stt.IndexPageCache.Loads = l.indexStats()
		stt.Cache.OverheadBytes = l.cacheOverhead()
		stt.ScanCache.OverheadBytes = l.scanOverhead()
		stt.IndexPageCache.OverheadBytes = l.indexOverhead()
		io := l.readIOStats()
		stt.Read.ReadBytes = io.ReadBytes
		stt.Read.DecompressedBytes = io.DecompressedBytes
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
