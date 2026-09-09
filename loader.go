package rowpack

import (
	"errors"

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
	index  *cache.LRU // decoded Row Index Page cache (Lazy mode only; nil in Eager)
	sf     cache.Group
}

// scanBudgetFor splits the total cache budget between the scan window and
// the random-read cache. The scan window gets half of the total, capped at
// 64 MiB so huge explicit cache sizes do not let scans squat on the
// random-read hot set. Unlike the historical heuristic there is no 1 MiB
// floor: DataCache + ScanWindow must never exceed the total (Options.CacheBytes
// is a hard budget, FILE_FORMAT_REFACTOR_PLAN.md §8.1). Large scan sets
// (deep chains) fit the window and are reused across iterations; very large
// scans fill it and then stream through the pool without allocating.
func scanBudgetFor(cacheBytes int64) int64 {
	b := cacheBytes / 2
	if b > 64<<20 {
		return 64 << 20
	}
	return b
}

// splitCacheBudget resolves the (data, scan) pair for a store. total <= 0
// disables caching entirely; scan < 0 keeps the data cache but disables the
// scan window; scan == 0 selects the default split. The two capacities always
// sum to exactly total.
func splitCacheBudget(total, scan int64) (dataCap, scanCap int64) {
	if total <= 0 {
		return 0, 0
	}
	if scan < 0 {
		scan = 0
	} else if scan == 0 {
		scan = scanBudgetFor(total)
	}
	return total - scan, scan
}

// indexBudgetFor returns the default IndexPageCache budget for lazy stores:
// a quarter of the total (capped at 32 MiB) so index pages get a fair slice
// while data blocks keep the majority. Never exceeds total.
func indexBudgetFor(cacheBytes int64) int64 {
	b := cacheBytes / 4
	if b > 32<<20 {
		return 32 << 20
	}
	return b
}

// splitCacheBudget3 resolves the (data, scan, index) triple for a store.
// total <= 0 disables all caching; scan < 0 disables the scan window;
// index < 0 disables the index page cache; index == 0 selects the default
// index split. The three capacities always sum to exactly total when total > 0.
func splitCacheBudget3(total, scan, index int64) (dataCap, scanCap, idxCap int64) {
	if total <= 0 {
		return 0, 0, 0
	}
	if index < 0 {
		idxCap = 0
	} else if index == 0 {
		idxCap = indexBudgetFor(total)
	} else {
		idxCap = index
	}
	if idxCap > total {
		idxCap = total
	}
	rest := total - idxCap
	if scan < 0 {
		scan = 0
	} else if scan == 0 {
		scan = scanBudgetFor(rest)
	}
	if scan > rest {
		scan = rest
	}
	return rest - scan, scan, idxCap
}

func newBlockLoader(reader *block.Reader, file string, cacheBytes, scanCacheBytes, indexCacheBytes int64) *blockLoader {
	dataCap, scanCap, idxCap := splitCacheBudget3(cacheBytes, scanCacheBytes, indexCacheBytes)
	var lru, scn, idx *cache.LRU
	if dataCap > 0 {
		lru = cache.NewLRU(dataCap)
	}
	if scanCap > 0 {
		scn = cache.NewLRU(scanCap)
	}
	if idxCap > 0 {
		idx = cache.NewLRU(idxCap)
	}
	return &blockLoader{reader: reader, file: file, cache: lru, scan: scn, index: idx}
}

// blockReadError normalizes a block read/decode failure into a structured
// CorruptionError so public read paths can match ErrCorruptData with
// errors.Is, while still unwrapping to the underlying Cause (e.g.
// ErrAuthFailed for a failed AEAD authentication). Failures that already
// carry a corruption or auth sentinel are returned unchanged so Verify and
// the recovery rebuild never double-wrap.
func (l *blockLoader) blockReadError(offset int64, blockID uint64, err error) error {
	if errors.Is(err, ErrCorruptData) || errors.Is(err, ErrAuthFailed) {
		return err
	}
	return &CorruptionError{
		File:    l.file,
		Offset:  offset,
		BlockID: blockID,
		Kind:    ErrCorruptData,
		Cause:   err,
		Reason:  err.Error(),
	}
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
			return nil, l.blockReadError(offset, blockID, err)
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

// loadRowsContext reads the validated Rows Block page container for a block.
// It reads only the block header + container header + page directory (a lazy
// container that reads individual pages on demand), so a cold single-row read
// pulls one page — for plain and (per-page-encrypted) encrypted stores alike:
// the page directory is plaintext, and each page is OPENed and decompressed
// only when accessed.
func (l *blockLoader) loadRowsContext(offset int64) (*block.RowsContainer, error) {
	return l.reader.ReadRowsDir(offset)
}

// LoadRows loads the validated Rows Block container for a block through the
// random-read cache with singleflight miss merging. Only
// CRC/geometry-validated containers are cached.
func (l *blockLoader) LoadRows(offset int64, blockID uint64) (*block.RowsContainer, error) {
	if l.cache == nil {
		return l.loadRowsContext(offset)
	}
	if v, ok := l.cache.Get(blockID); ok {
		if rc, ok := v.(*block.RowsContainer); ok {
			return rc, nil
		}
	}
	v, err := l.sf.Do(blockID, func() (any, error) {
		l.cache.NoteLoad()
		rc, err := l.loadRowsContext(offset)
		if err != nil {
			return nil, l.blockReadError(offset, blockID, err)
		}
		l.cache.Put(blockID, int64(rc.StoredLen()), rc)
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
	rc, err := l.loadRowsContext(offset)
	if err != nil {
		return nil, l.blockReadError(offset, blockID, err)
	}
	if l.scan != nil && uint64(rc.StoredLen()) <= l.scan.Remaining() {
		l.scan.Put(blockID, int64(rc.StoredLen()), rc)
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

// indexStats returns the Row Index Page cache counters (nil-safe).
func (l *blockLoader) indexStats() (capBytes, used, hits, misses, evictions, loads uint64) {
	if l.index == nil {
		return 0, 0, 0, 0, 0, 0
	}
	return l.index.CapacityBytes(), l.index.UsedBytes(), l.index.Hits(), l.index.Misses(), l.index.Evictions(), l.index.Loads()
}

// indexOverhead returns the Row Index Page cache management memory (nil-safe).
func (l *blockLoader) indexOverhead() uint64 {
	if l.index == nil {
		return 0
	}
	return l.index.OverheadBytes()
}

// indexCache returns the Row Index Page LRU (nil when Lazy index cache is
// disabled). The store's LazySource uses it to serve decoded pages.
func (l *blockLoader) indexCache() *cache.LRU { return l.index }

// readStats returns the cumulative physical I/O counters of the underlying
// reader (bytes pulled from the file, bytes produced by decompression).
func (l *blockLoader) readIOStats() block.IOStats { return l.reader.Stats() }

// cacheOverhead returns the estimated management memory of the random-read
// cache (nil-safe).
func (l *blockLoader) cacheOverhead() uint64 { return l.cache.OverheadBytes() }

// scanOverhead returns the estimated management memory of the scan window
// (nil-safe).
func (l *blockLoader) scanOverhead() uint64 { return l.scan.OverheadBytes() }
