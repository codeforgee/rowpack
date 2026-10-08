package seal

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

func TestNonceLayout(t *testing.T) {
	n := Nonce(0x01020304, 0x0A0B0C0D0E0F1011)
	want := [format.EncNonceLen]byte{
		0x04, 0x03, 0x02, 0x01, // epoch LE
		0x11, 0x10, 0x0F, 0x0E, 0x0D, 0x0C, 0x0B, 0x0A, // blockID LE
	}
	require.Equal(t, want, n, "Nonce = %x, want %x", n, want)
	if Nonce(1, 2) == Nonce(2, 1) {
		t.Fatal("distinct (epoch, blockID) pairs must not collide")
	}
}

func TestNonceIndexSetsDomainBit(t *testing.T) {
	epoch := uint32(5)
	n := Nonce(epoch|IndexDomainBit, 9)
	if got := binary.LittleEndian.Uint32(n[0:4]); got != epoch|IndexDomainBit {
		t.Fatalf("index nonce epoch word %x, want %x", got, epoch|IndexDomainBit)
	}
	require.Equal(t, binary.LittleEndian.Uint64(n[4:12]), binary.LittleEndian.Uint64(n[4:12]), "index nonce txn sequence")
	// A block nonce for the same counters must differ from the index nonce.
	if Nonce(epoch|IndexDomainBit, 9) == Nonce(epoch, 9) {
		t.Fatal("index nonce collides with block nonce")
	}
}

func TestBuildAADLayout(t *testing.T) {
	uuid := testUUID
	h := blockHdr(0x1122334455667788, 0x99AABBCCDDEEFF00, 77)
	aad := BuildAAD(&uuid, &h)

	if string(aad[0:12]) != "RowPackBlock" {
		t.Fatalf("AAD magic %q", aad[0:12])
	}
	if !bytes.Equal(aad[16:32], uuid[:]) {
		t.Fatal("AAD does not bind the store UUID")
	}
	if aad[32] != byte(format.BlockKindRows) || aad[33] != byte(format.CompressionZstd) {
		t.Fatalf("kind/comp bytes %x %x", aad[32], aad[33])
	}
	if aad[34] != 0 || aad[35] != 0 {
		t.Fatal("reserved bytes must be zero")
	}
	checkU64 := func(off int, want uint64, name string) {
		t.Helper()
		require.Equal(t, want, binary.LittleEndian.Uint64(aad[off:off+8]), "%s: got %d", name, binary.LittleEndian.Uint64(aad[off:off+8]))
	}
	checkU64(36, h.BlockID, "BlockID")
	checkU64(44, h.SnapshotID, "SnapshotID")
	checkU32 := func(off int, want uint32, name string) {
		t.Helper()
		require.Equal(t, want, binary.LittleEndian.Uint32(aad[off:off+4]), "%s: got %d", name, binary.LittleEndian.Uint32(aad[off:off+4]))
	}
	checkU32(52, h.TableID, "TableID")
	checkU32(56, h.ItemCount, "ItemCount")
	checkU32(60, h.RawSize, "RawSize")
	checkU32(64, h.StoredSize, "StoredSize")

	// Deterministic.
	if BuildAAD(&uuid, &h) != aad {
		t.Fatal("BuildAAD is not deterministic")
	}
}

func TestBuildAADIndexLayout(t *testing.T) {
	uuid := testUUID
	aad := BuildAADIndex(&uuid, 1, 2, 3, 4)
	if string(aad[0:12]) != "RowPackIndex" {
		t.Fatalf("AAD magic %q", aad[0:12])
	}
	if !bytes.Equal(aad[16:32], uuid[:]) {
		t.Fatal("AAD does not bind the store UUID")
	}
	for _, c := range []struct {
		off  int
		want uint64
		name string
	}{
		{32, 1, "SnapshotID"}, {40, 2, "txnStart"}, {48, 3, "txnEnd"},
	} {
		require.Equal(t, c.want, binary.LittleEndian.Uint64(aad[c.off:c.off+8]), "%s: got %d", c.name, binary.LittleEndian.Uint64(aad[c.off:c.off+8]))
	}
	require.Equal(t, uint32(4), binary.LittleEndian.Uint32(aad[56:60]), "AAD epoch = %d, want 4", binary.LittleEndian.Uint32(aad[56:60]))
	if len(aad) != AADIndexSize {
		t.Fatalf("AADIndexSize = %d, want %d", len(aad), AADIndexSize)
	}
}

