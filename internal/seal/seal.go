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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

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

// IndexDomainBit is the nonce-domain flag (R11): index nonces set bit 31 of
// the epoch word while block nonces always carry an epoch below 2^31, so a
// block nonce and an index nonce can never collide regardless of counter
// values. Nonce uniqueness never relies on the AAD.
const IndexDomainBit = uint32(0x80000000)

// NonceIndex returns the deterministic 96-bit nonce for one encrypted index
// transaction: (epoch | IndexDomainBit)(4B, LE) ‖ txnSequence(8B, LE).
func NonceIndex(epoch uint32, txnSequence uint64) [fileformat.EncNonceLen]byte {
	return Nonce(epoch|IndexDomainBit, txnSequence)
}

// chunkNonceKeyLabel domain-separates the chunk-nonce HMAC subkey derivation
// from any other use of the data key.
var chunkNonceKeyLabel = []byte("RowPack index chunk nonce key v1")

// chunkNonceInfoPrefix domain-separates the per-chunk nonce input.
var chunkNonceInfoPrefix = []byte("RowPackIdxChunkV1")

// aadMagicIndex domain-separates the index AAD layout from the block AAD.
var aadMagicIndex = [16]byte{'R', 'o', 'w', 'P', 'a', 'c', 'k', 'I', 'n', 'd', 'e', 'x', 'V', '1', 0, 0}

// AADIndexSize is the fixed serialized index AAD length.
const AADIndexSize = 60

// BuildAADIndex serializes the domain-separated AAD for one encrypted index
// transaction:
//
//	 0..15  aadMagicIndex
//	16..31  store UUID
//	32..39  SnapshotID
//	40..47  IndexTxnStartOffset
//	48..55  IndexTxnEndOffset
//	56..59  KeyEpoch
//
// A moved, resized or cross-store index transaction cannot authenticate.
func BuildAADIndex(uuid *[16]byte, snapshotID, txnStart, txnEnd uint64, epoch uint32) [AADIndexSize]byte {
	var aad [AADIndexSize]byte
	copy(aad[0:16], aadMagicIndex[:])
	copy(aad[16:32], uuid[:])
	le64(aad[32:40], snapshotID)
	le64(aad[40:48], txnStart)
	le64(aad[48:56], txnEnd)
	le32(aad[56:60], epoch)
	return aad
}

// aadMagicIndexChunk domain-separates the chunk AAD layout.
var aadMagicIndexChunk = [16]byte{'R', 'o', 'w', 'P', 'a', 'c', 'k', 'I', 'C', 'h', 'k', 'V', '1', 0, 0, 0}

// AADIndexChunkSize is the fixed serialized chunk AAD length.
const AADIndexChunkSize = 72

// BuildAADIndexChunk serializes the domain-separated AAD for one encrypted
// index chunk. It binds every field that gives the chunk its identity and
// length semantics (doc §5.4). File offsets are intentionally excluded: the
// chunk's stored size depends on compression while the footer offsets depend
// on all stored sizes, so offset binding would be circular — the footer's
// region CRC and byte extent carry the offset binding instead.
//
//	 0..15  aadMagicIndexChunk
//	16..31  store UUID
//	32..39  TxnSequence
//	40..47  SnapshotID
//	48..51  ChunkSequence
//	52..55  FirstEntryOrdinal
//	56      EntryKind
//	57..59  reserved
//	60..63  RawBytes
//	64..67  StoredBytes
//	68..71  KeyEpoch
func BuildAADIndexChunk(uuid *[16]byte, txnSeq, snapshotID uint64, chunkSeq, firstOrdinal, rawBytes, storedBytes uint32, kind uint8, epoch uint32) [AADIndexChunkSize]byte {
	var aad [AADIndexChunkSize]byte
	copy(aad[0:16], aadMagicIndexChunk[:])
	copy(aad[16:32], uuid[:])
	le64(aad[32:40], txnSeq)
	le64(aad[40:48], snapshotID)
	le32(aad[48:52], chunkSeq)
	le32(aad[52:56], firstOrdinal)
	aad[56] = kind
	le32(aad[60:64], rawBytes)
	le32(aad[64:68], storedBytes)
	le32(aad[68:72], epoch)
	return aad
}

