package rowpack

import (
	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/cache"
)

// blockLoader loads validated blocks through the LRU with concurrent miss
// merging (singleflight). Only CRC-validated blocks enter the cache; CRC
// failures are never cached. Blocks larger than the cache capacity are
// readable but not cached.
type blockLoader struct {
	reader *block.Reader
	cache  *cache.LRU
	sf     cache.Group
}

func newBlockLoader(reader *block.Reader, cacheBytes int64) *blockLoader {
	var lru *cache.LRU
	if cacheBytes > 0 {
		lru = cache.NewLRU(cacheBytes)
	}
	return &blockLoader{reader: reader, cache: lru}
}

// Load returns the validated block at offset with the given block ID, serving
// from cache when possible.
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
			return nil, err
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

// cacheStats returns the cache counters (nil-safe).
func (l *blockLoader) cacheStats() (capBytes, used, hits, misses, evictions, loads uint64) {
	if l.cache == nil {
		return 0, 0, 0, 0, 0, 0
	}
	return l.cache.CapacityBytes(), l.cache.UsedBytes(), l.cache.Hits(), l.cache.Misses(), l.cache.Evictions(), l.cache.Loads()
}
