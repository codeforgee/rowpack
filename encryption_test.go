package rowpack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// staticKeyProvider hands out a fixed AES-256 key for (keyID, epoch), with an
// optional forced error. It mirrors the provider used to generate the
// encrypted golden sample, so the golden can be reopened and checked.
type staticKeyProvider struct {
	keyID string
	key   []byte
	fail  error
}

func (p *staticKeyProvider) Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error) {
	if p.fail != nil {
		return nil, p.fail
	}
	if keyID != p.keyID {
		return nil, ErrKeyIDNotFound
	}
	return p.key, nil
}

// testKey derives a deterministic 32-byte key per key id.
func testKey(id string) []byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(id[i%len(id)]) + byte(i)
	}
	return k[:]
}

func encOptions(keyID string) Options {
	return Options{
		BlockSize: 1024,
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
			KeyID:       keyID,
		},
	}
}

// TestEncryptedRoundTrip writes FULL + DELTA into an encrypted store and
// reads them back through a fresh open with the same key.
func TestEncryptedRoundTrip(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc")
	db, err := Create(base, encOptions("k1"))
	require.NoError(t, err)
	ctx := context.Background()
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	insertUsers(t, w, 5)
	full, err := w.Commit(ctx)
	require.NoError(t, err)
	d, _ := db.BeginDelta(ctx, full)
	require.NoError(t, d.Delete(ctx, "users", 3))
	delta, err := d.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// Reopen with the key: everything decrypts.
	db2, err := Open(base, encOptions("k1"))
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	row, err := db2.Get(ctx, delta, "users", 2, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-2", name)
	_, err = db2.Get(ctx, delta, "users", 3, nil)
	require.ErrorIs(t, err, ErrNotFound, "encrypted delta hides the deleted row")
	row, err = db2.Get(ctx, delta, "users", 5, nil)
	require.NoError(t, err)
	name, _ = row[1].String()
	require.Equal(t, "user-5", name)
	// Full verify must pass on an intact encrypted store.
	rep, err := db2.Verify(ctx, VerifyFull)
	require.NoError(t, err)
	require.Equal(t, uint64(2), rep.SnapshotsChecked)
	require.Greater(t, rep.RowsChecked, uint64(0))
}

// TestEncryptedKeyContract enforces the key contract: no provider at Open
// fails with ErrKeyRequired, a provider error surfaces as ErrKeyUnavailable,
// and a wrong key fails authentication on the read path.
func TestEncryptedKeyContract(t *testing.T) {
	base := filepath.Join(tmpdb(t), "keycontract")
	db, err := Create(base, encOptions("k1"))
	require.NoError(t, err)
	ctx := context.Background()
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	insertUsers(t, w, 3)
	full, _ := w.Commit(ctx)
	require.NoError(t, db.Close())

	// Opening encrypted storage without a provider is a hard error, not a
	// half-usable store.
	_, err = Open(base, Options{BlockSize: 1024})
	require.ErrorIs(t, err, ErrKeyRequired)

	// Key ID validation at Create.
	_, err = Create(filepath.Join(tmpdb(t), "badid"), Options{
		Encryption: &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "x", key: testKey("x")}, KeyID: ""},
	})
	require.ErrorIs(t, err, ErrInvalidArgument)

	// A provider that errors (e.g. key rotation miss) surfaces as
	// ErrKeyUnavailable on read.
	failing := Options{
		BlockSize: 1024,
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: "k1", key: testKey("k1"), fail: context.Canceled},
			KeyID:       "k1",
		},
	}
	_, err = Open(base, failing)
	require.ErrorIs(t, err, ErrKeyUnavailable)

	// Wrong key: block authentication fails on open recovery.
	wrong := Options{
		BlockSize: 1024,
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: "k1", key: make([]byte, 32)}, // all-zero key
			KeyID:       "k1",
		},
	}
	_, err = Open(base, wrong)
	require.Error(t, err, "wrong key must not silently open")
	require.True(t, errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrCorruptData),
		"wrong key failure must expose ErrAuthFailed chain, got: %v", err)

	// The correct key still opens afterwards.
	dbOK, err := Open(base, encOptions("k1"))
	require.NoError(t, err)
	t.Cleanup(func() { dbOK.Close() })
	row, err := dbOK.Get(ctx, full, "users", 1, nil)
	require.NoError(t, err)
	name, _ := row[1].String()
	require.Equal(t, "user-1", name)
}