func TestIndexChunkAADLayout(t *testing.T) {
	uuid := testUUID
	aad := (ChunkContext{UUID: &uuid, TxnSequence: 11, SnapshotID: 22, ChunkSequence: 33,
		FirstOrdinal: 44, RawBytes: 55, StoredBytes: 66, Kind: 7, Epoch: 88}).AAD()
	if string(aad[0:12]) != "RowPackIChkV" {
		t.Fatalf("AAD magic %q", aad[0:12])
	}
	if !bytes.Equal(aad[16:32], uuid[:]) {
		t.Fatal("AAD does not bind the store UUID")
	}
	for _, c := range []struct {
		off  int
		want uint64
		name string
	}{
		{32, 11, "TxnSequence"}, {40, 22, "SnapshotID"},
	} {
		require.Equal(t, c.want, binary.LittleEndian.Uint64(aad[c.off:c.off+8]), "%s: got %d", c.name, binary.LittleEndian.Uint64(aad[c.off:c.off+8]))
	}
	for _, c := range []struct {
		off  int
		want uint32
		name string
	}{
		{48, 33, "ChunkSequence"}, {52, 44, "FirstEntryOrdinal"},
		{60, 55, "RawBytes"}, {64, 66, "StoredBytes"}, {68, 88, "KeyEpoch"},
	} {
		require.Equal(t, c.want, binary.LittleEndian.Uint32(aad[c.off:c.off+4]), "%s: got %d", c.name, binary.LittleEndian.Uint32(aad[c.off:c.off+4]))
	}
	require.Equal(t, uint8(7), aad[56], "AAD EntryKind = %d, want 7", aad[56])
	if len(aad) != AADIndexChunkSize {
		t.Fatalf("AADIndexChunkSize = %d, want %d", len(aad), AADIndexChunkSize)
	}
}

func TestSealOpenBlockRoundtrip(t *testing.T) {
	c := testCipher(t)
	uuid := testUUID
	h := blockHdr(42, 7, 3)
	h.KeyEpoch = 5
	plaintext := []byte("the quick brown fox jumps over the lazy dog")

	ct, err := c.Seal(&uuid, &h, plaintext)
	require.NoError(t, err, "Seal")
	if len(ct) != len(plaintext)+format.AESGCMTagLen {
		t.Fatalf("ciphertext len %d, want plaintext + tag", len(ct))
	}
	if bytes.Equal(ct[:len(plaintext)], plaintext) {
		t.Fatal("ciphertext leaks plaintext")
	}

	pt, err := c.Open(&uuid, &h, ct)
	require.NoError(t, err, "Open")
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("roundtrip mismatch: %q", pt)
	}
}

func TestSealOpenBlockAuthFailures(t *testing.T) {
	c := testCipher(t)
	uuid := testUUID
	h := blockHdr(42, 7, 3)
	h.KeyEpoch = 5
	ct, err := c.Seal(&uuid, &h, []byte("payload"))
	require.NoError(t, err, "Seal")

	// Tampered ciphertext.
	bad := append([]byte(nil), ct...)
	bad[0] ^= 0xFF
	if _, err := c.Open(&uuid, &h, bad); !errors.Is(err, ErrAuth) {
		t.Fatalf("tampered ciphertext: err %v, want ErrAuth", err)
	}

	// Moved block: same payload under a different block ID / store.
	hBlock := h
	hBlock.BlockID = 43
	if _, err := c.Open(&uuid, &hBlock, ct); !errors.Is(err, ErrAuth) {
		t.Fatalf("moved block id: err %v, want ErrAuth", err)
	}
	otherUUID := testUUID
	otherUUID[0] ^= 0xFF
	if _, err := c.Open(&otherUUID, &h, ct); !errors.Is(err, ErrAuth) {
		t.Fatalf("cross-store move: err %v, want ErrAuth", err)
	}

	// Tampered header field (ItemCount is bound by the AAD).
	h2 := h
	h2.ItemCount++
	if _, err := c.Open(&uuid, &h2, ct); !errors.Is(err, ErrAuth) {
		t.Fatalf("header drift: err %v, want ErrAuth", err)
	}

	// Wrong key.
	var otherKey [32]byte
	for i := range otherKey {
		otherKey[i] = byte(i*7 + 1)
	}
	other, err := NewCipher(otherKey[:])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(&uuid, &h, ct); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong key: err %v, want ErrAuth", err)
	}
}

