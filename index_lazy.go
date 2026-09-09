package rowpack

import (
	"fmt"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
)

// LoadIndexPage implements index.LazySource: it reads, authenticates,
// decompresses and decodes one Row Index Page for a Lazy view.
//
// The page lives in the txn body at absolute offset txnStart + fence.StoredOffset
// (the txn's span was read and CRC-verified at Open by readIndexTxn, so the
// stored bytes are integrity-checked before any key is needed). Concurrent
// misses on the same page are merged (singleflight). When cache is true the
// decoded entries are served from / promoted into the loader's IndexPageCache
// (random read); a streaming scan passes cache=false so it never pollutes that
// cache. The returned slice is owned by the cache (or freshly allocated for a
// scan) and must not be mutated or retained.
func (s *Store) LoadIndexPage(snapshotID uint64, pageIdx int, fence fileformat.RowIndexFenceEntry, cache bool) ([]fileformat.RowIndexEntry, error) {
	info, ok := s.lazyPages[snapshotID]
	if !ok {
		return nil, fmt.Errorf("rowpack: no lazy page context for snapshot %d", snapshotID)
	}
	// fence.StoredOffset is an offset into the txn BODY (after the IndexTxn
	// header); the txn itself starts at txnStart in the file.
	absOff := info.txnStart + fileformat.IndexTxnHeaderSize + int64(fence.StoredOffset)
	if cache && s.loader.indexCache() != nil {
		if v, ok := s.loader.indexCache().Get(uint64(absOff)); ok {
			return v.([]fileformat.RowIndexEntry), nil
		}
	}
	v, err := s.idxPageSF.Do(uint64(absOff), func() (any, error) {
		return s.loadAndDecodeIndexPage(snapshotID, pageIdx, fence, absOff, cache && s.loader.indexCache() != nil)
	})
	if err != nil {
		return nil, err
	}
	return v.([]fileformat.RowIndexEntry), nil
}

// loadAndDecodeIndexPage reads the stored page bytes, OPENs (when encrypted),
// decompresses, CRC-validates and decodes them into entries, then promotes into
// the IndexPageCache when cache is true. An error is normalized into a
// CorruptionError via the loader.
func (s *Store) loadAndDecodeIndexPage(snapshotID uint64, pageIdx int, fence fileformat.RowIndexFenceEntry, absOff int64, cache bool) ([]fileformat.RowIndexEntry, error) {
	info := s.lazyPages[snapshotID]
	stored := make([]byte, fence.StoredSize)
	if _, err := s.data.ReadAt(stored, absOff); err != nil {
		return nil, s.loader.blockReadError(absOff, 0, err)
	}
	raw := stored
	if info.crypto != nil {
		pt, err := info.crypto.Open(info.seq+uint32(pageIdx), fileformat.IndexChunkKindRow, uint32(pageIdx), int(fence.RawSize), stored)
		if err != nil {
			return nil, s.loader.blockReadError(absOff, 0, err)
		}
		raw = pt
	}
	pageRaw, err := block.Decompress(fileformat.CompressionZstd, nil, raw, fence.RawSize)
	if err != nil {
		return nil, s.loader.blockReadError(absOff, 0, err)
	}
	if uint32(len(pageRaw)) != fence.RawSize {
		return nil, s.loader.blockReadError(absOff, 0, fmt.Errorf("rowpack: index page raw %d bytes, want %d", len(pageRaw), fence.RawSize))
	}
	if len(pageRaw) < fileformat.IndexPageHeaderSize ||
		fileformat.CRC32C(pageRaw[fileformat.IndexPageHeaderSize:]) != fence.PageCRC32C {
		return nil, s.loader.blockReadError(absOff, 0, fmt.Errorf("rowpack: index page CRC mismatch"))
	}
	entries, err := index.DecodeIndexPage(pageRaw)
	if err != nil {
		return nil, s.loader.blockReadError(absOff, 0, err)
	}
	if uint32(len(entries)) != fence.EntryCount {
		return nil, s.loader.blockReadError(absOff, 0, fmt.Errorf("rowpack: index page %d entries, fence says %d", len(entries), fence.EntryCount))
	}
	if cache && s.loader.indexCache() != nil {
		s.loader.indexCache().Put(uint64(absOff), int64(len(entries))*fileformat.RowIndexEntrySize, entries)
	}
	return entries, nil
}
