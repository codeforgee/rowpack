package block

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// captureBuilder builds rows into a capture sink and returns the last flushed
// raw payload.
func captureBuilder(t *testing.T, compress fileformat.Compression, add func(b *RowsBlockBuilder) error) ([]capturedBlock, *RowsBlockBuilder) {
	t.Helper()
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, compress, 0, DefaultLimits(), s.flush)
	require.NoError(t, add(b))
	return s.blocks, b
}

func TestRowsBuilderDeletePendingRawPayload(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	require.Equal(t, 0, b.Pending(), "initial Pending = %d", b.Pending())
	require.NoError(t, b.Add(10, 1, fileformat.ChangeInsert, []byte("row-ten")))
	require.NoError(t, b.Delete(11, 1))
	require.NoError(t, b.Add(12, 1, fileformat.ChangeInsert, []byte("row-twelve")))
	require.Equal(t, 3, b.Pending(), "Pending = %d, want 3", b.Pending())
	// RawPayload builds without flushing; Pending must be unchanged and the
	// next Flush must produce identical bytes.
	raw1 := append([]byte(nil), b.RawPayload()...)
	require.Equal(t, 3, b.Pending(), "RawPayload flushed pending records")
	require.NoError(t, b.Flush())
	require.Equal(t, 0, b.Pending(), "Pending after Flush = %d", b.Pending())
	require.Len(t, s.blocks, 1, "flushed %d blocks", len(s.blocks))
	require.True(t, bytes.Equal(raw1, s.blocks[0].payload), "RawPayload differs from flushed payload")
	// Flush with nothing pending is a no-op.
	require.NoError(t, b.Flush())
	require.Len(t, s.blocks, 1, "empty Flush emitted a block")

	// Payload semantics: tombstones carry no row bytes and zero CRC.
	p, err := ParseRowsPayload(s.blocks[0].payload, 3)
	require.NoError(t, err)
	require.Nil(t, p.RowBytes(1), "DELETE RowBytes = %q", p.RowBytes(1))
	require.Equal(t, "row-ten", string(p.RowBytes(0)), "RowBytes(0) = %q", p.RowBytes(0))
	require.Equal(t, fileformat.CRC32C([]byte("row-ten")), p.RowCRC(0), "RowCRC(0) = %d", p.RowCRC(0))
	require.Zero(t, p.RowCRC(1), "DELETE RowCRC = %d", p.RowCRC(1))
	// Entry fields agree with the directory.
	e := p.Entries[1]
	require.Equal(t, uint64(11), e.RowID)
	require.Equal(t, fileformat.ChangeDelete, e.ChangeType, "DELETE entry = %+v", e)
}

func TestOversizedRowOwnBlock(t *testing.T) {
	// A row whose record does not fit the block size must land alone in its
	// own block even when pending records exist.
	var s captureSink
	// records buffer holds only record bytes (24-byte header + body), so a
	// blockSize of 80 fits one 8-byte row but forces a 64-byte row into the
	// oversized path.
	b := NewRowsBlockBuilder(1, 7, 80, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	require.NoError(t, b.Add(1, 1, fileformat.ChangeInsert, make([]byte, 8)))
	require.NoError(t, b.Add(2, 1, fileformat.ChangeInsert, make([]byte, 64))) // oversized
	require.NoError(t, b.Flush())
	require.Len(t, s.blocks, 2, "blocks = %d, want 2 (one small, one oversized)", len(s.blocks))
	require.Equal(t, uint32(1), s.blocks[1].header.ItemCount, "oversized block ItemCount = %d", s.blocks[1].header.ItemCount)
}

func TestRowsBuilderLimitErrors(t *testing.T) {
	limits := DefaultLimits()
	// Row exceeds MaxRawBytes.
	var s1 captureSink
	b1 := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, limits, s1.flush)
	require.Error(t, b1.Add(1, 1, fileformat.ChangeInsert, make([]byte, limits.MaxRawBytes+1)), "row over MaxRawBytes accepted")
	// Block payload exceeds MaxRawBytes (offset+length check in append): the
	// row itself fits the limit but the accumulated records do not.
	var s2 captureSink
	small := Limits{MaxRawBytes: 200, MaxStoredBytes: 1 << 20}
	b2 := NewRowsBlockBuilder(1, 7, 1<<20, fileformat.CompressionNone, 0, small, s2.flush)
	require.NoError(t, b2.Add(1, 1, fileformat.ChangeInsert, make([]byte, 100)))
	require.Error(t, b2.Add(2, 1, fileformat.ChangeInsert, make([]byte, 100)), "payload over MaxRawBytes accepted")
}

