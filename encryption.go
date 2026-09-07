package rowpack

import (
	"context"
	"fmt"
	"sync"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/seal"
)

// KeyProvider supplies encryption keys by logical key ID and epoch. RowPack
// never stores or manages keys: the key ID is persisted in the store header,
// the raw key material is only ever held by the provider (and, transiently, in
// the process's AEAD state). A store created with encryption requires a
// provider at every Open, Verify and RebuildIndex; without one those entry
// points fail with ErrKeyRequired.
type KeyProvider interface {
	// Key returns the raw key bytes for (keyID, epoch). Epoch 0 is the
	// initial epoch; an encrypted store with rotated keys requests the epoch
	// recorded in each block's KeyEpoch. The returned slice must be 32 bytes
	// (AES-256). Errors are wrapped as ErrKeyUnavailable (or ErrKeyIDNotFound
	// when the provider knows the key id does not exist).
	Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error)
}

// EncryptionConfig enables per-block AES-256-GCM encryption at Create time.
// Encryption is fixed when the store is created (the file headers are
// immutable); there is no way to encrypt an existing plain store or to switch
// back. KeyID must be 1..FileHeaderKeyIDMaxLen bytes.
type EncryptionConfig struct {
	KeyProvider KeyProvider
	KeyID       string
}

// validate checks an EncryptionConfig against rows of Options.validate.
func (c *EncryptionConfig) validate() error {
	if c.KeyProvider == nil {
		return fmt.Errorf("%w: encryption key provider is nil", ErrInvalidArgument)
	}
	if c.KeyID == "" {
		return fmt.Errorf("%w: encryption key id is empty", ErrInvalidArgument)
	}
	if len(c.KeyID) > fileformat.FileHeaderKeyIDMaxLen {
		return fmt.Errorf("%w: encryption key id %d bytes exceeds %d", ErrInvalidArgument, len(c.KeyID), fileformat.FileHeaderKeyIDMaxLen)
	}
	return nil
}

// buildEncryptor resolves the initial key and builds the write-path Cipher.
// It is called once at Create.
func buildEncryptor(cfg *EncryptionConfig) (*seal.Cipher, error) {
	if cfg == nil {
		return nil, nil
	}
	key, err := cfg.KeyProvider.Key(context.Background(), cfg.KeyID, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: key id %q epoch 0: %v", ErrKeyUnavailable, cfg.KeyID, err)
	}
	c, err := seal.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("rowpack: %w", err)
	}
	return c, nil
}

// storeDecrypter authenticates and decrypts blocks for one store. Ciphers
// are cached per KeyEpoch so reopen of the same stored key (e.g. the common
// epoch 0 across every block) is at most one key-schedule per epoch; the
// cipher.AEAD instances are safe for concurrent reads.
type storeDecrypter struct {
	provider KeyProvider
	keyID    string
	uuid     [16]byte

	mu     sync.Mutex
	epochs map[uint32]*seal.Cipher
}

func newStoreDecrypter(provider KeyProvider, keyID string, uuid [16]byte) *storeDecrypter {
	return &storeDecrypter{provider: provider, keyID: keyID, uuid: uuid, epochs: make(map[uint32]*seal.Cipher)}
}

// Decrypt implements block.Decrypter. The returned plaintext is a fresh
// buffer owned by the caller.
func (d *storeDecrypter) Decrypt(h fileformat.BlockHeader, ciphertext []byte) ([]byte, error) {
	c, err := d.cipherFor(h.KeyEpoch)
	if err != nil {
		return nil, err
	}
	pt, err := c.Open(h.KeyEpoch, h.BlockID, &d.uuid, &h, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: block %d (snapshot %d, table %d): %v", ErrAuthFailed, h.BlockID, h.SnapshotID, h.TableID, err)
	}
	return pt, nil
}

// cipherFor checks out (creating on first use) the cipher for one epoch.
func (d *storeDecrypter) cipherFor(epoch uint32) (*seal.Cipher, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.epochs[epoch]; c != nil {
		return c, nil
	}
	key, err := d.provider.Key(context.Background(), d.keyID, epoch)
	if err != nil {
		return nil, fmt.Errorf("%w: key id %q epoch %d: %v", ErrKeyUnavailable, d.keyID, epoch, err)
	}
	c, err := seal.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("rowpack: %w", err)
	}
	d.epochs[epoch] = c
	return c, nil
}

// Cipher returns the (cached) cipher for an epoch, resolving the key through
// the provider on first use. It is the shared entry point for the write
// path's single-writer encryptor.
func (d *storeDecrypter) Cipher(epoch uint32) (*seal.Cipher, error) {
	return d.cipherFor(epoch)
}