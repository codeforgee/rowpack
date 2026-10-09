package rowpack

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/codeforgee/rowpack/internal/format"
)

// Header is the plaintext store file header, readable without opening the
// store and without any decryption key. It carries the facts callers need to
// decide how to open a store: identity, creation time and, for encrypted
// stores, the key id the KeyProvider must serve.
type Header struct {
	UUID      [16]byte
	CreatedAt time.Time
	Encrypted bool
	KeyID     string
}

// PeekHeader reads and validates the store file header at basePath without
// opening the store. It works for encrypted stores too: the header is always
// plaintext. A missing store reports ErrNotFound; an unusable path, a damaged
// header or a foreign header reports the open/parse failure, ErrVersionUnsupported
// or a corruption error.
func PeekHeader(basePath string) (Header, error) {
	_, dataPath, err := storePaths(basePath)
	if err != nil {
		return Header{}, err
	}
	f, err := os.Open(dataPath)
	if err != nil {
		if storeFileMissing(dataPath, err) {
			return Header{}, fmt.Errorf("%w: missing store file %s", ErrNotFound, dataPath)
		}
		return Header{}, fmt.Errorf("rowpack: open store file: %w", err)
	}
	defer f.Close()
	buf := make([]byte, format.DataFileHeaderSize)
	if _, err := io.ReadFull(f, buf); err != nil {
		return Header{}, fmt.Errorf("rowpack: read store header: %w", err)
	}
	var dh format.DataFileHeader
	if err := dh.Unmarshal(buf); err != nil {
		if format.IsVersionError(err) {
			return Header{}, fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
		}
		return Header{}, fmt.Errorf("rowpack: parse store header: %w", err)
	}
	return Header{
		UUID:      dh.StoreUUID,
		CreatedAt: time.Unix(0, dh.CreatedUnixNano).UTC(),
		Encrypted: dh.EncryptionAlgorithm != format.EncNone,
		KeyID:     string(dh.KeyID),
	}, nil
}

// storeFileMissing reports whether an os.Open failure on dataPath means the
// store file itself is absent, rather than a path component in front of it
// being unusable. A plain os.IsNotExist check is not portable: for a component
// that is a regular file Windows fails with ERROR_PATH_NOT_FOUND, which Go maps
// to fs.ErrNotExist, while Unix fails with ENOTDIR, which it does not. The same
// Windows status also covers a genuinely missing directory, so the error alone
// cannot tell the two apart. The path is therefore only "missing" when its
// deepest existing ancestor is a directory.
func storeFileMissing(dataPath string, err error) bool {
	if !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for dir := filepath.Dir(dataPath); ; {
		info, statErr := os.Stat(dir)
		if statErr == nil {
			return info.IsDir()
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			// Present but unreadable: the open failure stands.
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Walked up to the volume root without finding anything.
			return true
		}
		dir = parent
	}
}

// KeyID returns the key id recorded in the store header: the caller label for
// an encrypted store created with WithKeyID-style labeling, or the derived
// digest. It is "" for a plain store.
func (s *Store) KeyID() string { return string(s.header.KeyID) }
