package rowpack

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// writeFullSnapshot writes n rows of a two-column table into a FULL snapshot
// and returns its info.
func writeFullSnapshot(t *testing.T, db *Store, n uint64) SnapshotInfo {
	t.Helper()
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, err)
	schema := schema1()
	require.NoError(t, w.DefineSchema(schema))
	for i := uint64(1); i <= n; i++ {
		require.NoError(t, w.Insert(context.Background(), 1, i, 1, row1(i)))
	}
	info, err := w.Commit(context.Background())
	require.NoError(t, err)
	return info
}

// schema1 returns a minimal two-column schema (id, name).
func schema1() Schema {
	return Schema{
		TableID: 1,
		Version: 1,
		Name:    "t1",
		Columns: []Column{
			{Name: "id", Type: TypeUint64},
			{Name: "name", Type: TypeString},
		},
	}
}

func row1(i uint64) Row {
	return Row{Uint64(i), String("row-" + itoa(i))}
}

func isErr(err, target error) bool {
	return err != nil && errors.Is(err, target)
}

// staticKeyProvider hands out a fixed AES-256 key for (keyID, epoch), with an
// optional forced error.
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

func testKey(id string) []byte {
	// 32-byte deterministic key, distinct per id.
	var k [32]byte
	for i := range k {
		k[i] = byte(id[i%len(id)]) + byte(i)
	}
	return k[:]
}

func encOptions(keyID string) Options {
	return Options{
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
			KeyID:       keyID,
		},
		Compression: CompressionNone, // crisp StoredSize assertions
	}
}

// TestCreateEncryptedHeader verifies the store headers carry the encryption
// fields and the key id.
func TestCreateEncryptedHeader(t *testing.T) {
	dir := tmpdb(t)
	db, err := Create(dir+"/db", encOptions("key-a"))
	require.NoError(t, err)
	require.NotNil(t, db.encCipher, "expected write-path cipher")
	db.Close()

	// Read the .rpk header back from disk.
	raw, err := os.ReadFile(dir + "/db.rpk")
	require.NoError(t, err)
	var h fileformat.DataFileHeader
	require.NoError(t, h.Unmarshal(raw[:fileformat.DataFileHeaderSize]))
	require.Equal(t, fileformat.EncAES256GCM, h.EncryptionAlgorithm, "algorithm = %d, want %d", h.EncryptionAlgorithm, fileformat.EncAES256GCM)
	require.Equal(t, fileformat.NonceCounterV1, h.NonceScheme, "nonce scheme = %d, want %d", h.NonceScheme, fileformat.NonceCounterV1)
	require.Equal(t, "key-a", string(h.KeyID), "key id = %q, want %q", h.KeyID, "key-a")
}

// TestEncryptedWriteSealsBlocks verifies committed blocks are sealed: the
// block header is flagged encrypted, KeyEpoch is set, and StoredSize grows by
// exactly the tag length (CompressionNone makes the plaintext length equal
// RawSize).
func TestEncryptedWriteSealsBlocks(t *testing.T) {
	dir := tmpdb(t)
	db, err := Create(dir+"/db", encOptions("key-b"))
	require.NoError(t, err)
	writeFullSnapshot(t, db, 100)
	db.Close()

	// Walk the .rpk payload: SnapshotHeader then per-block header+payload.
	raw, err := os.ReadFile(dir + "/db.rpk")
	require.NoError(t, err)
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	sawEncrypted := 0
	for off < int64(len(raw)) && string(raw[off:off+8]) != fileformat.MagicIndexTxnHdr {
		var bh fileformat.BlockHeader
		require.NoError(t, bh.Unmarshal(raw[off:off+fileformat.BlockHeaderSize]), "block header at %d", off)
		require.True(t, bh.Encrypted, "block %d at %d not encrypted", bh.BlockID, off)
		require.Equal(t, uint32(0), bh.KeyEpoch, "block %d epoch = %d, want 0", bh.BlockID, bh.KeyEpoch)
		require.Equal(t, uint32(bh.RawSize+fileformat.AESGCMTagLen), bh.StoredSize, "block %d stored %d != raw %d + tag %d", bh.BlockID, bh.StoredSize, bh.RawSize, fileformat.AESGCMTagLen)
		sawEncrypted++
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
	require.NotZero(t, sawEncrypted, "no block scanned")
}

// TestPlainStoreBlocksUnencrypted guards the plain path: no encryption bytes.
func TestPlainStoreBlocksUnencrypted(t *testing.T) {
	dir := tmpdb(t)
	db, err := Create(dir+"/db", Options{Compression: CompressionNone})
	require.NoError(t, err)
	writeFullSnapshot(t, db, 10)
	db.Close()

	raw, err := os.ReadFile(dir + "/db.rpk")
	require.NoError(t, err)
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	for off < int64(len(raw)) && string(raw[off:off+8]) != fileformat.MagicIndexTxnHdr {
		var bh fileformat.BlockHeader
		require.NoError(t, bh.Unmarshal(raw[off:off+fileformat.BlockHeaderSize]), "block header at %d", off)
		require.False(t, bh.Encrypted || bh.KeyEpoch != 0, "plain block %d carries encryption bytes", bh.BlockID)
		require.Equal(t, bh.RawSize, bh.StoredSize, "plain block %d stored %d != raw %d", bh.BlockID, bh.StoredSize, bh.RawSize)
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
}

// TestEncryptionInvalidConfig covers configuration rejection.
func TestEncryptionInvalidConfig(t *testing.T) {
	dir := tmpdb(t)
	_, err := Create(dir+"/a", Options{Encryption: &EncryptionConfig{KeyID: "k"}})
	require.ErrorIs(t, err, ErrInvalidArgument, "nil provider = %v, want ErrInvalidArgument", err)
	_, err = Create(dir+"/b", Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k", key: testKey("k")},
	}})
	require.ErrorIs(t, err, ErrInvalidArgument, "empty key id = %v, want ErrInvalidArgument", err)
	longID := make([]byte, fileformat.FileHeaderKeyIDMaxLen+1)
	for i := range longID {
		longID[i] = 'x'
	}
	_, err = Create(dir+"/c", Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: string(longID), key: testKey("k")},
		KeyID:       string(longID),
	}})
	require.ErrorIs(t, err, ErrInvalidArgument, "over-long key id = %v, want ErrInvalidArgument", err)
}

