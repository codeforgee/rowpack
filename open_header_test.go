package rowpack

import (
	"context"
	"os"
	"testing"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/stretchr/testify/require"
)

// patchHeader rewrites the 128-byte data-file header of a store created by
// newHeaderFixture through Unmarshal/mutate/MarshalTo (Marshal recomputes the
// header CRC), then returns the store path for Open.
func patchHeader(t *testing.T, mutate func(h *format.DataFileHeader)) string {
	t.Helper()
	path := newHeaderFixture(t)
	raw := make([]byte, format.DataFileHeaderSize)
	f, err := os.Open(path + ".rpk")
	require.NoError(t, err)
	_, err = f.ReadAt(raw, 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	var h format.DataFileHeader
	require.NoError(t, h.Unmarshal(raw))
	mutate(&h)
	require.NoError(t, h.MarshalTo(raw))

	f, err = os.OpenFile(path+".rpk", os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt(raw, 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}

// newHeaderFixture creates a small valid plain store with one snapshot.
func newHeaderFixture(t *testing.T) string {
	t.Helper()
	base := tmpdb(t)
	db, err := Create(base, Options{})
	require.NoError(t, err)
	ctx := context.Background()
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, w.Insert(ctx, "t", 1, Row{Uint64(1)}))
	_, err = w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return base
}

// TestOpenRejectsUnknownRequiredFeatures: a file demanding feature bits this
// build does not know is refused, never half-opened.
func TestOpenRejectsUnknownRequiredFeatures(t *testing.T) {
	path := patchHeader(t, func(h *format.DataFileHeader) { h.RequiredFeatures = 1 << 40 })
	_, err := Open(path, Options{})
	require.ErrorIs(t, err, ErrVersionUnsupported)
	require.ErrorContains(t, err, "unknown required feature bits")
}

// TestOpenRejectsBadDefaultBlockSize: creation-time geometry is a file
// property; an out-of-range stored default is corruption, not a tunable.
func TestOpenRejectsBadDefaultBlockSize(t *testing.T) {
	path := patchHeader(t, func(h *format.DataFileHeader) { h.DefaultBlockSize = 32 })
	_, err := Open(path, Options{})
	require.ErrorIs(t, err, ErrCorruptData)
	require.ErrorContains(t, err, "default block size")
}

// TestOpenRejectsBadDefaultCompression pins the unknown-compression arm.
func TestOpenRejectsBadDefaultCompression(t *testing.T) {
	path := patchHeader(t, func(h *format.DataFileHeader) { h.DefaultCompression = format.Compression(99) })
	_, err := Open(path, Options{})
	require.ErrorIs(t, err, ErrVersionUnsupported)
	require.ErrorContains(t, err, "default compression 99")
}

// TestOpenRejectsBadEncryptionFields: a header claiming an unknown encryption
// algorithm (or an unknown nonce scheme under a known algorithm) is refused.
func TestOpenRejectsBadEncryptionFields(t *testing.T) {
	path := patchHeader(t, func(h *format.DataFileHeader) { h.EncryptionAlgorithm = format.EncryptionAlgorithm(77) })
	_, err := Open(path, Options{})
	require.ErrorIs(t, err, ErrVersionUnsupported)
	require.ErrorContains(t, err, "encryption algorithm 77")

	path = patchHeader(t, func(h *format.DataFileHeader) {
		h.EncryptionAlgorithm = format.EncAES256GCM
		h.NonceScheme = format.NonceScheme(88)
	})
	_, err = Open(path, Options{})
	require.ErrorIs(t, err, ErrVersionUnsupported)
	require.ErrorContains(t, err, "nonce scheme 88")
}
