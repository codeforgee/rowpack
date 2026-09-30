package index

import (
	"bytes"
	"errors"
	"testing"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// page_parser_sinks_test.go 驱动 pageParser 的三条 sink 分派路径(bulk page /
// per-entry / buffered)与 Lazy 的 FenceCapture 短路:每条路径都必须自己负责
// 页面自洽性校验,而不是把篡改过的页交给上层。

// buildContrivedRegion wraps one raw page into a one-page region (stored page +
// fence directory) with every fence field under the caller's control. The page
// CRC is stamped to match the given raw bytes, so a tampered page still reaches
// the payload checks instead of stopping at the CRC.
func buildContrivedRegion(tb testing.TB, raw []byte, mutate func(*format.RowIndexFenceEntry)) []byte {
	tb.Helper()
	stored, err := block.Compress(format.CompressionZstd, 3, raw)
	require.NoError(tb, err)
	f := format.RowIndexFenceEntry{
		SnapshotID:   1,
		StoredSize:   uint32(len(stored)),
		RawSize:      uint32(len(raw)),
		EntryCount:   3,
		PageCRC32C:   format.CRC32C(raw[format.IndexPageHeaderSize:]),
		StoredOffset: 0,
	}
	if mutate != nil {
		mutate(&f)
	}
	var fbuf [format.IndexFenceEntrySize]byte
	require.NoError(tb, f.MarshalTo(fbuf[:]))
	return append(append([]byte{}, stored...), fbuf[:]...)
}

func parseContrived(tb testing.TB, region []byte, sink TxnSink) (uint64, error) {
	tb.Helper()
	_, rows, err := (&pageParser{region: region, pageCount: 1, snapshotID: 1, sink: sink}).parse()
	return rows, err
}

// illegalChangePage returns a well-formed-looking page whose packed change bits
// hold the illegal value 3 for the first entry: both the page header CRC and
// the fence CRC describe exactly these bytes, so nothing but the payload
// validation can reject it.
func illegalChangePage(tb testing.TB) []byte {
	tb.Helper()
	p := idxSingleRun(3)
	p.changeBits[0] |= 0x03
	return p.encode(tb)
}

type batchOnlySink struct {
	err     error
	batches int
	entries int
}

func (s *batchOnlySink) SetSnapshot(format.SnapshotIndexEntry) error { return nil }
func (s *batchOnlySink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *batchOnlySink) AddBlock(format.BlockIndexEntry) error       { return nil }
func (s *batchOnlySink) AddRows([]format.RowIndexEntry) error {
	return errors.New("batch sink must not be fed through AddRows")
}
func (s *batchOnlySink) AddRowBatch(b *pageRows, snapshotID uint64) error {
	s.batches++
	s.entries += len(b.rowIDs)
	return s.err
}

type entryOnlySink struct {
	err     error
	entries int
}

func (s *entryOnlySink) SetSnapshot(format.SnapshotIndexEntry) error { return nil }
func (s *entryOnlySink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *entryOnlySink) AddBlock(format.BlockIndexEntry) error       { return nil }
func (s *entryOnlySink) AddRows([]format.RowIndexEntry) error {
	return errors.New("entry sink must not be fed through AddRows")
}
func (s *entryOnlySink) AddRowEntry(e format.RowIndexEntry) error {
	s.entries++
	return s.err
}

type fenceCaptureSink struct {
	err      error
	fences   []format.RowIndexFenceEntry
	rowCalls int
}

func (s *fenceCaptureSink) SetSnapshot(format.SnapshotIndexEntry) error { return nil }
func (s *fenceCaptureSink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *fenceCaptureSink) AddBlock(format.BlockIndexEntry) error       { return nil }
func (s *fenceCaptureSink) AddRows([]format.RowIndexEntry) error {
	s.rowCalls++
	return nil
}
func (s *fenceCaptureSink) SetRowIndexFences(fences []format.RowIndexFenceEntry) error {
	s.fences = fences
	return s.err
}

// refusingRowsSink fails AddRows so the buffered path's own error wrapping runs.
type refusingRowsSink struct{ err error }

func (s *refusingRowsSink) SetSnapshot(format.SnapshotIndexEntry) error { return nil }
func (s *refusingRowsSink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *refusingRowsSink) AddBlock(format.BlockIndexEntry) error       { return nil }
func (s *refusingRowsSink) AddRows([]format.RowIndexEntry) error        { return s.err }

// TestPageParserRejectsPageCRCMismatch: the fence carries the page's stream CRC,
// so a page whose bytes drift from it must be rejected even though the stored
// frame still decompresses cleanly.
func TestPageParserRejectsPageCRCMismatch(t *testing.T) {
	region := buildContrivedRegion(t, idxSingleRun(3).encode(t), func(f *format.RowIndexFenceEntry) {
		f.PageCRC32C ^= 0xFFFFFFFF
	})
	_, err := parseContrived(t, region, &throwingSink{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "CRC mismatch")
}

func TestPageParserBatchSinkPath(t *testing.T) {
	t.Run("whole page lands in one batch", func(t *testing.T) {
		sink := &batchOnlySink{}
		_, err := parseContrived(t, buildContrivedRegion(t, idxSingleRun(3).encode(t), nil), sink)
		require.NoError(t, err)
		require.Equal(t, 1, sink.batches)
		require.Equal(t, 3, sink.entries)
	})

	t.Run("page does not decode", func(t *testing.T) {
		_, err := parseContrived(t, buildContrivedRegion(t, illegalChangePage(t), nil), &batchOnlySink{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "illegal packed change type")
	})

	t.Run("entry count disagrees with the fence", func(t *testing.T) {
		region := buildContrivedRegion(t, idxSingleRun(3).encode(t), func(f *format.RowIndexFenceEntry) {
			f.EntryCount = 5
		})
		_, err := parseContrived(t, region, &batchOnlySink{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "fence says 5")
	})

	t.Run("sink refuses the batch", func(t *testing.T) {
		sentinel := errors.New("shard builder refused the page")
		_, err := parseContrived(t, buildContrivedRegion(t, idxSingleRun(3).encode(t), nil), &batchOnlySink{err: sentinel})
		require.ErrorIs(t, err, sentinel)
	})
}

func TestPageParserRowEntrySinkPath(t *testing.T) {
	t.Run("entries arrive one at a time", func(t *testing.T) {
		sink := &entryOnlySink{}
		_, err := parseContrived(t, buildContrivedRegion(t, idxSingleRun(3).encode(t), nil), sink)
		require.NoError(t, err)
		require.Equal(t, 3, sink.entries)
	})

	t.Run("sink refuses an entry", func(t *testing.T) {
		sentinel := errors.New("shard builder refused the entry")
		_, err := parseContrived(t, buildContrivedRegion(t, idxSingleRun(3).encode(t), nil), &entryOnlySink{err: sentinel})
		require.ErrorIs(t, err, sentinel)
	})

	t.Run("entry count disagrees with the fence", func(t *testing.T) {
		region := buildContrivedRegion(t, idxSingleRun(3).encode(t), func(f *format.RowIndexFenceEntry) {
			f.EntryCount = 5
		})
		_, err := parseContrived(t, region, &entryOnlySink{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "fence says 5")
	})
}

func TestPageParserBufferedSinkPath(t *testing.T) {
	t.Run("page does not decode", func(t *testing.T) {
		_, err := parseContrived(t, buildContrivedRegion(t, illegalChangePage(t), nil), &throwingSink{})
		require.Error(t, err)
		require.Contains(t, err.Error(), "illegal packed change type")
	})

	t.Run("sink refuses the batch", func(t *testing.T) {
		sentinel := errors.New("sink refused the rows")
		_, err := parseContrived(t, buildContrivedRegion(t, idxSingleRun(3).encode(t), nil), &refusingRowsSink{err: sentinel})
		require.ErrorIs(t, err, sentinel)
	})
}

// TestPageParserFenceCaptureShortCircuit: Lazy Open hands the validated fence
// directory to the sink and must not touch a single page payload. The fixture's
// stored page is deliberately garbage, so any page decode here would fail.
func TestPageParserFenceCaptureShortCircuit(t *testing.T) {
	region := buildContrivedRegion(t, idxSingleRun(3).encode(t), nil)
	garbage := append([]byte{}, region...)
	for i := 8; i < 40 && i < len(region)-format.IndexFenceEntrySize; i++ {
		garbage[i] ^= 0xA5
	}

	sink := &fenceCaptureSink{}
	_, err := parseContrived(t, garbage, sink)
	require.NoError(t, err, "the fence must be accepted without decoding the page")
	require.Len(t, sink.fences, 1)
	require.EqualValues(t, 3, sink.fences[0].EntryCount)
	require.EqualValues(t, 1, sink.fences[0].SnapshotID)
	require.Zero(t, sink.rowCalls, "no row may be handed to the sink in Lazy mode")
}

func TestPageParserFenceCaptureErrorPropagates(t *testing.T) {
	sentinel := errors.New("fence directory rejected")
	_, err := (&bodyParser{
		region:            armBody(t, nil),
		snapshotID:        1,
		rowIndexPageCount: 1,
		sink:              &fenceCaptureSink{err: sentinel},
	}).parse()
	require.ErrorIs(t, err, sentinel)
}

// TestPageParserFenceCaptureRowCount: Lazy Open never counts rows itself — the
// caller derives the count from the captured fences — and that sum must agree
// with what these same pages decode to in Eager mode.
func TestPageParserFenceCaptureRowCount(t *testing.T) {
	body := armBody(t, nil)
	pageCount := uint32(1)

	capture := &fenceCaptureSink{}
	lazy, err := (&bodyParser{
		region:            body,
		snapshotID:        1,
		rowIndexPageCount: pageCount,
		sink:              capture,
	}).parse()
	require.NoError(t, err)
	require.EqualValues(t, 0, lazy.rowCount, "the Lazy path deliberately reports no row count")

	eager, err := (&bodyParser{region: body, snapshotID: 1, rowIndexPageCount: pageCount}).parse()
	require.NoError(t, err)

	fenced := uint64(0)
	for i := range capture.fences {
		fenced += uint64(capture.fences[i].EntryCount)
	}
	require.Equal(t, eager.rowCount, fenced)
	require.Len(t, eager.rows, int(fenced), "the Eager path must yield exactly the fenced entries")
}

// TestPageParserRejectsPagesRegionSlack: every fence taken alone is in bounds,
// but together they must still cover exactly the pages region.
func TestPageParserRejectsPagesRegionSlack(t *testing.T) {
	rows := []format.RowIndexEntry{
		riEntry(1, 1, 10, 0, format.ChangeInsert),
		riEntry(1, 2, 10, 1, format.ChangeInsert),
	}
	region := buildPageRegion(t, rows, 9)
	fenceLen := format.IndexFenceEntrySize
	cut := len(region) - fenceLen
	padded := append(append([]byte{}, region[:cut]...), bytes.Repeat([]byte{0xEE}, 8)...)
	padded = append(padded, region[cut:]...)

	_, err := parseCorruptPages(padded, 1, 9)
	require.Error(t, err)
	require.Contains(t, err.Error(), "row index pages span")
}

// TestParseBodyRejectsUnverifiableSnapshotEntry: the chunk header CRC admits the
// payload, but the snapshot entry carries its own CRC and must be restamped for
// the forgery to pass — so an unrestamped flip has to be caught here.
func TestParseBodyRejectsUnverifiableSnapshotEntry(t *testing.T) {
	body := append([]byte(nil), armBody(t, nil)...)
	var h format.IndexChunkHeader
	require.NoError(t, h.Unmarshal(body))
	storedOff := format.IndexChunkHeaderSize
	stored := body[storedOff : storedOff+int(h.StoredBytes)]
	require.Len(t, stored, format.SnapshotIndexEntrySize)

	stored[0] ^= 0xFF
	h.PayloadCRC32C = format.CRC32C(stored)
	require.NoError(t, h.MarshalTo(body[:format.IndexChunkHeaderSize]))

	_, err := (&bodyParser{region: body, snapshotID: 1}).parse()
	require.Error(t, err)
	require.Contains(t, err.Error(), "snapshot chunk 0")
}