// TestEncryptionProviderError propagates provider failure at Create.
func TestEncryptionProviderError(t *testing.T) {
	dir := tmpdb(t)
	_, err := Create(dir+"/db", Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k", key: testKey("k"), fail: context.Canceled},
		KeyID:       "k",
	}})
	require.Error(t, err, "provider error swallowed at Create")
	require.ErrorIs(t, err, ErrKeyUnavailable, "err = %v, want ErrKeyUnavailable wrapper", err)
}

// TestBlockHeaderOffsetsOnDisk guards the on-disk offsets read by the walk in
// TestEncryptedWriteSealsBlocks: magic at 0, StoredSize at 44, KeyEpoch at 56.
func TestBlockHeaderOffsetsOnDisk(t *testing.T) {
	var h fileformat.BlockHeader
	h.Encrypted = true
	h.KeyEpoch = 3
	h.StoredSize = 0xAABBCCDD
	var buf [fileformat.BlockHeaderSize]byte
	require.NoError(t, h.MarshalTo(buf[:]))
	require.Equal(t, "RPKBLOCK", string(buf[0:8]), "magic")
	require.Equal(t, h.StoredSize, binary.LittleEndian.Uint32(buf[44:]), "stored size offset")
	require.Equal(t, uint32(3), binary.LittleEndian.Uint32(buf[fileformat.BlockHeaderKeyEpochOffset:]), "key epoch offset")
}

// buildEncryptedGoldenStore writes a deterministic encrypted FULL store with
// one rows block (None compression keeps the ciphertext layout independent of
// the zstd library version).
func buildEncryptedGoldenStore(t *testing.T, base string) {
	t.Helper()
	uuid := [16]byte{0xE0, 0xC1, 0xE2, 0xC3, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	testUUIDOverride = &uuid
	testNowOverride = 1757400000000000001
	nonce := uint64(0xE0E0E0E0E0E0E0E1)
	testNonceOverride = &nonce
	t.Cleanup(func() {
		testUUIDOverride = nil
		testNowOverride = 0
		testNonceOverride = nil
	})
	keyID := "gk"
	db, err := Create(base, Options{
		Compression: CompressionNone,
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
			KeyID:       keyID,
		},
	})
	require.NoError(t, err)
	w, _ := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	require.NoError(t, w.DefineSchema(schema1()))
	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, w.Insert(context.Background(), 1, i, 1, row1(i)))
	}
	_, err = w.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// TestGoldenEncryptedStore locks the byte layout of an encrypted store
// (encryption header fields, block flags, KeyEpoch, ciphertext) and verifies
// the golden opens and reads back with its key.
func TestGoldenEncryptedStore(t *testing.T) {
	keyID := "gk"
	enc := func() Options {
		return Options{
			Compression: CompressionNone,
			Encryption: &EncryptionConfig{
				KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
				KeyID:       keyID,
			},
		}
	}
	base := filepath.Join(tmpdb(t), "golden-enc")
	buildEncryptedGoldenStore(t, base)
	generated, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	if *updateGolden {
		require.NoError(t, os.WriteFile(goldenPath("encrypted-store.rpk"), generated, 0o644))
		return
	}
	// Copy the golden sample and open it with the key.
	data, err := os.ReadFile(goldenPath("encrypted-store.rpk"))
	require.NoError(t, err, "read golden (regenerate with make golden): %v", err)
	require.Equal(t, data, generated, "writer output differs from locked encrypted golden (regenerate with make golden only for an intentional format change)")
	base = filepath.Join(tmpdb(t), "golden-enc-open")
	require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))
	db, err := Open(base, enc())
	require.NoError(t, err, "open golden: %v", err)
	defer db.Close()
	for i := uint64(1); i <= 3; i++ {
		r, err := db.Get(context.Background(), 1, 1, i, nil)
		require.NoError(t, err, "row %d: %v", i, err)
		n, _ := r[1].String()
		require.Equal(t, "row-"+itoa(i), n, "row %d name = %q", i, n)
	}
}
