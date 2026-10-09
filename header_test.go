package rowpack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// header.go's contract: PeekHeader reads the plaintext header without opening
// the store — no key, no writer lock — and a missing store is ErrNotFound while
// an unusable path is an open failure. Store.KeyID reports the stamped key id.

func TestPeekHeaderPlainStore(t *testing.T) {
	base := filepath.Join(tmpdb(t), "plain")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h, err := PeekHeader(base)
	require.NoError(t, err)
	require.False(t, h.Encrypted)
	require.Equal(t, "", h.KeyID)
	require.False(t, h.CreatedAt.IsZero())
	// Cross-check UUID/CreatedAt against the raw on-disk header.
	raw, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	var dh format.DataFileHeader
	require.NoError(t, dh.Unmarshal(raw))
	require.Equal(t, [16]byte(h.UUID), dh.StoreUUID)
	require.Equal(t, h.CreatedAt, time.Unix(0, dh.CreatedUnixNano).UTC())
}

func TestPeekHeaderEncryptedStore(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc")
	db, err := Create(base, encOptions("peek-key"))
	require.NoError(t, err)
	require.NoError(t, db.Close())

	h, err := PeekHeader(base)
	require.NoError(t, err)
	require.True(t, h.Encrypted)
	require.Equal(t, "peek-key", h.KeyID)
}

func TestPeekHeaderMissingStore(t *testing.T) {
	base := filepath.Join(tmpdb(t), "absent")
	_, err := PeekHeader(base)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestPeekHeaderGarbage(t *testing.T) {
	dir := tmpdb(t)
	p := filepath.Join(dir, "garbage.rpk")
	require.NoError(t, os.WriteFile(p, make([]byte, format.DataFileHeaderSize), 0o644))

	_, err := PeekHeader(filepath.Join(dir, "garbage"))
	require.Error(t, err) // bad magic; not a version error
	require.False(t, format.IsVersionError(err))
}

func TestPeekHeaderTruncated(t *testing.T) {
	dir := tmpdb(t)
	p := filepath.Join(dir, "short.rpk")
	require.NoError(t, os.WriteFile(p, []byte("RPK"), 0o644))

	_, err := PeekHeader(filepath.Join(dir, "short"))
	require.Error(t, err)
}

func TestPeekHeaderShortFile(t *testing.T) {
	base := filepath.Join(tmpdb(t), "short")
	require.NoError(t, os.WriteFile(base+".rpk", []byte("rowpack"), 0o644))
	_, err := PeekHeader(base)
	require.ErrorContains(t, err, "read store header")
}

func TestPeekHeaderFutureMajorVersion(t *testing.T) {
	dir := tmpdb(t)
	// Build a valid plain store, then bump the major version bytes in place.
	base := filepath.Join(dir, "future")
	db, err := Create(base, Options{BlockSize: 1024})
	require.NoError(t, err)
	require.NoError(t, db.Close())

	p := base + ".rpk"
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	raw[8] = 0x09 // major
	raw[9] = 0x00
	require.NoError(t, os.WriteFile(p, raw, 0o644))

	_, err = PeekHeader(base)
	require.ErrorIs(t, err, ErrVersionUnsupported)
}

func TestPeekHeaderInvalidPath(t *testing.T) {
	_, err := PeekHeader("")
	require.ErrorIs(t, err, ErrInvalidPath)
	_, err = PeekHeader(".rpk")
	require.ErrorIs(t, err, ErrInvalidPath)
}

// TestPeekHeaderRejectsFilePathComponent: a path component in front of the
// store name that is a regular file makes the path unusable, at any depth. The
// failure is the open error, never ErrNotFound — Windows reports
// ERROR_PATH_NOT_FOUND for this shape and Go folds that into fs.ErrNotExist,
// while Unix reports ENOTDIR, so the "missing store" verdict must come from the
// filesystem rather than from the status code.
func TestPeekHeaderRejectsFilePathComponent(t *testing.T) {
	dir := tmpdb(t)
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	for _, tc := range []struct{ name, base string }{
		{"direct", filepath.Join(blocker, "store")},
		{"nested", filepath.Join(blocker, "deep", "store")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PeekHeader(tc.base)
			require.ErrorContains(t, err, "open store file")
			require.NotErrorIs(t, err, ErrNotFound)
		})
	}
}

// TestStoreFileMissingFollowsFilesystem: the classifier behind that branch has
// to be portable — for a path component that is a regular file Windows fails
// with ERROR_PATH_NOT_FOUND, which Go maps to fs.ErrNotExist, while Unix fails
// with ENOTDIR, which it does not, so the status code alone cannot say whether
// the store file or one of its parents is the missing part. Feed it the
// ErrNotExist shape Windows produces and check it follows the filesystem.
func TestStoreFileMissingFollowsFilesystem(t *testing.T) {
	dir := tmpdb(t)
	blocker := filepath.Join(dir, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	// What os.Open returns for <blocker>/store.rpk on Windows: an error that
	// matches fs.ErrNotExist.
	notExist := func(p string) error { return fmt.Errorf("open %s: %w", p, os.ErrNotExist) }

	require.False(t, storeFileMissing(filepath.Join(blocker, "store.rpk"), notExist(blocker)),
		"a regular file in front of the store name is unusable, not missing")
	require.False(t, storeFileMissing(filepath.Join(blocker, "deep", "store.rpk"), notExist(blocker)),
		"…at any depth")
	require.True(t, storeFileMissing(filepath.Join(dir, "absent.rpk"), notExist(dir)),
		"an absent file in an existing directory is a missing store")
	require.True(t, storeFileMissing(filepath.Join(dir, "no", "such", "dir", "absent.rpk"), notExist(dir)),
		"a missing parent chain is still a missing store")
	require.False(t, storeFileMissing(filepath.Join(dir, "absent.rpk"), os.ErrPermission),
		"an error that is not ErrNotExist is never a missing store")
}

// ---- Store.KeyID ----

func TestStoreKeyIDAccessor(t *testing.T) {
	ctx := context.Background()
	dir := tmpdb(t)

	enc := Options{Encryption: &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "kid-1", key: testKey("kid-1")}, KeyID: "kid-1"}}
	db, err := Create(filepath.Join(dir, "enc"), enc)
	require.NoError(t, err)
	require.Equal(t, "kid-1", db.KeyID())
	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", []Column{{Name: "a", Type: TypeInt64}}))
	_, err = w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reopened, err := Open(filepath.Join(dir, "enc"), Options{Encryption: &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "kid-1", key: testKey("kid-1")}, KeyID: "kid-1"}})
	require.NoError(t, err)
	require.Equal(t, "kid-1", reopened.KeyID())
	require.NoError(t, reopened.Close())

	plain, err := Create(filepath.Join(dir, "plain"), Options{})
	require.NoError(t, err)
	require.Equal(t, "", plain.KeyID())
	require.NoError(t, plain.Close())
}
