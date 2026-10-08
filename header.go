package rowpack

import (
	"fmt"
	"io"
	"os"
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
// plaintext. A missing store reports ErrNotFound; a damaged or foreign header
// reports ErrVersionUnsupported or a corruption error.
func PeekHeader(basePath string) (Header, error) {
	_, dataPath, err := storePaths(basePath)
	if err != nil {
		return Header{}, err
	}
	f, err := os.Open(dataPath)
	if err != nil {
		if os.IsNotExist(err) {
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

// KeyID returns the key id recorded in the store header: the caller label for
// an encrypted store created with WithKeyID-style labeling, or the derived
// digest. It is "" for a plain store.
func (s *Store) KeyID() string { return string(s.header.KeyID) }
