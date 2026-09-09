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
	sf     cache.Group
}

// scanBudgetFor bounds the scan window to a share of the cache budget: half
// of it, floored at 1 MiB and capped at 64 MiB so huge explicit cache sizes
// do not let scans squat on the random-read hot set. Small and layered scan
// sets (deep chains) fit the window and are reused across iterations; very
// large scans fill it and then stream through the pool without allocating.
func scanBudgetFor(cacheBytes int64) int64 {
	b := cacheBytes / 2
	if b < 1<<20 {
		return 1 << 20
	}
	if b > 64<<20 {
		return 64 << 20
	}
	return b
}

func newBlockLoader(reader *block.Reader, file string, cacheBytes int64) *blockLoader {
	var lru, scn *cache.LRU
	if cacheBytes > 0 {
		lru = cache.NewLRU(cacheBytes)
		scn = cache.NewLRU(scanBudgetFor(cacheBytes))
	}
	return &blockLoader{reader: reader, file: file, cache: lru, scan: scn}
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

// scanRef wraps a block for streamed use. Cache-owned blocks are served
// directly (Release is a no-op); transient (uncached) blocks own a pooled
// scratch buffer that Release returns to the pool.
type scanRef struct {
	blk *block.Block
	sc  *block.BlockScratch
}

// Raw returns the validated uncompressed payload.
func (r *scanRef) Raw() []byte { return r.blk.Raw }

// Release returns any pooled scratch. It is idempotent.
func (r *scanRef) Release() {
	if r.sc != nil {
		r.sc.Release()
		r.sc = nil
	}
}

// LoadScan serves streaming reads (Scan and batch reads). Lookup order is the
// random-read cache, then the scan window; a miss is decompressed into a
// pooled scratch. A block with room in the window is promoted into it: an
// exact-fit scratch transfers its buffer without a copy (the pool
// replenishes itself on demand); an oversized scratch is copied so window
// accounting stays tight. The returned hit reports a cache/scan-window hit
// (used for batch cache statistics; scans ignore it).
func (l *blockLoader) LoadScan(offset int64, blockID uint64) (*scanRef, bool, error) {
	if l.cache != nil {
		if v, ok := l.cache.Get(blockID); ok {
			return &scanRef{blk: v.(*block.Block)}, true, nil
		}
		if v, ok := l.scan.Get(blockID); ok {
			return &scanRef{blk: v.(*block.Block)}, true, nil
		}
	}
	sc, err := l.reader.ReadAtBlockTransient(offset)
	if err != nil {
		return nil, false, l.blockReadError(offset, blockID, err)
	}
	if l.scan != nil && uint64(len(sc.Raw)) <= l.scan.Remaining() {
		if cap(sc.Raw) == len(sc.Raw) {
			// Exact-fit scratch: transfer ownership into the window.
			blk := sc.Block
			sc.Detach()
			l.scan.Put(blockID, int64(len(blk.Raw)), &blk)
			return &scanRef{blk: &blk}, false, nil
		}
		blk := &block.Block{Header: sc.Header, Raw: make([]byte, len(sc.Raw))}
		copy(blk.Raw, sc.Raw)
		l.scan.Put(blockID, int64(len(blk.Raw)), blk)
		sc.Release()
		return &scanRef{blk: blk}, false, nil
	}
	return &scanRef{blk: &sc.Block, sc: sc}, false, nil
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
