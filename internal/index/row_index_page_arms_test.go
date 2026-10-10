package index

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// row_index_page_arms_test.go 覆盖 Row Index Page 的解码校验阶梯与其调用方
// (buildPages / walkPage / pageFence) 的错误传播臂。页格式层只保证几何自洽与 CRC,
// 所以下面每个用例都是手工装配的「几何合法页 + 一处篡改」,让目标校验点成为唯一
// 能拦住它的地方。

func uvarm(vals ...uint64) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.AppendUvarint(b, v)
	}
	return b
}

// idxPageParts is one Row Index Page assembled from its five streams, bypassing
// encodePage so every byte pattern below is expressible.
type idxPageParts struct {
	count      uint32
	tableRun   []byte
	rowIDs     []byte
	blockRun   []byte
	ordinals   []byte
	changeBits []byte

	firstRowID, minRowID, maxRowID uint64
}

// idxSingleRun returns a consistent page of n INSERT entries under one table
// run (table 1) and one block run (block 10): RowIDs 1..n, ordinals 0..n-1.
func idxSingleRun(n int) idxPageParts {
	rowIDs := uvarm(1)
	ordinals := uvarm(0)
	for i := 2; i <= n; i++ {
		rowIDs = append(rowIDs, uvarm(1)...)
		ordinals = append(ordinals, uvarm(zigzag(1))...)
	}
	return idxPageParts{
		count:      uint32(n),
		tableRun:   uvarm(1, uint64(n)),
		rowIDs:     rowIDs,
		blockRun:   uvarm(10, uint64(n)),
		ordinals:   ordinals,
		changeBits: make([]byte, (n+3)/4),
		firstRowID: 1,
		minRowID:   1,
		maxRowID:   uint64(n),
	}
}

// encode lays the page out with matching byte lengths and a restamped streams
// CRC, so the geometry checks pass and only the tampered field can fail.
func (p idxPageParts) encode(tb testing.TB) []byte {
	tb.Helper()
	require.Equal(tb, (int(p.count)+3)/4, len(p.changeBits), "change bits length is frozen by the entry count")
	var streams []byte
	streams = append(streams, p.tableRun...)
	streams = append(streams, p.rowIDs...)
	streams = append(streams, p.blockRun...)
	streams = append(streams, p.ordinals...)
	streams = append(streams, p.changeBits...)

	h := format.RowIndexPageHeader{
		EntryCount:      p.count,
		TableRunBytes:   uint32(len(p.tableRun)),
		RowIDBytes:      uint32(len(p.rowIDs)),
		BlockRunBytes:   uint32(len(p.blockRun)),
		OrdinalBytes:    uint32(len(p.ordinals)),
		ChangeBitsBytes: uint32(len(p.changeBits)),
		FirstRowID:      p.firstRowID,
		MinRowID:        p.minRowID,
		MaxRowID:        p.maxRowID,
		CRC32C:          format.CRC32C(streams),
	}
	page := make([]byte, format.IndexPageHeaderSize+len(streams))
	require.NoError(tb, h.MarshalTo(page))
	copy(page[format.IndexPageHeaderSize:], streams)
	return page
}

// testBuilder returns a Builder carrying one valid row of the given snapshot.
func testBuilder(tb testing.TB, snapshotID uint64) *Builder {
	tb.Helper()
	b := NewBuilder(1)
	require.NoError(tb, b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: snapshotID, SnapshotType: format.SnapshotFull}))
	e := riEntry(1, 1, 2, 0, format.ChangeInsert)
	e.SnapshotID = snapshotID
	require.NoError(tb, b.AddRow(e))
	return b
}

func TestBuildPagesPropagatesFailures(t *testing.T) {
	t.Run("page encoding fails", func(t *testing.T) {
		// Change types outside 1..3 are unrepresentable in the 2-bit stream and
		// only encodePage can reject them.
		b := testBuilder(t, 1)
		e := riEntry(1, 2, 2, 1, format.ChangeType(9))
		e.SnapshotID = 1
		b.rows = append(b.rows, e)
		_, err := b.buildPages(nil, 3, 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "change type 9 not packable")
	})

	t.Run("sealing fails", func(t *testing.T) {
		sentinel := errors.New("keyring refused the page")
		crypto := &ChunkCrypto{
			Seal: func(uint32, uint8, uint32, int, []byte) ([]byte, error) { return nil, sentinel },
		}
		_, err := testBuilder(t, 1).buildPages(crypto, 3, 0)
		require.ErrorIs(t, err, sentinel, "a refused seal must reach the caller unchanged")
	})
}

