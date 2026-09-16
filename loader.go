package rowpack

import (
	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/cache"
)

// blockLoader loads validated blocks through the LRU with concurrent miss
// merging (singleflight). Only CRC-validated blocks enter the cache; CRC
// failures are never cached. Blocks larger than the cache capacity are
// readable but not cached.
//
// Two caches with separate budgets serve different access patterns:
//
//   - cache: the decoded random-read hot set (Get / ReadBatch). Its contents
//     are never displaced by scans.
//   - scan: a bounded decoded window for streaming reads. Blocks promoted
//     here are reused across scan iterations (small working sets hit fully);
//     once the window is full, further scan blocks stream through the
//     scratch pool, never allocating and never evicting the hot set.
//
// Every block read on the store goes through this type (Get/Scan/batch/scan
// verify/recovery rebuild), so consecutive decode failures are normalized
// into a structured CorruptionError before they escape to public callers.
type blockLoader struct {
	reader *block.Reader
	file   string // label for CorruptionError.File (the store data path)
	cache  *cache.LRU
	scan   *cache.LRU
	sf     cache.Group
}

// scanBudgetFor splits the total cache budget between the scan window and
// the random-read cache. The scan window gets half of the total, capped at
// 64 MiB so huge explicit cache sizes do not let scans squat on the
// random-read hot set. Unlike the historical heuristic there is no 1 MiB
// floor: DataCache + ScanWindow must never exceed the total (Options.CacheBytes
// is a hard budget, GO_API_DESIGN_V1.md §2). Large scan sets
// (deep chains) fit the window and are reused across iterations; very large
// scans fill it and then stream through the pool without allocating.
func scanBudgetFor(cacheBytes int64) int64 {
	b := cacheBytes / 2
	if b > 64<<20 {
		return 64 << 20
	}
	return b
}

func cacheBudget(total, scan int64) (dataCap, scanCap int64) {
	if total <= 0 {
		return 0, 0
	}
	if scan < 0 {
		scan = 0
	} else if scan == 0 {
		scan = scanBudgetFor(total)
	}
	if scan > total {
		scan = total
	}
	return total - scan, scan
}

func newLoader(reader *block.Reader, file string, cacheBytes, scanCacheBytes int64) *blockLoader {
	dataCap, scanCap := cacheBudget(cacheBytes, scanCacheBytes)
	var lru, scn *cache.LRU
	if dataCap > 0 {
		lru = cache.NewLRU(dataCap)
	}
	if scanCap > 0 {
		scn = cache.NewLRU(scanCap)
	}
	return &blockLoader{reader: reader, file: file, cache: lru, scan: scn}
}

// readError normalizes a block read/decode failure into a structured
// CorruptionError so public read paths can match ErrCorruptData with
// errors.Is, while still unwrapping to the underlying Cause (e.g.
// ErrAuthFailed for a failed AEAD authentication). Failures that already
// carry a corruption or auth sentinel are returned unchanged so Verify and
// the recovery rebuild never double-wrap.
func (l *blockLoader) readError(offset int64, blockID uint64, err error) error {
	return corruptError(l.file, offset, 0, 0, blockID, err)
}

// Load returns the validated block at offset with the given block ID, serving
// from the (random-read) cache when possible.
func (l *blockLoader) Load(offset int64, blockID uint64) (*block.Block, error) {
	if l.cache == nil {
		return l.reader.ReadAtBlock(offset)
	}
	if v, ok := l.cache.Get(blockID); ok {
		return v.(*block.Block), nil
	}
	v, err := l.sf.Do(blockID, func() (any, error) {
		l.cache.NoteLoad()
		blk, err := l.reader.ReadAtBlock(offset)
		if err != nil {
			return nil, l.readError(offset, blockID, err)
		}
		// Only validated blocks (CRC passed inside ReadAtBlock) are cached.
		l.cache.Put(blockID, int64(len(blk.Raw)), blk)
		return blk, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*block.Block), nil
}

// LoadRows loads the validated Rows Block container for a block through the
// random-read cache with singleflight miss merging. Only
// CRC/geometry-validated containers are cached.
func (l *blockLoader) LoadRows(offset int64, blockID uint64) (*block.RowsContainer, error) {
	if l.cache == nil {
		return l.reader.ReadRowsDir(offset)
	}
	if v, ok := l.cache.Get(blockID); ok {
		if rc, ok := v.(*block.RowsContainer); ok {
			return rc, nil
		}
	}
	v, err := l.sf.Do(blockID, func() (any, error) {
		l.cache.NoteLoad()
		rc, err := l.reader.ReadRowsDir(offset)
		if err != nil {
			return nil, l.readError(offset, blockID, err)
		}
		rc.SetCacheAccounting(func(size int64) { l.cache.Put(blockID, size, rc) })
		l.cache.Put(blockID, rc.RetainedLen(), rc)
		return rc, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*block.RowsContainer), nil
}

// LoadScanRows serves streaming reads (Scan / ScanBlocks) of Rows page
// containers. Lookup order is the random-read cache, then the scan window; a
// miss is read once and — when the container fits the window — promoted into
// it so a repeating scan reuses it without evicting hot random pages. An
// uncached container owns its buffer and is GC-reclaimed after use (there is
// no scratch to release, unlike a decompressed block).
func (l *blockLoader) LoadScanRows(offset int64, blockID uint64) (*block.RowsContainer, error) {
	if l.cache != nil {
		if v, ok := l.cache.Get(blockID); ok {
			if rc, ok := v.(*block.RowsContainer); ok {
				return rc, nil
			}
		}
		if v, ok := l.scan.Get(blockID); ok {
			if rc, ok := v.(*block.RowsContainer); ok {
				return rc, nil
			}
		}
	}
	rc, err := l.reader.ReadRowsDir(offset)
	if err != nil {
		return nil, l.readError(offset, blockID, err)
	}
	// RetainedLen is the actual initial resident footprint. StoredLen can be
	// only the header+directory for lazy containers and is not a safe budget
	// admission metric.
	if l.scan != nil && uint64(rc.RetainedLen()) <= l.scan.Remaining() {
		rc.SetCacheAccounting(func(size int64) { l.scan.Put(blockID, size, rc) })
		l.scan.Put(blockID, rc.RetainedLen(), rc)
	}
	return rc, nil
}

// cacheStats returns the cache counters (nil-safe).
func (l *blockLoader) cacheStats() (capBytes, used, hits, misses, evictions, loads uint64) {
	if l.cache == nil {
		return 0, 0, 0, 0, 0, 0
	}
	return l.cache.CapacityBytes(), l.cache.UsedBytes(), l.cache.Hits(), l.cache.Misses(), l.cache.Evictions(), l.cache.Loads()
}

// scanStats returns the scan-window counters (nil-safe).
func (l *blockLoader) scanStats() (capBytes, used, hits, misses, evictions, loads uint64) {
	if l.scan == nil {
		return 0, 0, 0, 0, 0, 0
	}
	return l.scan.CapacityBytes(), l.scan.UsedBytes(), l.scan.Hits(), l.scan.Misses(), l.scan.Evictions(), l.scan.Loads()
}
