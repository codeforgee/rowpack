package block

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
)

// rows_page_arms_test.go 覆盖 Rows Page 的二次校验阶梯:页面的 CRC 只能保证字节没
// 被中途改写,几何自洽的伪造页仍要靠 buildIndex / Records / RecordAt 各自重算一遍
// 流边界才能拦住。这里的每个用例都是手工装配的几何合法页 + 一处篡改。

// bodyLen is the tuple body size of every record built below.
const testBodyLen = 4

func uvals(vals ...uint64) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.AppendUvarint(b, v)
	}
	return b
}

func zz(d int64) uint64 { return uint64(d<<1) ^ uint64(d>>63) }

// craftedPage is a Rows Page assembled from its five streams, bypassing the
// encoder: every arm below needs a byte pattern the real writer never emits.
type craftedPage struct {
	count                                          uint32
	rowIDs, offsets, schemaRLE, changeBits, tuples []byte
	firstRowID, minRowID, maxRowID                 uint64
}

// insertPage returns a consistent page of n INSERT records: RowIDs 1..n (one
// byte each thanks to zigzag deltas of 1), testBodyLen-byte bodies and a single
// schema run covering the page.
func insertPage(n int) craftedPage {
	tuples := make([]byte, 0, n*testBodyLen)
	for i := 0; i < n*testBodyLen; i++ {
		tuples = append(tuples, byte(i))
	}
	rowIDs := uvals(1)
	for i := 2; i <= n; i++ {
		rowIDs = append(rowIDs, uvals(zz(1))...)
	}
	return craftedPage{
		count:      uint32(n),
		rowIDs:     rowIDs,
		offsets:    bytes.Repeat(uvals(testBodyLen), n),
		schemaRLE:  uvals(1, uint64(n)),
		changeBits: make([]byte, (n+3)/4),
		tuples:     tuples,
		firstRowID: 1,
		minRowID:   1,
		maxRowID:   uint64(n),
	}
}

// encode lays the page out and stamps the streams CRC, so every geometric check
// passes and only the tampered field can trip a validation.
func (c craftedPage) encode(tb testing.TB) []byte {
	tb.Helper()
	require.Equal(tb, (int(c.count)+3)/4, len(c.changeBits), "change bits length is frozen by the entry count")
	var streams []byte
	streams = append(streams, c.rowIDs...)
	streams = append(streams, c.offsets...)
	streams = append(streams, c.schemaRLE...)
	streams = append(streams, c.changeBits...)
	streams = append(streams, c.tuples...)

	h := format.RowsPageHeader{
		EntryCount:      c.count,
		RowIDsBytes:     uint32(len(c.rowIDs)),
		OffsetsBytes:    uint32(len(c.offsets)),
		SchemaRLEBytes:  uint32(len(c.schemaRLE)),
		ChangeBitsBytes: uint32(len(c.changeBits)),
		TuplesBytes:     uint32(len(c.tuples)),
		FirstRowID:      c.firstRowID,
		MinRowID:        c.minRowID,
		MaxRowID:        c.maxRowID,
		CRC32C:          format.CRC32C(streams),
	}
	page := make([]byte, format.RowsPageHeaderSize+len(streams))
	require.NoError(tb, h.MarshalTo(page))
	copy(page[format.RowsPageHeaderSize:], streams)
	return page
}

// parse2 builds and parses a two-record page, proving the fixtures are well
// formed before each arm tampers with one.
func parse2(tb testing.TB) *RowsPage {
	tb.Helper()
	p, err := ParseRowsPage(insertPage(2).encode(tb))
	require.NoError(tb, err, "the fixture page must parse")
	return p
}

func TestPageBuilderFinishEmpty(t *testing.T) {
	raw, err := NewPageBuilder(1 << 10).Finish()
	require.NoError(t, err)
	require.Nil(t, raw, "an empty builder finishes to nothing, not to a header-only page")
}