func TestSealWithOpenWith(t *testing.T) {
	c := testCipher(t)
	nonce := Nonce(3|IndexDomainBit, 1)
	aad := BuildAADIndex(&testUUID, 1, 2, 3, 4)
	pt := []byte("index txn bytes")

	ct := c.SealWith(nonce, aad[:], pt)
	if len(ct) != len(pt)+format.AESGCMTagLen {
		t.Fatalf("ciphertext len %d", len(ct))
	}
	got, err := c.OpenWith(nonce, aad[:], ct)
	require.NoError(t, err, "OpenWith")
	if !bytes.Equal(got, pt) {
		t.Fatalf("roundtrip mismatch: %q", got)
	}

	bad := append([]byte(nil), ct...)
	bad[len(bad)-1] ^= 0x01
	if _, err := c.OpenWith(nonce, aad[:], bad); !errors.Is(err, ErrAuth) {
		t.Fatalf("tampered: err %v, want ErrAuth", err)
	}
	if _, err := c.OpenWith(Nonce(3|IndexDomainBit, 2), aad[:], ct); !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong nonce: err %v, want ErrAuth", err)
	}
}

func TestSealOpenIndexChunk(t *testing.T) {
	c := testCipher(t)
	uuid := testUUID
	args := struct {
		txnSeq, snapshotID     uint64
		chunkSeq, firstOrdinal uint32
		rawBytes, storedBytes  uint32
		kind                   uint8
		epoch                  uint32
	}{11, 22, 33, 44, 55, 55 + format.AESGCMTagLen, 7, 88}
	pt := []byte("chunk payload")
	ctx := ChunkContext{
		UUID:          &uuid,
		TxnSequence:   args.txnSeq,
		SnapshotID:    args.snapshotID,
		ChunkSequence: args.chunkSeq,
		FirstOrdinal:  args.firstOrdinal,
		RawBytes:      args.rawBytes,
		StoredBytes:   args.storedBytes,
		Kind:          args.kind,
		Epoch:         args.epoch,
	}

	ct, err := c.SealIndexChunk(ctx, pt)
	require.NoError(t, err, "SealIndexChunk")
	got, err := c.OpenIndexChunk(ctx, ct)
	require.NoError(t, err, "OpenIndexChunk")
	if !bytes.Equal(got, pt) {
		t.Fatalf("roundtrip mismatch: %q", got)
	}

	// Chunk identity is bound: changing any AAD field breaks authentication.
	drift := ctx
	drift.ChunkSequence++
	if _, err := c.OpenIndexChunk(drift, ct); !errors.Is(err, ErrAuth) {
		t.Fatalf("chunk identity drift: err %v, want ErrAuth", err)
	}
}

func TestNonceIndexChunkDeterministic(t *testing.T) {
	c := testCipher(t)
	a := c.NonceIndexChunk(1, 2)
	b := c.NonceIndexChunk(1, 2)
	if a != b {
		t.Fatal("NonceIndexChunk is not deterministic")
	}
	if c.NonceIndexChunk(1, 2) == c.NonceIndexChunk(2, 1) {
		t.Fatal("(txnSeq, chunkSeq) pairs must not collide")
	}
	// Page and chunk nonce domains must be separated.
	if c.NonceIndexChunk(1, 2) == c.NoncePage(PageContext{
		UUID: &testUUID, SnapshotID: 1, BlockID: 2,
		Page: format.RowsPageDirEntry{PageOrdinal: 3}, Epoch: 4,
	}) {
		t.Fatal("chunk nonce collides with page nonce domain")
	}
}

func TestNewCipherKeyLength(t *testing.T) {
	if _, err := NewCipher(make([]byte, 31)); err == nil {
		t.Fatal("short key should error")
	}
	if _, err := NewCipher(make([]byte, 33)); err == nil {
		t.Fatal("long key should error")
	}
	if _, err := NewCipher(make([]byte, 32)); err != nil {
		t.Fatalf("32-byte key: %v", err)
	}
}