// TestEncodePageTracksPageWideRowIDs: RowIDs ascend within a table but restart
// for the next table, so the page minimum can come from a later table run.
func TestEncodePageTracksPageWideRowIDs(t *testing.T) {
	rows := []format.RowIndexEntry{
		riEntry(1, 100, 10, 0, format.ChangeInsert),
		riEntry(1, 101, 10, 1, format.ChangeInsert),
		riEntry(2, 5, 20, 0, format.ChangeInsert),
	}
	page, n, minRowID, maxRowID, err := encodePage(rows, indexPageEntryCount)
	require.NoError(t, err)
	require.EqualValues(t, 3, n)
	require.EqualValues(t, 5, minRowID, "the page-wide minimum comes from the second table run")
	require.EqualValues(t, 101, maxRowID)

	got, err := decodePage(page)
	require.NoError(t, err, "the header must agree with the entries it describes")
	require.True(t, rowsEq(got, rows))
}

func TestWalkPagePropagatesSinkError(t *testing.T) {
	page := idxSingleRun(4).encode(t)
	sentinel := errors.New("sink full")
	seen := 0
	err := walkPage(page, func(format.RowIndexEntry) error {
		seen++
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, 1, seen, "the walk stops at the first rejected entry")

	var want int
	require.NoError(t, walkPage(page, func(format.RowIndexEntry) error { want++; return nil }))
	require.Equal(t, 4, want)
}

func TestDecodeIndexPageValidationLadder(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*idxPageParts)
		want   string
	}{
		{"table id varint runs off the stream", func(p *idxPageParts) {
			p.tableRun = []byte{0xFF}
		}, "index page corrupt"},
		{"table id exceeds uint32", func(p *idxPageParts) {
			p.tableRun = uvarm(0x100000000, 1)
		}, "table id"},
		{"table run length runs off the stream", func(p *idxPageParts) {
			p.tableRun = append(uvarm(1), 0xFF)
		}, "index page corrupt"},
		{"table run length is zero", func(p *idxPageParts) {
			p.tableRun = uvarm(1, 0)
		}, "table run len"},
		{"table run length overruns the page", func(p *idxPageParts) {
			p.tableRun = uvarm(1, 99)
		}, "table run len"},
		{"first row id varint runs off the stream", func(p *idxPageParts) {
			// splitPage's cheap guard only demands len(rowIDs) >= count, so the
			// truncated varint still needs that many bytes to reach the decoder.
			p.rowIDs = []byte{0xFF, 0xFF}
		}, "index page corrupt"},
		{"row id delta varint runs off the stream", func(p *idxPageParts) {
			p.rowIDs = append(uvarm(1), 0xFF)
		}, "index page corrupt"},
		{"block id varint runs off the stream", func(p *idxPageParts) {
			p.blockRun = []byte{0xFF}
		}, "index page corrupt"},
		{"block run length runs off the stream", func(p *idxPageParts) {
			p.blockRun = append(uvarm(10), 0xFF)
		}, "index page corrupt"},
		{"block run length is zero", func(p *idxPageParts) {
			p.blockRun = uvarm(10, 0)
		}, "block run len"},
		{"first ordinal varint runs off the stream", func(p *idxPageParts) {
			p.ordinals = []byte{0xFF}
		}, "index page corrupt"},
		{"ordinal exceeds uint32", func(p *idxPageParts) {
			p.ordinals = uvarm(0x100000000)
		}, "exceeds uint32"},
		{"ordinal delta varint runs off the stream", func(p *idxPageParts) {
			p.ordinals = append(uvarm(0), 0xFF)
		}, "index page corrupt"},
		{"ordinal goes negative", func(p *idxPageParts) {
			p.ordinals = append(uvarm(0), uvarm(zigzag(-5))...)
		}, "out of range"},
		{"illegal packed change type", func(p *idxPageParts) {
			p.changeBits[0] |= 0x03
		}, "illegal packed change type"},
		{"table ids descend across runs", func(p *idxPageParts) {
			p.tableRun = uvarm(5, 1, 3, 1)
		}, "table ids not sorted"},
		{"row ids do not ascend within a table", func(p *idxPageParts) {
			p.rowIDs = append(uvarm(^uint64(0)), uvarm(0)...)
		}, "row ids not strictly ascending"},
		{"table run carries trailing bytes", func(p *idxPageParts) {
			p.tableRun = append(append([]byte(nil), p.tableRun...), 0x00)
		}, "table run has"},
		{"row id stream carries trailing bytes", func(p *idxPageParts) {
			p.rowIDs = append(append([]byte(nil), p.rowIDs...), 0x00)
		}, "row id stream has"},
		{"block run carries trailing bytes", func(p *idxPageParts) {
			p.blockRun = append(append([]byte(nil), p.blockRun...), 0x00)
		}, "block run has"},
		{"ordinal stream carries trailing bytes", func(p *idxPageParts) {
			p.ordinals = append(append([]byte(nil), p.ordinals...), 0x00)
		}, "record ordinal stream has"},
		{"first row id disagrees with the header", func(p *idxPageParts) {
			p.firstRowID = 99
		}, "first row id"},
		{"min row id disagrees with the header", func(p *idxPageParts) {
			p.minRowID = 42
		}, "min row id"},
		{"max row id disagrees with the header", func(p *idxPageParts) {
			p.maxRowID = 0
		}, "max row id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := idxSingleRun(2)
			tc.mutate(&p)
			_, err := decodePage(p.encode(t))
			require.Error(t, err, "a geometrically valid page with a broken stream must be rejected")
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestDecodeIndexPageGlobalMinAcrossTables covers the page-wide minimum update:
// the second table run starts below everything seen so far.
func TestDecodeIndexPageGlobalMinAcrossTables(t *testing.T) {
	p := idxPageParts{
		count:      2,
		tableRun:   uvarm(1, 1, 2, 1),
		rowIDs:     append(uvarm(100), uvarm(5)...),
		blockRun:   uvarm(10, 1, 20, 1),
		ordinals:   append(uvarm(0), uvarm(zigzag(1))...),
		changeBits: []byte{0x00},
		firstRowID: 100,
		minRowID:   5,
		maxRowID:   100,
	}
	got, err := decodePage(p.encode(t))
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.EqualValues(t, 100, got[0].RowID)
	require.EqualValues(t, 5, got[1].RowID)
}

func TestPageFenceRejectsUnusablePages(t *testing.T) {
	t.Run("shorter than the page header", func(t *testing.T) {
		_, err := pageFence(make([]byte, 10), 8)
		require.ErrorIs(t, err, errIndexPageCorrupt)
	})

	t.Run("header does not decode", func(t *testing.T) {
		raw := make([]byte, format.IndexPageHeaderSize+8)
		raw[0] ^= 0xFF // break the page magic
		_, err := pageFence(raw, 8)
		require.Error(t, err)
		require.Contains(t, err.Error(), "bad magic")
	})

	t.Run("first table id exceeds uint32", func(t *testing.T) {
		// The header only cross-checks geometry, so a table run holding a value
		// beyond uint32 still reaches pageFence's own extraction, which is what
		// the fence must fail on rather than silently truncating into a fence key.
		p := idxPageParts{count: 1, tableRun: uvarm(0x100000000), changeBits: []byte{0x00}}
		_, err := pageFence(p.encode(t), 8)
		require.ErrorIs(t, err, errIndexPageCorrupt)
	})

	t.Run("well formed page", func(t *testing.T) {
		raw := idxSingleRun(3).encode(t)
		fence, err := pageFence(raw, 16)
		require.NoError(t, err)
		require.Zero(t, fence.SnapshotID, "the snapshot is injected by the parser, not stored")
		require.Zero(t, fence.StoredOffset, "the offset is recomputed by the parser, not stored")
		require.EqualValues(t, 16, fence.StoredSize)
		require.EqualValues(t, 3, fence.EntryCount)
	})
}

func TestFirstTableIDOfPageGuards(t *testing.T) {
	t.Run("shorter than the page header", func(t *testing.T) {
		_, ok := firstTableIDOfPage(make([]byte, 10))
		require.False(t, ok)
	})

	t.Run("varint runs off the page", func(t *testing.T) {
		raw := make([]byte, format.IndexPageHeaderSize+2)
		raw[format.IndexPageHeaderSize] = 0xFF
		raw[format.IndexPageHeaderSize+1] = 0xFF
		_, ok := firstTableIDOfPage(raw)
		require.False(t, ok)
	})

	t.Run("table id exceeds uint32", func(t *testing.T) {
		raw := append(make([]byte, format.IndexPageHeaderSize), uvarm(0x100000000)...)
		_, ok := firstTableIDOfPage(raw)
		require.False(t, ok)
	})

	t.Run("well formed page", func(t *testing.T) {
		tid, ok := firstTableIDOfPage(idxSingleRun(3).encode(t))
		require.True(t, ok)
		require.EqualValues(t, 1, tid)
	})
}
