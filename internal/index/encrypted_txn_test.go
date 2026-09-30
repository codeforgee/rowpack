package index

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// The stub cipher appends a 16-byte HMAC tag to every sealed payload and
// verifies + strips it on open. It is keyed deterministically from the chunk
// identity, so a payload moved between chunks fails authentication — same
// trust structure as the real AES-GCM seal without pulling in keys.
func newStubCrypto(epoch uint32) *ChunkCrypto {
	tag := func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) []byte {
		mac := sha256.Sum256([]byte{byte(chunkSeq >> 24), byte(chunkSeq >> 16), byte(chunkSeq >> 8), byte(chunkSeq), kind, byte(firstOrdinal), byte(rawBytes)})
		m := hmac.New(sha256.New, mac[:])
		m.Write(stored)
		return m.Sum(nil)[:format.AESGCMTagLen]
	}
	return &ChunkCrypto{
		TxnSequence: 1,
		SnapshotID:  1,
		Epoch:       epoch,
		Seal: func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error) {
			return append(append([]byte(nil), stored...), tag(chunkSeq, kind, firstOrdinal, rawBytes, stored)...), nil
		},
		Open: func(chunkSeq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error) {
			if len(stored) < format.AESGCMTagLen {
				return nil, errStubAuth
			}
			body := stored[:len(stored)-format.AESGCMTagLen]
			want := tag(chunkSeq, kind, firstOrdinal, rawBytes, body)
			if !hmac.Equal(want, stored[len(stored)-format.AESGCMTagLen:]) {
				return nil, errStubAuth
			}
			return body, nil
		},
	}
}

var errStubAuth = errors.New("stub auth failed")

