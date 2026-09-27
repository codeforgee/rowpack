package block

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
)

// containerSink captures flushed blocks from a builder.
type containerSink struct {
	blocks []*FlushedBlock
}

func (s *containerSink) flush(fb *FlushedBlock) error {
	s.blocks = append(s.blocks, fb)
	return nil
}

// buildContainer builds one page-container block over the given records,
// compressing each page with alg. The returned block is the fresh stored
// container + its validated parse.
func buildContainer(tb testing.TB, pageSize, blockSize int, alg format.Compression, want []expectedPageRow, bodies [][]byte) (*FlushedBlock, *RowsContainer) {
	tb.Helper()
	if pageSize <= 0 {
		pageSize = 16 << 10
	}
	if blockSize <= 0 {
		blockSize = 1 << 20
	}
	var sink containerSink
	b := NewRowsBuilder(1, 1, Config{BlockSize: blockSize, Compression: alg, Level: 0, Limits: DefaultLimits(), OnFlush: sink.flush})
	b.SetPageSize(pageSize)
	for i := range want {
		var body []byte
		if want[i].ct != format.ChangeDelete {
			body = bodies[i]
		}
		if err := b.Add(want[i].rowID, want[i].version, want[i].ct, body); err != nil {
			tb.Fatalf("add %d: %v", i, err)
		}
	}
	if err := b.Flush(); err != nil {
		tb.Fatalf("flush: %v", err)
	}
	if len(sink.blocks) != 1 {
		tb.Fatalf("got %d blocks, want 1", len(sink.blocks))
	}
	fb := sink.blocks[0]
	rc, err := ParseContainer(fb.Stored, fb.Header, DefaultLimits())
	if err != nil {
		tb.Fatalf("parse container: %v", err)
	}
	return fb, rc
}

