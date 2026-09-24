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

// This file pins walkSnapshot's per-structure rejection arms, one appended
// tail each. Every tail starts with a valid (re-CRC'd) SnapshotHeader for a
// fresh snapshot id so the walk enters the structure loop before hitting the
// specific break — the walkTail scanner would otherwise dismiss the tail as
// "not a snapshot header" without classifying it.

// writeWalkHeader mints a valid SnapshotHeader for snapshot id 9999.
func writeWalkHeader(t *testing.T, base string) [format.SnapshotHeaderSize]byte {
	t.Helper()
	committed, _, err := scanCommitted(t, base)
	require.NoError(t, err)
	last := committed[len(committed)-1]
	var sh [format.SnapshotHeaderSize]byte
	fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
	require.NoError(t, err)
	_, err = fr.ReadAt(sh[:], last.start)
	require.NoError(t, err)
	require.NoError(t, fr.Close())
	binary.LittleEndian.PutUint64(sh[16:], 9999)
	// CRC 规则：字段置零后计算，再写回。
	binary.LittleEndian.PutUint32(sh[88:], 0)
	binary.LittleEndian.PutUint32(sh[88:], format.CRC32C(sh[:]))
	return sh
}

// validBlockHeaderTemplate reads a real block header from the file.
func validBlockHeaderTemplate(t *testing.T, base string) [format.BlockHeaderSize]byte {
	t.Helper()
	committed, _, err := scanCommitted(t, base)
	require.NoError(t, err)
	last := committed[len(committed)-1]
	var tmpl [format.BlockHeaderSize]byte
	fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
	require.NoError(t, err)
	_, err = fr.ReadAt(tmpl[:], last.blocksStart)
	require.NoError(t, err)
	require.NoError(t, fr.Close())
	return tmpl
}

// validTxnHeaderTemplate reads a real IndexTxn header from the file.
func validTxnHeaderTemplate(t *testing.T, base string) [format.IndexTxnHeaderSize]byte {
	t.Helper()
	committed, _, err := scanCommitted(t, base)
	require.NoError(t, err)
	last := committed[len(committed)-1]
	var hdr [format.IndexTxnHeaderSize]byte
	fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
	require.NoError(t, err)
	_, err = fr.ReadAt(hdr[:], last.txnStart)
	require.NoError(t, err)
	require.NoError(t, fr.Close())
	return hdr
}

func mustPrepareWalkFile(t *testing.T) string {
	t.Helper()
	base := filepath.Join(tmpdb(t), "walk")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	buildTwoSnapshots(t, db)
	require.NoError(t, db.Close())
	return base
}

func TestWalkSnapshotRejectsBrokenTails(t *testing.T) {
	cases := []struct {
		name string
		tail func(t *testing.T, base string) []byte
	}{
		// Fewer than SnapshotHeaderSize bytes remain after the magic.
		{"half header", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			return sh[:48]
		}},
		// Header parsed, then fewer than 8 bytes remain.
		{"five trailing bytes", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			return append(sh[:], 1, 2, 3, 4, 5)
		}},
		// Block magic but the header fails its own validation.
		{"broken block header", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			tmpl := validBlockHeaderTemplate(t, base)
			binary.LittleEndian.PutUint32(tmpl[12:], 999) // wrong size word
			binary.LittleEndian.PutUint32(tmpl[52:], format.CRC32C(tmpl[:]))
			return append(sh[:], tmpl[:]...)
		}},
		// Block payload shorter than StoredSize claims.
		{"truncated block payload", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			tmpl := validBlockHeaderTemplate(t, base)
			binary.LittleEndian.PutUint32(tmpl[40:], 1<<20) // StoredSize huge
			binary.LittleEndian.PutUint32(tmpl[52:], format.CRC32C(tmpl[:]))
			return append(sh[:], tmpl[:]...)
		}},
		// Txn magic but the header fails validation.
		{"broken txn header", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			hdr := validTxnHeaderTemplate(t, base)
			binary.LittleEndian.PutUint32(hdr[8:], 999) // wrong size word
			binary.LittleEndian.PutUint32(hdr[72:], format.CRC32C(hdr[:]))
			return append(sh[:], hdr[:]...)
		}},
		// Txn body longer than the remaining file.
		{"truncated txn body", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			hdr := validTxnHeaderTemplate(t, base)
			binary.LittleEndian.PutUint64(hdr[64:], 1<<40) // BodyBytes huge
			binary.LittleEndian.PutUint32(hdr[72:], format.CRC32C(hdr[:]))
			return append(sh[:], hdr[:]...)
		}},
		// Txn magic, valid header, but the footer region is truncated.
		{"truncated txn footer", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			hdr := validTxnHeaderTemplate(t, base)
			binary.LittleEndian.PutUint64(hdr[64:], 0) // empty body
			binary.LittleEndian.PutUint32(hdr[72:], format.CRC32C(hdr[:]))
			// Header says body=0 so a footer must follow immediately; supply
			// only half of one.
			ftr := make([]byte, format.SnapshotFooterSize/2)
			return append(append(sh[:], hdr[:]...), ftr...)
		}},
		// Footer magic but fewer than SnapshotFooterSize bytes follow.
		{"interrupted footer write", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			ftr := make([]byte, 8+16)
			copy(ftr, format.MagicSnapshotFtr)
			return append(sh[:], ftr...)
		}},
		// Footer magic + full length but the CRC fails.
		{"broken footer CRC", func(t *testing.T, base string) []byte {
			sh := writeWalkHeader(t, base)
			ftr := make([]byte, format.SnapshotFooterSize)
			copy(ftr, format.MagicSnapshotFtr)
			return append(sh[:], ftr...)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := mustPrepareWalkFile(t)
			appendTail(t, base, tc.tail(t, base))

			db, err := Open(base, Options{})
			require.NoError(t, err)
			defer func() { _ = db.Close() }()
			// The broken tail must be ignored as an uncommitted tail: both
			// original snapshots stay readable.
			snaps, err := db.ListSnapshots(context.Background())
			require.NoError(t, err)
			require.Len(t, snaps, 2, "broken tail must not hide committed snapshots")
		})
	}
}

// TestWalkSnapshotRejectsFooterIDMismatch pins the one mid-file corruption
// error inside walkSnapshot: a footer whose snapshot id differs from its
// header's cannot be an interrupted commit.
func TestWalkSnapshotRejectsFooterIDMismatch(t *testing.T) {
	base := mustPrepareWalkFile(t)

	// Tail: valid header (id 9999) + a fully valid footer for id 8888.
	sh := writeWalkHeader(t, base)
	committed, _, err := scanCommitted(t, base)
	require.NoError(t, err)
	last := committed[len(committed)-1]
	var ftr [format.SnapshotFooterSize]byte
	fr, err := os.OpenFile(base+".rpk", os.O_RDONLY, 0)
	require.NoError(t, err)
	_, err = fr.ReadAt(ftr[:], last.footerOff)
	require.NoError(t, err)
	require.NoError(t, fr.Close())
	binary.LittleEndian.PutUint64(ftr[16:], 8888)
	binary.LittleEndian.PutUint32(ftr[136:], 0)
	binary.LittleEndian.PutUint32(ftr[136:], format.CRC32C(ftr[:]))

	appendTail(t, base, append(sh[:], ftr[:]...))

	_, err = Open(base, Options{})
	require.Error(t, err)
	require.ErrorContains(t, err, "footer snapshot 8888 != header snapshot 9999")
}
