package rowpack

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// restampFooterCRC recomputes the footer CRC32C after callers patched fields.
func restampFooterCRC(fb []byte) {
	binary.LittleEndian.PutUint32(fb[format.SnapshotFooterCRC32COffset:], 0)
	binary.LittleEndian.PutUint32(fb[format.SnapshotFooterCRC32COffset:], format.CRC32C(fb))
}

// readSpan reads the IndexTxn span [start, end) of the data file.
func readSpan(t *testing.T, base string, start, end int64) []byte {
	t.Helper()
	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	buf := make([]byte, end-start)
	_, err = f.ReadAt(buf, start)
	require.NoError(t, err)
	return buf
}

func writeFileSpan(t *testing.T, base string, off int64, b []byte) {
	t.Helper()
	f, err := os.OpenFile(base+".rpk", os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(b, off)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// restampSpanCRC writes the span's CRC into the footer's IndexTxnCRC32C field
// and restamps the footer CRC, keeping the file internally consistent.
func restampSpanCRC(t *testing.T, base string, span []byte, footerOff int64) {
	t.Helper()
	fb := readSpan(t, base, footerOff, footerOff+format.SnapshotFooterSize)
	binary.LittleEndian.PutUint32(fb[132:], format.CRC32C(span))
	restampFooterCRC(fb)
	writeFileSpan(t, base, footerOff, fb)
}

// TestOpenDataFileTooSmall pins the tiny-file contract: the fixed header is
// read first, so a short file fails there ("read data header: EOF") before
// recovery's own size guard could run.
func TestOpenDataFileTooSmall(t *testing.T) {
	base := filepath.Join(t.TempDir(), "tiny")
	require.NoError(t, os.WriteFile(base+".rpk", make([]byte, 50), 0o644))
	_, err := Open(base, Options{})
	require.ErrorContains(t, err, "read data header")
}

// TestTailPartialStructures feeds crafted tails whose first structure magic is
// recognizable but whose bytes end (or turn to garbage) before the structure
// completes: the scanner must classify each as an uncommitted tail, truncate
// it at open, and keep both committed snapshots intact.
func TestTailPartialStructures(t *testing.T) {
	cases := []struct {
		name string
		tail func(t *testing.T, base string, sh []byte)
	}{
		// Block magic present, fewer bytes than BlockHeaderSize.
		{"partial-block-header", func(t *testing.T, base string, sh []byte) {
			tail := append(append([]byte{}, sh...), []byte(format.MagicBlockHdr)[:8]...)
			tail = append(tail, 1, 2, 3, 4, 5)
			appendTail(t, base, tail)
		}},
		// IndexTxn magic present, fewer bytes than IndexTxnHeaderSize.
		{"partial-txn-header", func(t *testing.T, base string, sh []byte) {
			tail := append(append([]byte{}, sh...), []byte(format.MagicIndexTxnHdr)...)
			tail = append(tail, 9, 8, 7, 6, 5, 4, 3, 2, 1, 0)
			appendTail(t, base, tail)
		}},
		// Full-size but garbage IndexTxn footer (header itself is valid with
		// BodyBytes=0, so the scanner reaches the footer Unmarshal).
		{"garbage-txn-footer", func(t *testing.T, base string, sh []byte) {
			committed, _, err := scanCommitted(t, base)
			require.NoError(t, err)
			last := committed[len(committed)-1]
			hdr := readSpan(t, base, last.txnStart, last.txnStart+format.IndexTxnHeaderSize)
			binary.LittleEndian.PutUint64(hdr[64:], 0) // BodyBytes = 0
			binary.LittleEndian.PutUint32(hdr[72:], 0)
			binary.LittleEndian.PutUint32(hdr[72:], format.CRC32C(hdr))
			tail := append(append([]byte{}, sh...), hdr...)
			tail = append(tail, make([]byte, format.IndexTxnFooterSize)...) // zero footer: bad magic
			appendTail(t, base, tail)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			base := filepath.Join(t.TempDir(), "tail")
			db, err := Create(base, Options{BlockSize: 1024})
			require.NoError(t, err)
			buildTwoSnapshots(t, db)
			require.NoError(t, db.Close())

			// A valid SnapshotHeader (fresh id) starts the tail so the walk
			// enters the structure loop before hitting the broken bytes.
			committed, _, err := scanCommitted(t, base)
			require.NoError(t, err)
			last := committed[len(committed)-1]
			sh := readSpan(t, base, last.start, last.start+format.SnapshotHeaderSize)
			binary.LittleEndian.PutUint64(sh[16:], 9999)
			binary.LittleEndian.PutUint32(sh[88:], 0)
			binary.LittleEndian.PutUint32(sh[88:], format.CRC32C(sh))
			tc.tail(t, base, sh)

			db2, err := Open(base, Options{BlockSize: 1024})
			require.NoError(t, err)
			t.Cleanup(func() { db2.Close() })
			rep := db2.Stats().Recovery
			require.True(t, rep.Performed, "tail must be reported as repaired")
			require.Greater(t, rep.DataTailIgnored, uint64(0))
			snaps, err := db2.ListSnapshots(ctx)
			require.NoError(t, err)
			require.Len(t, snaps, 2, "committed snapshots survive the tail")
			_, err = db2.Get(ctx, 2, "users", 7, nil)
			require.NoError(t, err)
		})
	}
}

// TestBrokenTxnHeaderIsMidFileCorruption: a txn whose header fails to
// unmarshal while the footer-bound span CRC passes is tampering, not a torn
// tail — the footer is the commit authority, so open must report mid-file
// corruption instead of silently rebuilding. (The rebuild fallback for this
// shape in readIndexTxn is only reachable when the file changes between the
// scan and the read — a concurrent-writer race.)
func TestBrokenTxnHeaderIsMidFileCorruption(t *testing.T) {
	base := filepath.Join(t.TempDir(), "hdrsnap")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	last := committed[len(committed)-1]
	require.NoError(t, db.Close())

	// Break the header's own CRC field (magic stays recognizable so the
	// scanner reaches the unmarshal), then restamp the span + footer CRCs so
	// everything around the bad header is internally consistent.
	span := readSpan(t, base, last.txnStart, last.txnEnd)
	binary.LittleEndian.PutUint32(span[72:], 0xBADC0DE)
	restampSpanCRC(t, base, span, last.footerOff)
	writeFileSpan(t, base, last.txnStart, span)

	_, err = Open(base, Options{BlockSize: 1024})
	require.ErrorContains(t, err, "mid-file corruption")
}

// TestZeroIDFooterIsNotCommitAuthority: a snapshot whose header is broken is
// only mid-file corruption if a later footer with a non-zero SnapshotID
// validates. A footer with SnapshotID 0 is not a commit authority, so the
// region is an uncommitted tail and open truncates it.
func TestZeroIDFooterIsNotCommitAuthority(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(t.TempDir(), "zeroid")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	last := committed[len(committed)-1]
	require.NoError(t, db.Close())

	// Break snapshot 2's header so the walk stops at its start...
	hdr := readSpan(t, base, last.start, last.start+format.SnapshotHeaderSize)
	copy(hdr[0:8], "RPKSNAXX")
	writeFileSpan(t, base, last.start, hdr)
	// ...and demote its footer to a zero-id non-authority.
	fb := readSpan(t, base, last.footerOff, last.footerOff+format.SnapshotFooterSize)
	binary.LittleEndian.PutUint64(fb[16:], 0)
	restampFooterCRC(fb)
	writeFileSpan(t, base, last.footerOff, fb)

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err, "no valid footer follows: the region is a tail")
	t.Cleanup(func() { db2.Close() })
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 1, "snapshot 2 had no valid commit authority")
	require.Equal(t, uint64(1), snaps[0].ID)
	_, err = db2.Get(ctx, 1, "users", 7, nil)
	require.NoError(t, err)
}

