package index

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
)

// FuzzParseRowIndexPages feeds arbitrary page regions to pageParser:
// it must never panic and never hand a malformed batch to the sink. It is a
// no-op during normal `go test` beyond the seed corpus; run with
// `go test -fuzz=FuzzParseRowIndexPages ./internal/index` for fuzzing.
func FuzzParseRowIndexPages(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, byte(0))
	f.Add([]byte{0x52, 0x50, 0x4B, 0x49, 0x44, 0x58, 0x50, 0x47, 0x01, 0, 0, 0}, byte(1))
	f.Fuzz(func(t *testing.T, region []byte, countByte byte) {
		// Keep the CI smoke fuzz bounded on slower platforms. Larger regions are
		// still covered by the deterministic corruption tests below.
		if len(region) > 64<<10 {
			t.Skip()
		}
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic for len=%d count=%d: %v", len(region), countByte, r)
			}
		}()
		sink := &throwingSink{}
		// Bound the page count so a forged huge count never reaches an
		// attacker-sized allocation; the parser must reject out-of-bounds
		// fence lengths before allocating.
		pageCount := uint32(countByte) * 4
		_, _, _ = (&pageParser{region: region, pageCount: pageCount, snapshotID: 1, sink: sink}).parse()
	})
}

type throwingSink struct {
	rows int
}

func (s *throwingSink) SetSnapshot(format.SnapshotIndexEntry) error { return nil }
func (s *throwingSink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *throwingSink) AddBlock(format.BlockIndexEntry) error       { return nil }
func (s *throwingSink) AddRows(batch []format.RowIndexEntry) error {
	s.rows += len(batch)
	return nil
}

// buildPageRegion compiles rows into the on-disk page region (pages + fences)
// with correct StoredOffsets, for a given snapshotID.
func buildPageRegion(t *testing.T, rows []format.RowIndexEntry, snapshotID uint64) []byte {
	t.Helper()
	b := NewBuilder(1)
	if err := b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: snapshotID, SnapshotType: format.SnapshotFull}); err != nil {
		t.Fatal(err)
	}
	for i := range rows {
		e := rows[i]
		e.SnapshotID = snapshotID
		if err := b.AddRow(e); err != nil {
			t.Fatal(err)
		}
	}
	pages, err := b.buildPages(nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	off := uint64(0)
	for i := range pages {
		pages[i].fence.StoredOffset = off
		off += uint64(len(pages[i].stored))
		out = append(out, pages[i].stored...)
	}
	var fbuf [format.IndexFenceEntrySize]byte
	for i := range pages {
		if err := pages[i].fence.MarshalTo(fbuf[:]); err != nil {
			t.Fatal(err)
		}
		out = append(out, fbuf[:]...)
	}
	return out
}

func parseCorruptPages(region []byte, pageCount uint32, snapshotID uint64) (int, error) {
	sink := &throwingSink{}
	_, n, err := (&pageParser{region: region, pageCount: pageCount, snapshotID: snapshotID, sink: sink}).parse()
	return int(n), err
}

func TestParseRowsValid(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	rows, err := parseCorruptPages(region, uint32(len(riSeq(100, 25))/indexPageEntryCount+1), 9)
	require.NoError(t, err, "valid parse rejected")
	require.EqualValues(t, 100, rows, "rows = %d, want 100", rows)
}

func TestParseRowsForgedPageCount(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	// Huge page count: fence directory must exceed the region before any
	// attacker-sized allocation.
	if _, err := parseCorruptPages(region, 0x7FFFFFFF, 9); err == nil {
		t.Fatal("forged huge page count = nil error")
	}
	// page count 0 with a non-empty fence region: no pages requested.
	if n, err := parseCorruptPages(region, 0, 9); err != nil || n != 0 {
		t.Fatalf("pageCount=0 => n=%d err=%v, want 0,nil", n, err)
	}
}

// TestParseRowsFenceSizeOutOfBounds: StoredOffset is no longer on disk, so the
// declared size is what can still run off the end of the pages region.
func TestParseRowsFenceSizeOutOfBounds(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	pageCount := uint32((100 + indexPageEntryCount - 1) / indexPageEntryCount)
	fenceStart := len(region) - int(pageCount)*format.IndexFenceEntrySize
	// Forge the first fence's StoredSize (bytes 20..24) to an absurd value.
	binary.LittleEndian.PutUint32(region[fenceStart+20:], 0xFFFFFFFF)
	if _, err := parseCorruptPages(region, pageCount, 9); err == nil {
		t.Fatal("fence size out of bounds = nil error")
	}
}