func TestParseRowsDirectoryAndIndexRowBytes(t *testing.T) {
	raw := func() []byte {
		var s captureSink
		b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
		_ = b.Add(5, 1, fileformat.ChangeInsert, []byte("five"))
		_ = b.Delete(6, 1)
		_ = b.Add(7, 1, fileformat.ChangeInsert, []byte("seven"))
		require.NoError(t, b.Flush())
		return s.blocks[0].payload
	}()

	idx, err := ParseRowsDirectory(raw, 3, nil)
	require.NoError(t, err)
	require.Len(t, idx.Entries, 3, "index = %+v", idx.Header)
	require.Equal(t, uint32(3), idx.Header.ItemCount, "index = %+v", idx.Header)
	require.Equal(t, "five", string(idx.RowBytes(0)), "RowBytes(0) = %q", idx.RowBytes(0))
	require.Nil(t, idx.RowBytes(1), "DELETE RowBytes = %q", idx.RowBytes(1))
	require.Equal(t, "seven", string(idx.RowBytes(2)), "RowBytes(2) = %q", idx.RowBytes(2))

	// Rejections.
	_, err = ParseRowsDirectory(raw, 4, nil)
	require.Error(t, err, "item count mismatch accepted")
	_, err = ParseRowsDirectory(raw[:fileformat.RowsPayloadHeaderSize+2*fileformat.RowDirectoryEntrySize], 3, nil)
	require.Error(t, err, "truncated directory accepted")
	// Records region mismatch: header claims more record bytes than present
	// (RecordsBytes is a uint64 at payload offset 24).
	bad := append([]byte(nil), raw...)
	bad[31] = 0xFF
	_, err = ParseRowsDirectory(bad, 3, nil)
	require.Error(t, err, "records region mismatch accepted")
	// Out-of-bounds record offset in the directory (entry[1].RecordOffset is
	// at payload header 32 + 1*24 + 8).
	bad = append([]byte(nil), raw...)
	bad[32+24+8+3] = 0x7F
	_, err = ParseRowsDirectory(bad, 3, nil)
	require.Error(t, err, "out-of-bounds record accepted")
}

func TestParseRowAt(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	_ = b.Add(5, 1, fileformat.ChangeInsert, []byte("five"))
	_ = b.Delete(6, 1)
	_ = b.Add(7, 1, fileformat.ChangeInsert, []byte("seven"))
	require.NoError(t, b.Flush())
	raw := s.blocks[0].payload

	ref, err := ParseRowAt(raw, 3, 2)
	require.NoError(t, err)
	require.Equal(t, uint64(7), ref.Entry.RowID, "ParseRowAt(2) = %+v %q", ref.Entry, ref.Row)
	require.Equal(t, "seven", string(ref.Row), "ParseRowAt(2) = %+v %q", ref.Entry, ref.Row)
	require.Equal(t, fileformat.CRC32C([]byte("seven")), ref.Header.RowCRC32C, "record CRC = %d", ref.Header.RowCRC32C)
	// DELETE: row bytes nil, tombstone invariants hold.
	ref, err = ParseRowAt(raw, 3, 1)
	require.NoError(t, err)
	require.Nil(t, ref.Row, "DELETE ref = %+v", ref)
	require.Equal(t, fileformat.RowEncodingNone, ref.Header.RowEncoding, "DELETE ref = %+v", ref)

	// Rejections.
	_, err = ParseRowAt(raw, 4, 0)
	require.Error(t, err, "item count mismatch accepted")
	_, err = ParseRowAt(raw, 3, 3)
	require.Error(t, err, "ordinal out of range accepted")
	// Corrupted directory entry: record header no longer agrees (entry[0]
	// starts at payload offset 32; RowID low byte is entry offset 0).
	bad := append([]byte(nil), raw...)
	bad[32] ^= 0xFF
	_, err = ParseRowAt(bad, 3, 0)
	require.Error(t, err, "header/directory mismatch accepted")
	// Records region mismatch (RecordsBytes at payload offset 24).
	bad = append([]byte(nil), raw...)
	bad[31] = 0xFF
	_, err = ParseRowAt(bad, 3, 0)
	require.Error(t, err, "records region mismatch accepted")
}

