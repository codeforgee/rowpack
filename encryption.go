package rowpack

import (
	"context"
	"fmt"
	"sync"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/seal"
)

// KeyProvider supplies encryption keys by logical key ID and epoch. RowPack
// never stores or manages keys: the key ID is persisted in the store header,
// the raw key material is only ever held by the provider (and, transiently, in
// the process's AEAD state). A store created with encryption requires a
// provider at every Open and Verify; without one those entry points fail
// with ErrKeyRequired (in-memory IndexTxn rebuilds also require the key,
// because they re-read and authenticate every block).
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
	if len(c.KeyID) > format.FileHeaderKeyIDMaxLen {
		return fmt.Errorf("%w: encryption key id %d bytes exceeds %d", ErrInvalidArgument, len(c.KeyID), format.FileHeaderKeyIDMaxLen)
	}
	return nil
}

// newEncryptor resolves the initial key and builds the write-path Cipher.
// It is called once at Create.
func newEncryptor(cfg *EncryptionConfig) (*seal.Cipher, error) {
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

// decrypter authenticates and decrypts blocks for one store. Ciphers
// are cached per KeyEpoch so reopen of the same stored key (e.g. the common
// epoch 0 across every block) is at most one key-schedule per epoch; the
// cipher.AEAD instances are safe for concurrent reads.
type decrypter struct {
	provider KeyProvider
	keyID    string
	uuid     [16]byte

	mu     sync.Mutex
	epochs map[uint32]*seal.Cipher
}

func newDecrypter(provider KeyProvider, keyID string, uuid [16]byte) *decrypter {
	return &decrypter{provider: provider, keyID: keyID, uuid: uuid, epochs: make(map[uint32]*seal.Cipher)}
}

// Decrypt implements block.Decrypter. The returned plaintext is a fresh
// buffer owned by the caller.
func (d *decrypter) Decrypt(h format.BlockHeader, ciphertext []byte) ([]byte, error) {
	c, err := d.cipherFor(h.KeyEpoch)
	if err != nil {
		return nil, err
	}
	pt, err := c.Open(&d.uuid, &h, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: block %d (snapshot %d, table %d): %v", ErrAuthFailed, h.BlockID, h.SnapshotID, h.TableID, err)
	}
	return pt, nil
}

// OpenPage authenticates and decrypts one sealed Rows Page stored bytes
// (per-page encryption). The nonce binds the store/snapshot/block/page/epoch
// and the AAD binds the page-directory fields and block identity
// (BINARY_FORMAT_V1 §5.1). The returned plaintext is the page's compressed
// payload.
func (d *decrypter) OpenPage(h format.BlockHeader, page format.RowsPageDirEntry, ciphertext []byte) ([]byte, error) {
	c, err := d.cipherFor(h.KeyEpoch)
	if err != nil {
		return nil, err
	}
	pt, err := c.OpenPage(seal.PageContext{
		UUID:        &d.uuid,
		BlockID:     h.BlockID,
		SnapshotID:  h.SnapshotID,
		TableID:     h.TableID,
		Compression: h.Compression,
		Page:        page,
		Epoch:       h.KeyEpoch,
	}, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("%w: rows page %d (block %d, snapshot %d, table %d): %v", ErrAuthFailed, page.PageOrdinal, h.BlockID, h.SnapshotID, h.TableID, err)
	}
	return pt, nil
}

// OpenIndexChunk authenticates and decrypts one sealed index txn chunk. The
// nonce is HMAC-derived from (txn sequence, chunk sequence) and the AAD binds
// store, txn, chunk identity and lengths (doc §5.3/§5.4). ctx.UUID is ignored:
// the decrypter is the authority on the store identity and injects its own
// uuid, so callers never repeat it. The returned plaintext is the compressed
// chunk payload.
func (d *decrypter) OpenIndexChunk(ctx seal.ChunkContext, stored []byte) ([]byte, error) {
	c, err := d.cipherFor(ctx.Epoch)
	if err != nil {
		return nil, err
	}
	ctx.UUID = &d.uuid
	pt, err := c.OpenIndexChunk(ctx, stored)
	if err != nil {
		return nil, fmt.Errorf("%w: index chunk %d (txn %d, snapshot %d): %v",
			ErrAuthFailed, ctx.ChunkSequence, ctx.TxnSequence, ctx.SnapshotID, err)
	}
	return pt, nil
}

// cipherFor checks out (creating on first use) the cipher for one epoch. It
// is the shared entry point for the write path's single-writer encryptor;
// the returned cipher is cached per epoch so a reopen of the same stored key
// costs at most one key schedule.
func (d *decrypter) cipherFor(epoch uint32) (*seal.Cipher, error) {
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