func TestParseRowsFenceSizeZero(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	pageCount := uint32((100 + indexPageEntryCount - 1) / indexPageEntryCount)
	fenceStart := len(region) - int(pageCount)*format.IndexFenceEntrySize
	// Zero the first fence's StoredSize (bytes 20..24).
	for i := range 4 {
		region[fenceStart+20+i] = 0
	}
	if _, err := parseCorruptPages(region, pageCount, 9); err == nil {
		t.Fatal("fence zero stored size = nil error")
	}
}

// TestParseRowsFenceSnapshotIsInjected: SnapshotID is no longer on disk, so
// there is no per-entry copy to forge — a fence always reports the snapshot the
// txn header declared. What used to be a mismatch check over disk bytes is now
// structural, so the same region follows the header.
func TestParseRowsFenceSnapshotIsInjected(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	pageCount := uint32((100 + indexPageEntryCount - 1) / indexPageEntryCount)
	for _, snap := range []uint64{9, 12345} {
		sink := &fenceCaptureSink{}
		_, _, err := (&pageParser{region: region, pageCount: pageCount, snapshotID: snap, sink: sink}).parse()
		require.NoError(t, err, "snapshot %d", snap)
		require.NotEmpty(t, sink.fences, "snapshot %d", snap)
		for i, f := range sink.fences {
			require.EqualValues(t, snap, f.SnapshotID, "fence %d", i)
		}
	}
}

// TestParseRowsPagesMustTileRegion: StoredOffset is recomputed by the parser
// from the preceding sizes, so pages cannot be made to overlap from disk any
// more. What still has to hold is that those sizes add up to exactly the pages
// region; shrink the first by one byte and the region no longer tiles.
func TestParseRowsPagesMustTileRegion(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	pageCount := uint32((100 + indexPageEntryCount - 1) / indexPageEntryCount)
	fenceStart := len(region) - int(pageCount)*format.IndexFenceEntrySize
	binary.LittleEndian.PutUint32(region[fenceStart+20:],
		binary.LittleEndian.Uint32(region[fenceStart+20:])-1)
	if _, err := parseCorruptPages(region, pageCount, 9); err == nil {
		t.Fatal("pages region not tiled exactly = nil error")
	}
}

func TestParseRowsPageCorruptCompressed(t *testing.T) {
	region := buildPageRegion(t, riSeq(100, 25), 9)
	pageCount := uint32((100 + indexPageEntryCount - 1) / indexPageEntryCount)
	// Flip a byte inside the first page's compressed payload (in the pages
	// region). Decompression or the page CRC must catch it.
	region[3] ^= 0xFF
	if _, err := parseCorruptPages(region, pageCount, 9); err == nil {
		t.Fatal("corrupt compressed page = nil error")
	}
}

// TestParseRowsFenceRawSizeBelowHeaderRejected: a fence whose RawSize is
// smaller than the 64-byte Row Index Page header must be rejected with an
// error, not a slice panic on pageRaw[RagePageHeaderSize:].
func TestParseRowsFenceRawSizeBelowHeaderRejected(t *testing.T) {
	frameSrc := make([]byte, 30)
	frame, err := block.Compress(format.CompressionZstd, 3, frameSrc)
	if err != nil {
		t.Fatal(err)
	}
	var fence format.RowIndexFenceEntry
	fence.SnapshotID = 9
	fence.StoredOffset = 0
	fence.StoredSize = uint32(len(frame))
	fence.RawSize = 30 // below the 64-byte page header
	fence.EntryCount = 3
	var fbuf [format.IndexFenceEntrySize]byte
	if err := fence.MarshalTo(fbuf[:]); err != nil {
		t.Fatal(err)
	}
	region := append(append([]byte{}, frame...), fbuf[:]...)
	if _, err := parseCorruptPages(region, 1, 9); err == nil {
		t.Fatal("fence raw size below page header = nil error")
	}
}