func mustBuildEncrypted(t *testing.T, crypto *ChunkCrypto) []byte {
	t.Helper()
	b := NewBuilder(1)
	b.SetRowDedup(false)
	if err := b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull, BlockCount: 1}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddMetadata(format.MetadataIndexEntry{SnapshotID: 1, ObjectID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := b.AddBlock(format.BlockIndexEntry{BlockID: 11, SnapshotID: 1, TableID: 1}); err != nil {
		t.Fatal(err)
	}
	for _, r := range riSeq(30, 10) {
		r.SnapshotID = 1
		if err := b.AddRow(r); err != nil {
			t.Fatal(err)
		}
	}
	data, _, err := b.BuildStored(crypto, 0, nil, 0, crypto.Epoch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseStream(data, crypto, nil); err != nil {
		t.Fatalf("pristine encrypted txn rejected: %v", err)
	}
	return data
}

// TestEncryptedChunkRoundTrip seals every payload (snapshot + metadata +
// block chunks AND row-index pages) through the crypto hook and opens them
// again on the parse side. It also re-verifies that the plaintext-body CRC
// covers the *plaintext* stream: tampering with any entry must be caught by
// AEAD, not by the CRC.
func TestEncryptedChunkRoundTrip(t *testing.T) {
	crypto := newStubCrypto(7)
	data := mustBuildEncrypted(t, crypto)

	// Every chunk header must claim AESGCM encryption and the store epoch.
	chunks, _, fenceOff, pageCount := scanTxn(t, data)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	for i, c := range chunks {
		require.Equal(t, format.IndexChunkEncryptionAESGCM, c.raw.Encryption, "chunk %d encryption = %d, want AESGCM", i, c.raw.Encryption)
		require.Equal(t, uint32(7), c.raw.KeyEpoch, "chunk %d key epoch = %d, want 7", i, c.raw.KeyEpoch)
	}
	// The snapshot chunk's payload must differ from its plaintext: 72B entry
	// + 16B tag sealed via crypto.Seal.
	if chunks[0].raw.StoredBytes != format.SnapshotIndexEntrySize+format.AESGCMTagLen {
		t.Fatalf("snapshot stored = %d, want %d", chunks[0].raw.StoredBytes, format.SnapshotIndexEntrySize+format.AESGCMTagLen)
	}
	// Row index pages are sealed too: their stored sizes carry the tag
	// (zstd shrinks them below raw, so equality with raw is impossible).
	for i := 0; i < int(pageCount); i++ {
		var f format.RowIndexFenceEntry
		if err := f.Unmarshal(data[fenceOff+i*format.IndexFenceEntrySize:]); err != nil {
			t.Fatal(err)
		}
		if f.StoredSize == f.RawSize {
			t.Fatalf("page %d stored %d == raw %d: tag missing", i, f.StoredSize, f.RawSize)
		}
	}
}

// TestEncryptedChunkRejectsWrongEpoch pins the parse-side key-epoch check.
func TestEncryptedChunkRejectsWrongEpoch(t *testing.T) {
	data := mustBuildEncrypted(t, newStubCrypto(7))
	if _, err := parseStream(data, newStubCrypto(8), nil); err == nil || !strings.Contains(err.Error(), "key epoch") {
		t.Fatalf("want key epoch rejection, got %v", err)
	}
}

// TestEncryptedChunkRejectsTamperedPayload flips a ciphertext byte in the
// first chunk (re-stamping only the payload CRC, which sits below the AEAD):
// Open must fail authentication.
func TestEncryptedChunkRejectsTamperedPayload(t *testing.T) {
	crypto := newStubCrypto(7)
	data := mustBuildEncrypted(t, crypto)
	chunks, _, _, _ := scanTxn(t, data)
	restampChunk(t, data, chunks[0].off, func(h *format.IndexChunkHeader) {
		data[chunks[0].paylo+2] ^= 0xFF
		h.PayloadCRC32C = format.CRC32C(data[chunks[0].paylo : chunks[0].paylo+int(h.StoredBytes)])
	})
	if _, err := parseStream(data, crypto, nil); err == nil || !strings.Contains(err.Error(), "chunk 0 open") {
		t.Fatalf("want chunk-open auth failure, got %v", err)
	}
}

// TestEncryptedPageRejectsTamperedPayload flips a sealed row-index page: the
// page-open arm (not the page CRC) must fire.
func TestEncryptedPageRejectsTamperedPayload(t *testing.T) {
	crypto := newStubCrypto(7)
	data := mustBuildEncrypted(t, crypto)
	_, _, fenceOff, _ := scanTxn(t, data)
	var f format.RowIndexFenceEntry
	if err := f.Unmarshal(data[fenceOff:]); err != nil {
		t.Fatal(err)
	}
	base := uint64(format.IndexTxnHeaderSize)
	pos := base + f.StoredOffset
	data[pos+2] ^= 0xFF
	if _, err := parseStream(data, crypto, nil); err == nil || !strings.Contains(err.Error(), "row index page 0 open") {
		t.Fatalf("want page-open auth failure, got %v", err)
	}
}

// TestEncryptedSealFailurePropagates wires a Seal that fails: building must
// surface the error from both the chunk and the page sealing arms.
func TestEncryptedSealFailurePropagates(t *testing.T) {
	boom := errors.New("sealboom")
	b := NewBuilder(1)
	if err := b.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 1, SnapshotType: format.SnapshotFull}); err != nil {
		t.Fatal(err)
	}
	crypto := &ChunkCrypto{Epoch: 1, Seal: func(uint32, uint8, uint32, int, []byte) ([]byte, error) { return nil, boom }}
	// No pages: the snapshot chunk sealing fails.
	if _, _, err := b.BuildStored(crypto, 0, nil, 0, 1); err == nil || !strings.Contains(err.Error(), "sealboom") {
		t.Fatalf("chunk seal error not surfaced: %v", err)
	}
}

// TestEncryptedPlaintextCRCStreams pins that AssembleTxn stamps the header
// key epoch and that a plain parse refuses an encrypted txn.
func TestEncryptedPlaintextCRCStreams(t *testing.T) {
	crypto := newStubCrypto(3)
	data := mustBuildEncrypted(t, crypto)
	var h format.IndexTxnHeader
	if err := h.Unmarshal(data); err != nil {
		t.Fatal(err)
	}
	require.Equal(t, uint32(3), format.IndexTxnHeaderKeyEpoch(data[:format.IndexTxnHeaderSize]), "header key epoch must be 3")
	if _, err := parseStream(data, nil, nil); err == nil {
		t.Fatal("plain parse accepted an encrypted txn")
	}
}
