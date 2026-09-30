package block

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
)

// plainReaderAt is an io.ReaderAt without the viewer interface, forcing the
// copy path in ReadAtBlock.
func plainReaderAt(data []byte) io.ReaderAt { return bytes.NewReader(data) }

func TestReaderReadAtBlockCopyNonViewer(t *testing.T) {
	raw := []byte("copy-path block payload")
	compressed, err := Compress(format.CompressionZstd, 3, raw)
	if err != nil {
		t.Fatal(err)
	}
	h := format.BlockHeader{
		BlockKind:   format.BlockKindRows,
		Compression: format.CompressionZstd,
		SnapshotID:  1,
		TableID:     2,
		ItemCount:   1,
		RawSize:     uint32(len(raw)),
		StoredSize:  uint32(len(compressed)),
		RawCRC32C:   format.CRC32C(raw),
	}
	hdr := make([]byte, format.BlockHeaderSize)
	if err := h.MarshalTo(hdr); err != nil {
		t.Fatal(err)
	}
	buf := append(append([]byte(nil), hdr...), compressed...)

	limits := Limits{MaxStoredBytes: 1 << 20, MaxRawBytes: 1 << 20}
	r := NewReader(plainReaderAt(buf), limits)
	blk, err := r.ReadAtBlock(0)
	require.NoError(t, err, "ReadAtBlock")
	if !bytes.Equal(blk.Raw, raw) {
		t.Fatalf("raw mismatch: %q", blk.Raw)
	}

	// Truncated stored payload surfaces a read error.
	if _, err := r.ReadAtBlock(int64(len(buf))); err == nil {
		t.Fatal("read past EOF should error")
	}

	// CRC mismatch is caught on the copy path too.
	bad := append([]byte(nil), buf...)
	bad[format.BlockHeaderSize] ^= 0xFF
	r2 := NewReader(plainReaderAt(bad), limits)
	if _, err := r2.ReadAtBlock(0); err == nil {
		t.Fatal("corrupt payload should error")
	}
}

func TestReaderSetDecrypterClears(t *testing.T) {
	r := NewReader(plainReaderAt(nil), DefaultLimits())
	r.SetDecrypter(nil) // must not panic
}

func TestRowsContainerAccountingHooks(t *testing.T) {
	_, want := []expectedPageRow{}, 0
	_ = want
	schema := pageTestSchema()
	var rows []expectedPageRow
	var bodies [][]byte
	for i := 1; i <= 8; i++ {
		rows = append(rows, expectedPageRow{rowID: uint64(i), version: 1, ct: format.ChangeInsert, bodyLen: 8})
		bodies = append(bodies, []byte{byte(i), 0, 0, 0, 0, 0, 0, 0})
	}
	_ = schema
	fb, rc := buildContainer(t, 0, 0, format.CompressionNone, rows, bodies)

	// RecordsRegionStart: header + directory.
	if got := rc.RecordsRegionStart(); got != format.RowsBlockHeaderSize+len(rc.Dir)*format.RowsPageDirEntrySize {
		t.Fatalf("RecordsRegionStart = %d", got)
	}
	// StoredLen covers the whole plaintext container.
	if got, want := rc.StoredLen(), len(fb.Stored); got != want {
		t.Fatalf("StoredLen = %d, want %d", got, want)
	}

	// Cache accounting: the callback fires as pages are memoized, so it must
	// be installed before the first page load.
	var reported int64
	rc.SetCacheAccounting(func(delta int64) { reported += delta })

	var n int
	err := rc.ForEach(func(codec.PageRecord) error { n++; return nil })
	require.NoError(t, err, "ForEach")
	if n != len(rows) {
		t.Fatalf("ForEach visited %d records, want %d", n, len(rows))
	}
	if reported == 0 {
		t.Fatal("cache accounting callback never fired")
	}

	if rc.RetainedLen() <= 0 {
		t.Fatalf("RetainedLen = %d", rc.RetainedLen())
	}

	// PageScratch returns memoized pages with a no-op release.
	p, release, err := rc.PageScratch(0)
	require.NoError(t, err, "PageScratch")
	release()
	if p == nil || len(p.raw) == 0 {
		t.Fatal("PageScratch returned an empty page")
	}
}

func TestRowsContainerForEachPropagatesError(t *testing.T) {
	var rows []expectedPageRow
	var bodies [][]byte
	for i := 1; i <= 4; i++ {
		rows = append(rows, expectedPageRow{rowID: uint64(i), version: 1, ct: format.ChangeInsert, bodyLen: 4})
		bodies = append(bodies, []byte{1, 2, 3, 4})
	}
	_, rc := buildContainer(t, 0, 0, format.CompressionNone, rows, bodies)
	sentinel := errPageTruncated
	err := rc.ForEach(func(codec.PageRecord) error { return sentinel })
	require.Error(t, err, "ForEach must propagate the callback error")
}

func TestRowsPageBuilderResetAndDecodedIDs(t *testing.T) {
	var want []expectedPageRow
	var bodies [][]byte
	for i := 1; i <= 5; i++ {
		want = append(want, expectedPageRow{rowID: uint64(i) * 10, version: 2, ct: format.ChangeInsert, bodyLen: 4})
		bodies = append(bodies, []byte{9, 9, 9, 9})
	}
	page := buildAndVerifyPage(t, 16<<10, want, bodies)

	for i, w := range want {
		got := page.ids[i]
		require.Equal(t, w.rowID, got, "decoded id %d = %d, want %d", i, got, w.rowID)
	}

	// Reset clears the builder for reuse.
	b := NewPageBuilder(16 << 10)
	for i := range want {
		if err := b.Add(want[i].rowID, want[i].version, want[i].ct, bodies[i]); err != nil {
			t.Fatal(err)
		}
	}
	if b.countRows() != uint32(len(want)) {
		t.Fatalf("count %d, want %d", b.countRows(), len(want))
	}
	if _, err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	b.reset()
	if b.countRows() != 0 {
		t.Fatalf("builder not empty after reset: %d rows", b.countRows())
	}
}