// SealIndexChunk seals one compressed index chunk payload under the chunk
// nonce/AAD. The returned ciphertext carries exactly AESGCMTagLen extra bytes.
func (c *Cipher) SealIndexChunk(uuid *[16]byte, txnSeq, snapshotID uint64, chunkSeq, firstOrdinal, rawBytes, storedBytes uint32, kind uint8, epoch uint32, plaintext []byte) ([]byte, error) {
	nonce := c.NonceIndexChunk(txnSeq, chunkSeq)
	aad := BuildAADIndexChunk(uuid, txnSeq, snapshotID, chunkSeq, firstOrdinal, rawBytes, storedBytes, kind, epoch)
	return c.SealWith(nonce, aad[:], plaintext), nil
}

// OpenIndexChunk authenticates and opens one stored (compressed) chunk
// payload. storedBytes must be the header-declared payload length including
// the tag.
func (c *Cipher) OpenIndexChunk(uuid *[16]byte, txnSeq, snapshotID uint64, chunkSeq, firstOrdinal, rawBytes, storedBytes uint32, kind uint8, epoch uint32, stored []byte) ([]byte, error) {
	nonce := c.NonceIndexChunk(txnSeq, chunkSeq)
	aad := BuildAADIndexChunk(uuid, txnSeq, snapshotID, chunkSeq, firstOrdinal, rawBytes, storedBytes, kind, epoch)
	return c.OpenWith(nonce, aad[:], stored)
}

// SealWith encrypts plaintext under an explicit nonce/AAD pair (index path;
// the block path uses Seal which derives both from the header).
func (c *Cipher) SealWith(nonce [fileformat.EncNonceLen]byte, aad []byte, plaintext []byte) []byte {
	out := make([]byte, 0, len(plaintext)+fileformat.AESGCMTagLen)
	return c.aead.Seal(out, nonce[:], plaintext, aad)
}

// OpenWith authenticates and opens ciphertext under an explicit nonce/AAD
// pair. On failure it returns ErrAuth wrapped with context.
func (c *Cipher) OpenWith(nonce [fileformat.EncNonceLen]byte, aad []byte, ciphertext []byte) ([]byte, error) {
	pt, err := c.aead.Open(nil, nonce[:], ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: nonce domain index", ErrAuth)
	}
	return pt, nil
}

// Cipher encrypts/decrypts blocks with one fixed AES-256 key. It wraps a
// cipher.AEAD so repeated block operations skip key schedule and GHASH setup.
// Cipher is safe for concurrent use (cipher.AEAD is).
type Cipher struct {
	aead cipher.AEAD
	key  [32]byte

	chunkHkOnce sync.Once
	chunkHk     [sha256.Size]byte // derived HMAC subkey for chunk nonces
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
	var key32 [32]byte
	copy(key32[:], key)
	return &Cipher{aead: gcm, key: key32}, nil
}

// chunkNonceKey derives (once) the HMAC subkey used for index chunk nonces.
// Storing the key material here is internal to seal; it never leaves the
// package except through authenticated ciphertext.
func (c *Cipher) chunkNonceKey() [sha256.Size]byte {
	c.chunkHkOnce.Do(func() {
		mac := hmac.New(sha256.New, c.key[:])
		mac.Write(chunkNonceKeyLabel)
		copy(c.chunkHk[:], mac.Sum(nil))
	})
	return c.chunkHk
}

// NonceIndexChunk returns the deterministic 96-bit nonce for one encrypted
// index txn chunk: Trunc12(HMAC-SHA256(chunkNonceKey, BE(txnSeq) ‖ BE(chunkSeq))).
// The 96-bit nonce space of NonceIndex is already occupied by
// (epoch, txnSequence), so chunk nonces are derived instead; injectivity of
// (txnSeq, chunkSeq) rests on HMAC collision resistance. Direct alternatives
// (truncating txnSeq, XOR-ing the low word) were rejected: both can produce
// nonce reuse.
func (c *Cipher) NonceIndexChunk(txnSeq uint64, chunkSeq uint32) [fileformat.EncNonceLen]byte {
	hk := c.chunkNonceKey()
	mac := hmac.New(sha256.New, hk[:])
	mac.Write(chunkNonceInfoPrefix)
	var in [12]byte
	binary.BigEndian.PutUint64(in[0:8], txnSeq)
	binary.BigEndian.PutUint32(in[8:12], chunkSeq)
	mac.Write(in[:])
	sum := mac.Sum(nil)
	var n [fileformat.EncNonceLen]byte
	copy(n[:], sum[:fileformat.EncNonceLen])
	return n
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