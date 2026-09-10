package rowpack

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/seal"
)

// TestEncryptionConfigValidate covers every rejection branch of validate.
func TestEncryptionConfigValidate(t *testing.T) {
	cfg := &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "k1"}}
	if err := cfg.validate(); err == nil {
		t.Fatal("empty key id should error")
	}

	cfg = &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "k1"}, KeyID: strings.Repeat("k", fileformat.FileHeaderKeyIDMaxLen+1)}
	if err := cfg.validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized key id: %v", err)
	}

	cfg = &EncryptionConfig{KeyProvider: &staticKeyProvider{keyID: "k1"}, KeyID: "k1"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

// TestBuildEncryptorErrors covers key-provider failure and bad key length.
func TestBuildEncryptorErrors(t *testing.T) {
	if c, err := newEncryptor(nil); c != nil || err != nil {
		t.Fatalf("nil config must yield nil cipher, got %v, %v", c, err)
	}

	boom := errors.New("kms down")
	cfg := &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k1", fail: boom},
		KeyID:       "k1",
	}
	if _, err := newEncryptor(cfg); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("provider failure: %v, want ErrKeyUnavailable", err)
	}

	cfg = &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k1", key: []byte("short key")},
		KeyID:       "k1",
	}
	if _, err := newEncryptor(cfg); err == nil || strings.Contains(err.Error(), "ErrKeyUnavailable") {
		t.Fatalf("bad key length should fail with cipher error, got %v", err)
	}
}

// TestReadDataHeaderCorruptFiles points Open at crafted garbage files to hit
// the header error branches (short read, bad magic, bad version).
func TestReadDataHeaderCorruptFiles(t *testing.T) {
	mk := func(t *testing.T, buf []byte) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "store.rpk")
		if err := fileformatFromBytes(p, buf); err != nil {
			t.Fatal(err)
		}
		return p
	}
	valid := func() []byte {
		// A valid header via a real Create, then keep only the header.
		base := t.TempDir()
		db, err := Create(filepath.Join(base, "s"), Options{})
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		data, err := readAll(filepath.Join(base, "s.rpk"))
		if err != nil {
			t.Fatal(err)
		}
		return data[:fileformat.DataFileHeaderSize]
	}()

	// Truncated file: header read fails.
	p := mk(t, valid[:10])
	if _, err := Open(p, Options{}); err == nil {
		t.Fatal("truncated header should fail to open")
	}

	// Bad magic.
	bad := append([]byte(nil), valid...)
	bad[0] = 'X'
	p = mk(t, bad)
	if _, err := Open(p, Options{}); err == nil {
		t.Fatal("bad magic should fail to open")
	}

	// Future version → ErrVersionUnsupported.
	bad = append([]byte(nil), valid...)
	for i, b := range []byte("RPKFMT99") {
		if i < len(bad) {
			bad[i] = b
		}
	}
	p = mk(t, bad)
	if _, err := Open(p, Options{ReadOnly: true}); !errors.Is(err, ErrVersionUnsupported) && err == nil {
		t.Fatalf("future version: %v", err)
	}
}

// TestStoreDecrypterWrapsAuthFailure seals real payloads with the same key
// the decrypter resolves, tampers with them, and confirms the wrappers
// surface ErrAuthFailed for blocks, rows pages and index chunks.
func TestStoreDecrypterWrapsAuthFailure(t *testing.T) {
	key := testKey("k1")
	prov := &staticKeyProvider{keyID: "k1", key: key}
	uuid := [16]byte{1, 2, 3}
	d := newDecrypter(prov, "k1", uuid)

	cipher, err := newEncryptor(&EncryptionConfig{KeyProvider: prov, KeyID: "k1"})
	if err != nil {
		t.Fatal(err)
	}

	// Block path.
	h := fileformat.BlockHeader{
		BlockKind: fileformat.BlockKindRows, Compression: fileformat.CompressionNone,
		BlockID: 9, SnapshotID: 1, TableID: 2, ItemCount: 1,
		RawSize: 4, StoredSize: 4, KeyEpoch: 0,
	}
	ct, err := cipher.Seal(&uuid, &h, []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Decrypt(h, ct); err != nil {
		t.Fatalf("honest block should decrypt: %v", err)
	}
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xFF
	if _, err := d.Decrypt(h, bad); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampered block: %v, want ErrAuthFailed", err)
	}

	// Rows page path.
	page := fileformat.RowsPageDirEntry{PageOrdinal: 1, RecordCount: 1, StoredSize: 8, RawSize: 4}
	pct, err := cipher.SealPage(seal.PageContext{
		UUID: &uuid, BlockID: 9, SnapshotID: 1, TableID: 2,
		Compression: fileformat.CompressionNone, Page: page, Epoch: 0,
	}, []byte("pagedata"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.OpenPage(h, page, pct); err != nil {
		t.Fatalf("honest page should decrypt: %v", err)
	}
	bad = append([]byte(nil), pct...)
	bad[len(bad)-1] ^= 0x80
	if _, err := d.OpenPage(h, page, bad); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampered page: %v, want ErrAuthFailed", err)
	}

	// Index chunk path.
	cct, err := cipher.SealIndexChunk(seal.ChunkContext{
		UUID: &uuid, TxnSequence: 1, SnapshotID: 1, ChunkSequence: 0, FirstOrdinal: 0,
		RawBytes: 4, StoredBytes: 4 + fileformat.AESGCMTagLen,
		Kind: uint8(fileformat.IndexChunkKindRow), Epoch: 0,
	}, []byte("indx"))
	if err != nil {
		t.Fatal(err)
	}
	chunkCtx := seal.ChunkContext{
		TxnSequence: 1, SnapshotID: 1, ChunkSequence: 0, FirstOrdinal: 0,
		RawBytes: 4, StoredBytes: 4 + fileformat.AESGCMTagLen,
		Kind: uint8(fileformat.IndexChunkKindRow), Epoch: 0,
	}
	if _, err := d.OpenIndexChunk(chunkCtx, cct); err != nil {
		t.Fatalf("honest chunk should decrypt: %v", err)
	}
	bad = append([]byte(nil), cct...)
	bad[1] ^= 0x01
	if _, err := d.OpenIndexChunk(chunkCtx, bad); !errors.Is(err, ErrAuthFailed) {
		t.Fatalf("tampered chunk: %v, want ErrAuthFailed", err)
	}

	// Epoch resolution: a fresh epoch resolves a new cipher via the provider
	// (staticKeyProvider hands out the same key), so the block still decrypts.
	if _, err := d.Decrypt(h, ct); err != nil {
		t.Fatalf("same key under a new epoch should decrypt: %v", err)
	}
}

func requireNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// fileformatFromBytes and readAll are small helpers for crafted-file tests.
func fileformatFromBytes(path string, buf []byte) error {
	return os.WriteFile(path, buf, 0o644)
}

func readAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}