func TestParseRowsPayloadRejections(t *testing.T) {
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
	_ = b.Add(5, 1, fileformat.ChangeInsert, []byte("five"))
	_ = b.Delete(6, 1)
	require.NoError(t, b.Flush())
	raw := s.blocks[0].payload

	_, err := ParseRowsPayload(raw, 5)
	require.Error(t, err, "item count mismatch accepted")
	// Directory exceeds payload.
	_, err = ParseRowsPayload(raw[:fileformat.RowsPayloadHeaderSize], 2)
	require.Error(t, err, "truncated payload accepted")
	// Records bytes overhang (RecordsBytes is a uint64 at payload offset 24).
	bad := append([]byte(nil), raw...)
	bad[31] = 0xFF
	_, err = ParseRowsPayload(bad, 2)
	require.Error(t, err, "records overhang accepted")
	// Directory entry claims a record beyond the records region
	// (entry[0].RecordOffset at payload offset 32+8).
	bad = append([]byte(nil), raw...)
	bad[32+8+3] = 0x7F
	_, err = ParseRowsPayload(bad, 2)
	require.Error(t, err, "out-of-bounds record accepted")
	// Record claiming DELETE while carrying row bytes: both the directory
	// entry (ChangeType at entry offset 16) and the record header (record
	// byte 12) must agree on DELETE while RowLength stays nonzero.
	bad = append([]byte(nil), raw...)
	recOff := fileformat.RowsPayloadHeaderSize + 2*fileformat.RowDirectoryEntrySize
	bad[32+16] = byte(fileformat.ChangeDelete)
	bad[recOff+12] = byte(fileformat.ChangeDelete)
	_, err = ParseRowsPayload(bad, 2)
	require.Error(t, err, "DELETE record with row bytes accepted")
}

// TestParseRowsDirectoryReuse verifies the directory entry slice argument is
// reused across blocks: parsing a second payload with the same backing slice
// overwrites entries correctly and validates each payload independently.
func TestParseRowsDirectoryReuse(t *testing.T) {
	mkRaw := func(ids []uint64, _ []string) []byte {
		var s captureSink
		b := NewRowsBlockBuilder(1, 7, 64<<10, fileformat.CompressionNone, 0, DefaultLimits(), s.flush)
		for i, id := range ids {
			_ = b.Add(id, 1, fileformat.ChangeInsert, []byte(fmt.Sprintf("row-%d", id)))
			_ = i
		}
		require.NoError(t, b.Flush())
		return s.blocks[0].payload
	}
	rawA := mkRaw([]uint64{1, 2}, nil)
	rawB := mkRaw([]uint64{100, 200, 300}, nil)

	var reuse []fileformat.RowDirectoryEntry
	idxA, err := ParseRowsDirectory(rawA, 2, reuse)
	require.NoError(t, err)
	reuse = idxA.Entries
	require.Len(t, idxA.Entries, 2, "A: entries = %d", len(idxA.Entries))
	// Reuse the same backing slice for a payload with MORE entries.
	idxB, err := ParseRowsDirectory(rawB, 3, reuse)
	require.NoError(t, err)
	require.Len(t, idxB.Entries, 3, "B: entries = %d", len(idxB.Entries))
	require.Equal(t, "row-300", string(idxB.RowBytes(2)), "B RowBytes(2) = %q", idxB.RowBytes(2))
	// Backing slice grew past the original capacity: A's alias is stale but
	// the next parse with the grown slice must still work.
	idxA2, err := ParseRowsDirectory(rawA, 2, idxB.Entries)
	require.NoError(t, err)
	require.Equal(t, "row-2", string(idxA2.RowBytes(1)), "A2 RowBytes(1) = %q", idxA2.RowBytes(1))
}
