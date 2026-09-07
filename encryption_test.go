package rowpack

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// writeFullSnapshot writes n rows of a two-column table into a FULL snapshot
// and returns its info.
func writeFullSnapshot(t *testing.T, db *Store, n uint64) SnapshotInfo {
	t.Helper()
	w, err := db.BeginSnapshot(context.Background(), SnapshotFull, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	schema := schema1()
	if err := w.DefineSchema(schema); err != nil {
		t.Fatal(err)
	}
	for i := uint64(1); i <= n; i++ {
		if err := w.Insert(context.Background(), 1, i, 1, row1(i)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := w.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
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
	dir := t.TempDir()
	db, err := Create(dir+"/db", encOptions("key-a"))
	if err != nil {
		t.Fatal(err)
	}
	if db.encCipher == nil {
		t.Fatal("expected write-path cipher")
	}
	db.Close()

	// Read the .rpk header back from disk.
	raw, err := os.ReadFile(dir + "/db.rpk")
	if err != nil {
		t.Fatal(err)
	}
	var h fileformat.DataFileHeader
	if err := h.Unmarshal(raw[:fileformat.DataFileHeaderSize]); err != nil {
		t.Fatal(err)
	}
	if h.EncryptionAlgorithm != fileformat.EncAES256GCM {
		t.Fatalf("algorithm = %d, want %d", h.EncryptionAlgorithm, fileformat.EncAES256GCM)
	}
	if h.NonceScheme != fileformat.NonceCounterV1 {
		t.Fatalf("nonce scheme = %d, want %d", h.NonceScheme, fileformat.NonceCounterV1)
	}
	if string(h.KeyID) != "key-a" {
		t.Fatalf("key id = %q, want %q", h.KeyID, "key-a")
	}
}

// TestEncryptedWriteSealsBlocks verifies committed blocks are sealed: the
// block header is flagged encrypted, KeyEpoch is set, and StoredSize grows by
// exactly the tag length (CompressionNone makes the plaintext length equal
// RawSize).
func TestEncryptedWriteSealsBlocks(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir+"/db", encOptions("key-b"))
	if err != nil {
		t.Fatal(err)
	}
	writeFullSnapshot(t, db, 100)
	db.Close()

	// Walk the .rpk payload: SnapshotHeader then per-block header+payload.
	raw, err := os.ReadFile(dir + "/db.rpk")
	if err != nil {
		t.Fatal(err)
	}
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	sawEncrypted := 0
	for off < int64(len(raw)) && string(raw[off:off+8]) != "RPKSNAPF" {
		var bh fileformat.BlockHeader
		if err := bh.Unmarshal(raw[off : off+fileformat.BlockHeaderSize]); err != nil {
			t.Fatalf("block header at %d: %v", off, err)
		}
		if !bh.Encrypted {
			t.Fatalf("block %d at %d not encrypted", bh.BlockID, off)
		}
		if bh.KeyEpoch != 0 {
			t.Fatalf("block %d epoch = %d, want 0", bh.BlockID, bh.KeyEpoch)
		}
		if bh.StoredSize != bh.RawSize+fileformat.AESGCMTagLen {
			t.Fatalf("block %d stored %d != raw %d + tag %d", bh.BlockID, bh.StoredSize, bh.RawSize, fileformat.AESGCMTagLen)
		}
		sawEncrypted++
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
	if sawEncrypted == 0 {
		t.Fatal("no block scanned")
	}
}

// TestPlainStoreBlocksUnencrypted guards the plain path: no encryption bytes.
func TestPlainStoreBlocksUnencrypted(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(dir+"/db", Options{Compression: CompressionNone})
	if err != nil {
		t.Fatal(err)
	}
	writeFullSnapshot(t, db, 10)
	db.Close()

	raw, err := os.ReadFile(dir + "/db.rpk")
	if err != nil {
		t.Fatal(err)
	}
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	for off < int64(len(raw)) && string(raw[off:off+8]) != "RPKSNAPF" {
		var bh fileformat.BlockHeader
		if err := bh.Unmarshal(raw[off : off+fileformat.BlockHeaderSize]); err != nil {
			t.Fatalf("block header at %d: %v", off, err)
		}
		if bh.Encrypted || bh.KeyEpoch != 0 {
			t.Fatalf("plain block %d carries encryption bytes", bh.BlockID)
		}
		if bh.StoredSize != bh.RawSize {
			t.Fatalf("plain block %d stored %d != raw %d", bh.BlockID, bh.StoredSize, bh.RawSize)
		}
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
}

// TestEncryptionInvalidConfig covers configuration rejection.
func TestEncryptionInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(dir+"/a", Options{Encryption: &EncryptionConfig{KeyID: "k"}}); !isErr(err, ErrInvalidArgument) {
		t.Fatalf("nil provider = %v, want ErrInvalidArgument", err)
	}
	if _, err := Create(dir+"/b", Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k", key: testKey("k")},
	}}); !isErr(err, ErrInvalidArgument) {
		t.Fatalf("empty key id = %v, want ErrInvalidArgument", err)
	}
	longID := make([]byte, fileformat.FileHeaderKeyIDMaxLen+1)
	for i := range longID {
		longID[i] = 'x'
	}
	if _, err := Create(dir+"/c", Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: string(longID), key: testKey("k")},
		KeyID:       string(longID),
	}}); !isErr(err, ErrInvalidArgument) {
		t.Fatalf("over-long key id = %v, want ErrInvalidArgument", err)
	}
}

// TestEncryptionProviderError propagates provider failure at Create.
func TestEncryptionProviderError(t *testing.T) {
	dir := t.TempDir()
	_, err := Create(dir+"/db", Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k", key: testKey("k"), fail: context.Canceled},
		KeyID:       "k",
	}})
	if err == nil {
		t.Fatal("provider error swallowed at Create")
	}
	if !isErr(err, ErrKeyUnavailable) {
		t.Fatalf("err = %v, want ErrKeyUnavailable wrapper", err)
	}
}

// TestBlockHeaderOffsetsOnDisk guards the on-disk offsets read by the walk in
// TestEncryptedWriteSealsBlocks: magic at 0, StoredSize at 44, KeyEpoch at 56.
func TestBlockHeaderOffsetsOnDisk(t *testing.T) {
	var h fileformat.BlockHeader
	h.Encrypted = true
	h.KeyEpoch = 3
	h.StoredSize = 0xAABBCCDD
	var buf [fileformat.BlockHeaderSize]byte
	if err := h.MarshalTo(buf[:]); err != nil {
		t.Fatal(err)
	}
	if string(buf[0:8]) != "RPKBLOCK" {
		t.Fatal("magic")
	}
	if binary.LittleEndian.Uint32(buf[44:]) != h.StoredSize {
		t.Fatal("stored size offset")
	}
	if binary.LittleEndian.Uint32(buf[fileformat.BlockHeaderKeyEpochOffset:]) != 3 {
		t.Fatal("key epoch offset")
	}
}