// TestGoldenEncryptedStore reopens the locked encrypted golden sample (from
// testdata/golden/encrypted-store.rpk) with its generation key and verifies
// the full content reads back. The manifest already locks the bytes; this
// test locks the open+decrypt semantics.
func TestGoldenEncryptedStore(t *testing.T) {
	keyID := "gk"
	opts := func() Options {
		return Options{
			Compression: CompressionNone,
			Encryption:  &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)}, KeyID: keyID},
		}
	}
	base := filepath.Join(tmpdb(t), "golden-enc")
	data, err := os.ReadFile(goldenPath("encrypted-store.rpk"))
	require.NoError(t, err, "read encrypted golden (regenerate with make golden)")
	require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))

	db, err := Open(base, opts())
	require.NoError(t, err, "open encrypted golden")
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	snaps, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	require.True(t, len(snaps) >= 1)
	require.Equal(t, SnapshotType(SnapshotFull), snaps[0].Type)
	// The golden holds a FULL with three rows of (id, name) == ("row-N").
	for i := uint64(1); i <= 3; i++ {
		row, err := db.Get(ctx, snaps[0].ID, "t1", i, nil)
		require.NoError(t, err, "golden row %d", i)
		name, _ := row[1].String()
		require.Equal(t, fmt.Sprintf("row-%d", i), name)
	}
}

// TestGoldenEncryptedStoreGenerate regenerates testdata/golden/encrypted-store.rpk
// under -update-golden. The bytes are deterministic (fixed UUID/now/nonce +
// a static key), and TestGoldenEncryptedStore (above) reopens the locked
// sample with its key. Skipped unless -update-golden is set.
func TestGoldenEncryptedStoreGenerate(t *testing.T) {
	if !*updateGolden {
		t.Skip("regenerate the encrypted golden with -update-golden")
	}
	uuid := [16]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}
	testUUIDOverride = &uuid
	testNowOverride = 1757400000000000000
	nonce := uint64(0x454E435259505444) // "ENCRYPTD"
	testNonceOverride = &nonce
	t.Cleanup(func() {
		testUUIDOverride = nil
		testNowOverride = 0
		testNonceOverride = nil
	})
	base := filepath.Join(tmpdb(t), "golden-encgen")
	opts := Options{
		Compression: CompressionNone,
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: "gk", key: testKey("gk")},
			KeyID:       "gk",
		},
	}
	db, err := Create(base, opts)
	require.NoError(t, err)
	ctx := context.Background()
	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("t1", []Column{{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}}))
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, w.Insert(ctx, "t1", i, Row{Uint64(i), String(fmt.Sprintf("row-%d", i))}))
	}
	_, err = w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	data, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(goldenPath("encrypted-store.rpk"), data, 0o644))
}

// TestEncryptionTamperDetect flips one ciphertext byte and requires the read
// path to fail authentication: the AEAD is the integrity boundary, so a
// flipped byte must never decode to a modified row.
func TestEncryptionTamperDetect(t *testing.T) {
	base := filepath.Join(tmpdb(t), "tamper")
	db, err := Create(base, encOptions("k1"))
	require.NoError(t, err)
	ctx := context.Background()
	w, _ := db.BeginFull(ctx)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	insertUsers(t, w, 10)
	full, _ := w.Commit(ctx)

	// Address the rows block that physically holds row 1 (physical flush
	// order of metadata vs rows blocks varies), and tamper its ciphertext.
	st, err := db.captureState()
	require.NoError(t, err)
	loc := st.view.ResolveRow(full, 1, 1)
	require.NotNil(t, loc)
	rowsBlk := st.view.Block(loc.BlockID)
	require.NotNil(t, rowsBlk)
	blkOff := int64(rowsBlk.DataOffset) + 64 // 64-byte BlockHeader
	// Per-page encryption: the container header + page directory are plaintext
	// and the stored pages are sealed. Tamper a byte inside the first sealed
	// page so the AEAD authenticates (a flip in the plaintext header would be
	// caught by structure validation, not authentication). Read the container
	// header for PageCount, then jump past the directory to page 0.
	var chdr [fileformat.RowsBlockHeaderSize]byte
	require.NoError(t, db.Close())

	f, err := os.OpenFile(base+".rpk", os.O_RDWR, 0)
	require.NoError(t, err)
	_, err = f.ReadAt(chdr[:], blkOff)
	require.NoError(t, err)
	pageCount := le32(chdr[12:]) // RowsBlockHeader.PageCount
	require.Greater(t, pageCount, uint32(0))
	page0Stored := blkOff + int64(fileformat.RowsBlockHeaderSize) + int64(pageCount)*int64(fileformat.RowsPageDirEntrySize)
	payload := make([]byte, 16)
	_, err = f.ReadAt(payload, page0Stored)
	require.NoError(t, err)
	payload[0] ^= 0x01
	_, err = f.WriteAt(payload, page0Stored)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Reopen is possible (no block read during index replay) but any read of
	// the tampered block fails with ErrAuthFailed.
	db2, err := Open(base, encOptions("k1"))
	require.NoError(t, err)
	t.Cleanup(func() { db2.Close() })
	_, err = db2.Get(ctx, full, "users", 1, nil)
	require.ErrorIs(t, err, ErrAuthFailed, "tampered ciphertext must fail authentication")
	_, err = db2.Verify(ctx, VerifyFull)
	require.ErrorIs(t, err, ErrAuthFailed, "Verify must report the tampered block")
}
