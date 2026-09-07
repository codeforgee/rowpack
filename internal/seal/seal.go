// Package seal implements per-block encryption for RowPack: AES-256-GCM with
// a deterministic 96-bit nonce and domain-separated AAD.
//
// Pipeline (fix order): Rows/Metadata payload -> compress -> Seal -> write.
// Read order is the reverse: read -> Open -> decompress -> validate.
//
// Nonce (NonceScheme=1): 96 bits = KeyEpoch(4B) ‖ BlockID(8B). Uniqueness
// holds because BlockID is monotonically increasing within a store, .rpk
// payloads are never rewritten, and the epoch is part of the nonce.
//
// AAD binds the StoreUUID and every BlockHeader field except the mutable
// per-block encryption affordances (flags key material and KeyEpoch, which is
// already part of the nonce), so a sealed payload cannot be moved to another
// block, table or store and still authenticate.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// ErrAuth is returned when AEAD authentication fails (tampered AAD,
// ciphertext or tag, or a wrong key/nonce).
var ErrAuth = errors.New("seal: authentication failed")

// aadMagic prefixes the AAD to domain-separate it from any future AAD layout.
var aadMagic = [16]byte{'R', 'o', 'w', 'P', 'a', 'c', 'k', 'B', 'l', 'o', 'c', 'k', 'V', '1', 0, 0}

// AADSize is the fixed serialized AAD length for NonceScheme=1.
const AADSize = 68

// Nonce returns the deterministic 96-bit nonce for (epoch, blockID):
// epoch(4B, LE) ‖ blockID(8B, LE).
func Nonce(epoch uint32, blockID uint64) [fileformat.EncNonceLen]byte {
	var n [fileformat.EncNonceLen]byte
	n[0] = byte(epoch)
	n[1] = byte(epoch >> 8)
	n[2] = byte(epoch >> 16)
	n[3] = byte(epoch >> 24)
	n[4] = byte(blockID)
	n[5] = byte(blockID >> 8)
	n[6] = byte(blockID >> 16)
	n[7] = byte(blockID >> 24)
	n[8] = byte(blockID >> 32)
	n[9] = byte(blockID >> 40)
	n[10] = byte(blockID >> 48)
	n[11] = byte(blockID >> 56)
	return n
}

// BuildAAD serializes the domain-separated AAD for a block:
//
//	0..15  aadMagic
//	16..31 store UUID
//	32     BlockKind
//	33     Compression
//	34..35 reserved (0)
//	36..43 BlockID
//	44..51 SnapshotID
//	52..55 TableID
//	56..59 ItemCount
//	60..63 RawSize
//	64..67 StoredSize
//
// KeyEpoch and the Encrypted flag are intentionally excluded: KeyEpoch is
// already part of the nonce, and the Encrypted flag is what selects this code
// path (a plain block has no nonce/AAD at all).
func BuildAAD(uuid *[16]byte, h *fileformat.BlockHeader) [AADSize]byte {
	var aad [AADSize]byte
	copy(aad[0:16], aadMagic[:])
	copy(aad[16:32], uuid[:])
	aad[32] = byte(h.BlockKind)
	aad[33] = byte(h.Compression)
	// 34..35 zero
	le64(aad[36:44], h.BlockID)
	le64(aad[44:52], h.SnapshotID)
	le32(aad[52:56], h.TableID)
	le32(aad[56:60], h.ItemCount)
	le32(aad[60:64], h.RawSize)
	le32(aad[64:68], h.StoredSize)
	return aad
}

// Cipher encrypts/decrypts blocks with one fixed AES-256 key. It wraps a
// cipher.AEAD so repeated block operations skip key schedule and GHASH setup.
// Cipher is safe for concurrent use (cipher.AEAD is).
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher creates a Cipher from a 32-byte (AES-256) key.
func NewCipher(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("seal: key length %d, want 32", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("seal: aes: %w", err)
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, fmt.Errorf("seal: gcm: %w", err)
	}
	if gcm.NonceSize() != fileformat.EncNonceLen {
		return nil, fmt.Errorf("seal: gcm nonce size %d, want %d", gcm.NonceSize(), fileformat.EncNonceLen)
	}
	if gcm.Overhead() != fileformat.AESGCMTagLen {
		return nil, fmt.Errorf("seal: gcm overhead %d, want %d", gcm.Overhead(), fileformat.AESGCMTagLen)
	}
	return &Cipher{aead: gcm}, nil
}

// Seal encrypts plaintext into a fresh ciphertext buffer (plaintext +
// AESGCMTagLen). plaintext is not modified.
func (c *Cipher) Seal(epoch uint32, blockID uint64, uuid *[16]byte, h *fileformat.BlockHeader, plaintext []byte) ([]byte, error) {
	nonce := Nonce(epoch, blockID)
	aad := BuildAAD(uuid, h)
	out := make([]byte, 0, len(plaintext)+fileformat.AESGCMTagLen)
	return c.aead.Seal(out, nonce[:], plaintext, aad[:]), nil
}

// Open authenticates ciphertext against (epoch, blockID, uuid, header) and
// returns the plaintext in a fresh buffer. On any authentication failure it
// returns ErrAuth (wrapped with context) and a nil payload.
func (c *Cipher) Open(epoch uint32, blockID uint64, uuid *[16]byte, h *fileformat.BlockHeader, ciphertext []byte) ([]byte, error) {
	nonce := Nonce(epoch, blockID)
	aad := BuildAAD(uuid, h)
	pt, err := c.aead.Open(nil, nonce[:], ciphertext, aad[:])
	if err != nil {
		return nil, fmt.Errorf("%w: block %d epoch %d", ErrAuth, blockID, epoch)
	}
	return pt, nil
}

func le64(dst []byte, v uint64) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
	dst[4] = byte(v >> 32)
	dst[5] = byte(v >> 40)
	dst[6] = byte(v >> 48)
	dst[7] = byte(v >> 56)
}

func le32(dst []byte, v uint32) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
}