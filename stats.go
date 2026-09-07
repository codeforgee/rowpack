package rowpack

import "time"

// CacheStats summarizes block cache activity.
type CacheStats struct {
	CapacityBytes uint64
	UsedBytes     uint64
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

// Stats is an approximate read-only snapshot of store statistics. It does not
// establish a transaction barrier.
type Stats struct {
	Snapshots        uint64
	Tables           uint64
	Blocks           uint64
	LogicalRows      uint64
	DataFileBytes    int64
	IndexFileBytes   int64
	RawBytes         uint64
	StoredBytes      uint64
	IndexMemoryBytes uint64
	Cache            CacheStats
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
	if sz, err := s.data.Size(); err == nil {
		stt.DataFileBytes = sz
	}
	if sz, err := s.index.Size(); err == nil {
		stt.IndexFileBytes = sz
	}
	if l := s.loader; l != nil {
		stt.Cache.CapacityBytes, stt.Cache.UsedBytes, stt.Cache.Hits, stt.Cache.Misses, stt.Cache.Evictions, stt.Cache.Loads = l.cacheStats()
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

var _ = time.Second