// TestRebuildBoundsHugeFooterCounts: footer counts are untrusted input. A
// forged footer claiming billions of records must not turn into a giant
// preallocation; the rebuild caps the hint and reads the real blocks.
func TestRebuildBoundsHugeFooterCounts(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(t.TempDir(), "hugecounts")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	last := committed[len(committed)-1]
	require.NoError(t, db.Close())

	// Corrupt one body byte (span CRC no longer matches) to force the rebuild
	// path, then forge huge counts with a consistent footer CRC.
	span := readSpan(t, base, last.txnStart, last.txnEnd)
	span[format.IndexTxnHeaderSize+10] ^= 0xFF
	writeFileSpan(t, base, last.txnStart, span)

	fb := readSpan(t, base, last.footerOff, last.footerOff+format.SnapshotFooterSize)
	binary.LittleEndian.PutUint32(fb[96:], ^uint32(0))           // BlockCount
	binary.LittleEndian.PutUint32(fb[100:], ^uint32(0))          // MetadataBlockCount
	binary.LittleEndian.PutUint64(fb[104:], 1<<40)               // RowRecordCount
	binary.LittleEndian.PutUint32(fb[132:], format.CRC32C(span)) // IndexTxnCRC32C (still mismatched)
	restampFooterCRC(fb)
	writeFileSpan(t, base, last.footerOff, fb)

	db2, err := Open(base, Options{BlockSize: 1024})
	require.NoError(t, err, "forged counts must be bounded, not OOM")
	t.Cleanup(func() { db2.Close() })
	rep := db2.Stats().Recovery
	require.GreaterOrEqual(t, rep.SnapshotsRebuilt, uint64(1))
	snaps, err := db2.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, snaps, 2)
	_, err = db2.Get(ctx, 2, "users", 7, nil)
	require.NoError(t, err)
}

func mustUint64(t *testing.T, r Row, i int) uint64 {
	t.Helper()
	v, ok := r[i].Uint64()
	require.True(t, ok)
	return v
}

// TestRebuildChainInvalidAfterParentForge: a committed DELTA whose IndexTxn
// is corrupt falls back to the block rebuild; if its snapshot header has been
// forged to claim a parent that was never committed, the rebuilt txn fails
// Apply and open must refuse with "chain invalid after rebuild" instead of
// silently grafting the snapshot onto the wrong parent.
func TestRebuildChainInvalidAfterParentForge(t *testing.T) {
	base := filepath.Join(t.TempDir(), "chainforge")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	committed, _, err := db.scanDataFile()
	require.NoError(t, err)
	last := committed[len(committed)-1]
	require.NoError(t, db.Close())

	// Corrupt the txn body (footer span CRC no longer matches -> rebuild) and
	// forge the snapshot header's parent to a snapshot that does not exist.
	span := readSpan(t, base, last.txnStart, last.txnEnd)
	span[format.IndexTxnHeaderSize+10] ^= 0xFF
	writeFileSpan(t, base, last.txnStart, span)

	hdr := readSpan(t, base, last.start, last.start+format.SnapshotHeaderSize)
	binary.LittleEndian.PutUint64(hdr[24:], 999)
	binary.LittleEndian.PutUint32(hdr[88:], 0)
	binary.LittleEndian.PutUint32(hdr[88:], format.CRC32C(hdr))
	writeFileSpan(t, base, last.start, hdr)

	_, err = Open(base, Options{BlockSize: 1024})
	require.ErrorContains(t, err, "chain invalid after rebuild")
}