func TestRowsPageIndexRejectsMalformedStreams(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*craftedPage)
		want   string
	}{
		{"row id varint runs off the stream", func(c *craftedPage) {
			c.rowIDs = []byte{0xFF}
		}, "page stream truncated"},
		{"end-offset varint runs off the stream", func(c *craftedPage) {
			c.offsets = []byte{0xFF}
		}, "page stream truncated"},
		{"end offset overflows uint32", func(c *craftedPage) {
			c.offsets = uvals(^uint64(0))
		}, "end offset overflows"},
		{"schema version varint runs off the RLE", func(c *craftedPage) {
			c.schemaRLE = []byte{0xFF}
		}, "page stream truncated"},
		{"schema run length varint runs off the RLE", func(c *craftedPage) {
			c.schemaRLE = append(uvals(1), 0xFF)
		}, "page stream truncated"},
		{"schema version exceeds uint32", func(c *craftedPage) {
			c.schemaRLE = uvals(^uint64(0), 1)
		}, "exceeds uint32"},
		{"schema run is empty", func(c *craftedPage) {
			c.schemaRLE = uvals(1, 0)
		}, "exceeds remaining records"},
		{"schema run overruns the page", func(c *craftedPage) {
			c.schemaRLE = uvals(1, 99)
		}, "exceeds remaining records"},
		{"streams carry trailing bytes", func(c *craftedPage) {
			c.rowIDs = append(append([]byte(nil), c.rowIDs...), 0x00)
		}, "trailing bytes"},
		{"tuple bodies are shorter than the last end offset", func(c *craftedPage) {
			c.tuples = c.tuples[:len(c.tuples)-1]
		}, "exceeds tuples"},
		{"tuple bodies carry trailing bytes", func(c *craftedPage) {
			c.tuples = append(append([]byte(nil), c.tuples...), 0xEE)
		}, "tuple ends at"},
		{"first row id disagrees with the header", func(c *craftedPage) {
			c.firstRowID = 99
		}, "first row id"},
		{"row id range disagrees with the header", func(c *craftedPage) {
			c.maxRowID = 42
		}, "row id range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := insertPage(2)
			tc.mutate(&c)
			_, err := ParseRowsPage(c.encode(t))
			require.Error(t, err, "a geometrically valid page with a broken stream must be rejected")
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestValidateChangeBitsRejectsIllegalTailLane covers the lanes past the last
// full byte, which the SWAR fast path cannot see.
func TestValidateChangeBitsRejectsIllegalTailLane(t *testing.T) {
	c := insertPage(5) // 5 records => one full byte plus one tail byte
	require.Len(t, c.changeBits, 2)
	c.changeBits[1] = 0x03 // tail entry's packed change type == 3

	_, err := ParseRowsPage(c.encode(t))
	require.Error(t, err)
	require.Contains(t, err.Error(), "illegal change type marker")
	require.Contains(t, err.Error(), "record 4", "the offending tail entry is attributed")

	// The same marker in an unused lane of that byte is ignored by design.
	c.changeBits[1] = 0xC0
	_, err = ParseRowsPage(c.encode(t))
	require.NoError(t, err, "unused lanes past the entry count stay ignored")
}

func TestRecordAtGuards(t *testing.T) {
	t.Run("ordinal out of range", func(t *testing.T) {
		p := parse2(t)
		_, err := p.RecordAt(p.h.EntryCount)
		require.Error(t, err)
		require.Contains(t, err.Error(), "out of range")
	})

	t.Run("record decodes", func(t *testing.T) {
		p := parse2(t)
		rec, err := p.RecordAt(1)
		require.NoError(t, err)
		require.Equal(t, uint64(2), rec.RowID)
		require.EqualValues(t, 1, rec.SchemaVersion)
		require.Equal(t, format.ChangeInsert, rec.ChangeType)
		require.Len(t, rec.Body, testBodyLen)
	})

	t.Run("illegal packed change type", func(t *testing.T) {
		// Only reachable through a page mutated after parsing: validateChangeBits
		// runs first, so RecordAt keeps its own guard as defense in depth.
		p := parse2(t)
		p.changeBits[0] |= 0x03
		_, err := p.RecordAt(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "illegal packed change type")
	})

	t.Run("delete carrying a body", func(t *testing.T) {
		p := parse2(t)
		p.changeBits[0] = 0x02 // record 0 becomes DELETE but keeps its body
		_, err := p.RecordAt(0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "delete record 0 carries a body")
	})
}

// TestRecordsRevalidatesStreams drives Records over already-indexed pages whose
// streams were broken after parsing: the sequential walker repeats every
// boundary check instead of trusting the index built at parse time.
func TestRecordsRevalidatesStreams(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RowsPage)
		want   string
	}{
		{"row id varint runs off the stream", func(p *RowsPage) { p.rowIDs = []byte{0xFF} }, "page stream truncated"},
		{"end-offset varint runs off the stream", func(p *RowsPage) { p.offsets = []byte{0xFF} }, "page stream truncated"},
		{"schema version varint runs off the RLE", func(p *RowsPage) { p.schemaRLE = []byte{0xFF} }, "page stream truncated"},
		{"schema run length varint runs off the RLE", func(p *RowsPage) {
			p.schemaRLE = append(uvals(1), 0xFF)
		}, "page stream truncated"},
		{"illegal packed change type", func(p *RowsPage) { p.changeBits[0] |= 0x03 }, "illegal packed change type"},
		{"delete carrying a body", func(p *RowsPage) { p.changeBits[0] = 0x02 }, "delete record 0 carries a body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := parse2(t)
			tc.mutate(p)
			err := p.Records(func(codec.PageRecord) error { return nil })
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestRecordsVisitsEveryRecord is the control arm: the same walker succeeds on
// the untouched page, so the cases above fail on their tampered field only.
func TestRecordsVisitsEveryRecord(t *testing.T) {
	p := parse2(t)
	var got []uint64
	require.NoError(t, p.Records(func(rec codec.PageRecord) error {
		got = append(got, rec.RowID)
		require.Len(t, rec.Body, testBodyLen)
		return nil
	}))
	require.Equal(t, []uint64{1, 2}, got)
}