func TestRowsContainerRoundTripMultiPage(t *testing.T) {
	schema := pageTestSchema()
	var want []expectedPageRow
	var bodies [][]byte
	// 300 records across several small pages, mixed change types, schema
	// version runs and unsorted IDs.
	for i := uint64(1); i <= 300; i++ {
		var ct format.ChangeType
		switch i % 5 {
		case 0:
			ct = format.ChangeDelete
		case 1, 2:
			ct = format.ChangeInsert
		default:
			ct = format.ChangeUpdate
		}
		version := uint32(1)
		if i >= 150 {
			version = 2
		}
		body := pageTestRow(t, schema, i)
		rec := expectedPageRow{rowID: 1000 - i, version: version, ct: ct} // descending IDs
		if ct != format.ChangeDelete {
			rec.bodyLen = len(body)
		}
		want = append(want, rec)
		bodies = append(bodies, body)
	}
	_, rc := buildContainer(t, 4<<10, 1<<20, format.CompressionNone, want, bodies)

	// Container-level metadata.
	if uint32(len(want)) != rc.Header.TotalRecords {
		t.Fatalf("total records %d, want %d", rc.Header.TotalRecords, len(want))
	}
	if rc.PageCount() < 2 {
		t.Fatalf("expected multiple pages, got %d", rc.PageCount())
	}

	// Sequential iterate: every record in call order, delete carries no body.
	idx := 0
	if err := rc.ForEach(func(rec codec.PageRecord) error {
		w := want[idx]
		if rec.RowID != w.rowID || rec.SchemaVersion != w.version || rec.ChangeType != w.ct {
			t.Fatalf("ForEach(%d) = {%d v%d ct%d}, want {%d v%d ct%d}", idx, rec.RowID, rec.SchemaVersion, rec.ChangeType, w.rowID, w.version, w.ct)
		}
		if w.ct == format.ChangeDelete {
			require.EqualValues(t, 0, len(rec.Body), "ForEach(%d): delete carries body", idx)
		} else if len(rec.Body) != w.bodyLen {
			t.Fatalf("ForEach(%d) body %d bytes, want %d", idx, len(rec.Body), w.bodyLen)
		}
		idx++
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
	if idx != len(want) {
		t.Fatalf("ForEach visited %d, want %d", idx, len(want))
	}

	// Random single-record access (the Get path) must match.
	for _, ord := range []uint32{0, 1, 50, 149, 250, 299} {
		rec, release, err := rc.RecordAt(ord)
		require.Nil(t, err, "RecordAt(%d): %v", ord, err)
		w := want[ord]
		if rec.RowID != w.rowID || rec.SchemaVersion != w.version || rec.ChangeType != w.ct {
			t.Fatalf("RecordAt(%d) = {%d v%d ct%d}, want {%d v%d ct%d}", ord, rec.RowID, rec.SchemaVersion, rec.ChangeType, w.rowID, w.version, w.ct)
		}
		if w.ct == format.ChangeDelete {
			require.EqualValues(t, 0, len(rec.Body), "RecordAt(%d): delete carries body", ord)
		} else if len(rec.Body) != w.bodyLen {
			t.Fatalf("RecordAt(%d) body %d bytes, want %d", ord, len(rec.Body), w.bodyLen)
		}
		release()
	}
}

func TestRowsContainerRoundTripZstd(t *testing.T) {
	schema := pageTestSchema()
	var want []expectedPageRow
	var bodies [][]byte
	for i := uint64(1); i <= 500; i++ {
		body := pageTestRow(t, schema, i)
		rec := expectedPageRow{rowID: i, version: 1, ct: format.ChangeInsert, bodyLen: len(body)}
		if i%7 == 0 {
			rec.ct = format.ChangeDelete
			rec.bodyLen = 0
		}
		want = append(want, rec)
		bodies = append(bodies, body)
	}
	fb, rc := buildContainer(t, 8<<10, 1<<20, format.CompressionZstd, want, bodies)
	// zstd-compressed StoredSize must be smaller than the raw sum.
	if int(fb.Header.RawSize) <= int(fb.Header.StoredSize) {
		t.Fatalf("zstd container stored %d not smaller than raw %d", fb.Header.StoredSize, fb.Header.RawSize)
	}
	idx := 0
	if err := rc.ForEach(func(rec codec.PageRecord) error {
		w := want[idx]
		if rec.RowID != w.rowID || rec.ChangeType != w.ct {
			t.Fatalf("ForEach(%d) = {%d ct%d}, want {%d ct%d}", idx, rec.RowID, rec.ChangeType, w.rowID, w.ct)
		}
		if w.ct != format.ChangeDelete && len(rec.Body) != w.bodyLen {
			t.Fatalf("ForEach(%d) body %d, want %d", idx, len(rec.Body), w.bodyLen)
		}
		idx++
		return nil
	}); err != nil {
		t.Fatalf("ForEach: %v", err)
	}
}

func TestRowsContainerOversizedPage(t *testing.T) {
	schema := pageTestSchema()
	big := make([]byte, 32<<10)
	for i := range big {
		big[i] = byte(i)
	}
	row := []codec.Value{
		codec.Uint64(1), codec.Int64(2), codec.Float64(0.5),
		codec.String("big"), codec.Bytes(big),
	}
	body, err := testCodec.EncodeInto(schema, row, nil)
	requireNoErr(t, err)
	// A single >16 KiB row in a 16 KiB page must form its own oversized page.
	var want []expectedPageRow
	want = append(want, expectedPageRow{rowID: 1, version: 1, ct: format.ChangeInsert, bodyLen: len(body)})
	_, rc := buildContainer(t, 16<<10, 1<<20, format.CompressionNone, want, [][]byte{body})
	if rc.Dir[0].Flags&1 == 0 {
		t.Fatalf("oversized page flag not set")
	}
	rec, release, err := rc.RecordAt(0)
	requireNoErr(t, err)
	if len(rec.Body) != len(body) {
		t.Fatalf("oversized row body %d, want %d", len(rec.Body), len(body))
	}
	release()
}

func TestRowsContainerCorruption(t *testing.T) {
	schema := pageTestSchema()
	var want []expectedPageRow
	var bodies [][]byte
	for i := uint64(1); i <= 100; i++ {
		body := pageTestRow(t, schema, i)
		rec := expectedPageRow{rowID: i, version: 1, ct: format.ChangeInsert, bodyLen: len(body)}
		want = append(want, rec)
		bodies = append(bodies, body)
	}
	fb, _ := buildContainer(t, 16<<10, 1<<20, format.CompressionNone, want, bodies)
	payload := fb.Stored

	// Truncation at region boundaries must fail cleanly, never panic.
	for _, cut := range []int{0, 1, format.RowsBlockHeaderSize - 1, format.RowsBlockHeaderSize, format.RowsBlockHeaderSize + 24, len(payload) - 2, len(payload) - 1} {
		if _, err := ParseContainer(payload[:cut], fb.Header, DefaultLimits()); err == nil {
			t.Fatalf("truncated container at %d accepted", cut)
		}
	}

	// Single-bit flips in the header/directory region must be caught by the
	// container CRC (or earlier magic/geometry validation).
	for _, off := range []int{2, 12, 30, format.RowsBlockHeaderSize + 5} {
		bad := append([]byte(nil), payload...)
		bad[off] ^= 0x01
		if _, err := ParseContainer(bad, fb.Header, DefaultLimits()); err == nil {
			t.Fatalf("bit flip at %d accepted", off)
		}
	}

	// 伪造 ItemCount（TotalRecords 与块头不符）由 container_geometry_test.go
	// 的 "item-count-disagrees" 用例覆盖，并额外断言诊断文本 "total records"。
}
