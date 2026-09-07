package seal

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher(bytes.Repeat([]byte{0x42}, 32))
	require.NoError(t, err)
	return c
}

func testHeader() *fileformat.BlockHeader {
	return &fileformat.BlockHeader{
		BlockKind:   fileformat.BlockKindRows,
		Compression: fileformat.CompressionZstd,
		BlockID:     7,
		SnapshotID:  42,
		TableID:     3,
		ItemCount:   512,
		RawSize:     260000,
		StoredSize:  100000,
		Encrypted:   true,
		KeyEpoch:    0,
	}
}

func testUUID() *[16]byte {
	u := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	return &u
}

// TestRoundTrip covers plaintext of every interesting size, including empty
// and exactly one AES block.
func TestRoundTrip(t *testing.T) {
	c := testCipher(t)
	h := testHeader()
	u := testUUID()
	sizes := []int{0, 1, 15, 16, 17, 255, 256, 4096, 256 << 10}
	for _, n := range sizes {
		pt := bytes.Repeat([]byte{0xAB}, n)
		ct, err := c.Seal(0, h.BlockID, u, h, pt)
		require.NoError(t, err, "seal %d", n)
		require.Equal(t, n+fileformat.AESGCMTagLen, len(ct), "seal %d: ciphertext %d, want %d", n, len(ct), n+fileformat.AESGCMTagLen)
		got, err := c.Open(0, h.BlockID, u, h, ct)
		require.NoError(t, err, "open %d", n)
		require.True(t, bytes.Equal(got, pt), "open %d: payload mismatch", n)
	}
}

// TestWrongKey covers a wrong key being rejected.
func TestWrongKey(t *testing.T) {
	c := testCipher(t)
	other, err := NewCipher(bytes.Repeat([]byte{0x24}, 32))
	require.NoError(t, err)
	h := testHeader()
	u := testUUID()
	ct, err := c.Seal(0, h.BlockID, u, h, []byte("payload"))
	require.NoError(t, err)
	_, err = other.Open(0, h.BlockID, u, h, ct)
	require.Error(t, err, "wrong key accepted")
}

// TestCiphertextTamper flips every byte of a ciphertext and requires
// authentication failure for each position.
func TestCiphertextTamper(t *testing.T) {
	c := testCipher(t)
	h := testHeader()
	u := testUUID()
	pt := bytes.Repeat([]byte{0x77}, 300)
	ct, err := c.Seal(0, h.BlockID, u, h, pt)
	require.NoError(t, err)
	for i := range ct {
		mut := append([]byte(nil), ct...)
		mut[i] ^= 0x01
		_, err := c.Open(0, h.BlockID, u, h, mut)
		require.Error(t, err, "tampered byte %d accepted", i)
	}
}

// TestAADTamper requires authentication failure when any AAD-binding header
// field differs at open time. The nonce stays fixed (same epoch and blockID
// arguments), so failures are purely AAD-driven.
func TestAADTamper(t *testing.T) {
	c := testCipher(t)
	u := testUUID()
	base := testHeader()
	ct, err := c.Seal(0, base.BlockID, u, base, []byte("payload"))
	require.NoError(t, err)
	cases := []struct {
		name   string
		mutate func(h *fileformat.BlockHeader)
	}{
		{"block kind", func(h *fileformat.BlockHeader) { h.BlockKind ^= 0xFF }},
		{"compression", func(h *fileformat.BlockHeader) { h.Compression ^= 0xFF }},
		{"snapshot id", func(h *fileformat.BlockHeader) { h.SnapshotID ^= 0x01 }},
		{"table id", func(h *fileformat.BlockHeader) { h.TableID ^= 0x01 }},
		{"item count", func(h *fileformat.BlockHeader) { h.ItemCount ^= 0x01 }},
		{"raw size", func(h *fileformat.BlockHeader) { h.RawSize ^= 0x01 }},
		{"stored size", func(h *fileformat.BlockHeader) { h.StoredSize ^= 0x01 }},
	}
	for _, tc := range cases {
		hh := *base
		tc.mutate(&hh)
		// Same nonce arguments, mutated AAD: must fail.
		got, err := c.Open(0, base.BlockID, u, &hh, ct)
		require.Error(t, err, "tampered %s accepted (got %x)", tc.name, got)
	}

	// Store UUID change also breaks authentication (AAD includes it).
	u2 := [16]byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	_, err = c.Open(0, base.BlockID, &u2, base, ct)
	require.Error(t, err, "changed store uuid accepted")

	// The Encrypted flag and KeyEpoch are NOT part of the AAD: with the same
	// nonce arguments (epoch, blockID) they must not affect authentication.
	hh := *base
	hh.Encrypted = false
	hh.KeyEpoch = 9
	_, err = c.Open(0, base.BlockID, u, &hh, ct)
	require.NoError(t, err, "flag/epoch must not be AAD-bound: %v", err)
}

// TestNonceUnique ensures distinct (epoch, blockID) pairs produce distinct
// nonces.
func TestNonceUnique(t *testing.T) {
	seen := map[[fileformat.EncNonceLen]byte]bool{}
	for _, epoch := range []uint32{0, 1, 0xFFFFFFFF} {
		for _, id := range []uint64{0, 1, 2, 1000, 0xFFFFFFFFFFFFFFFF} {
			n := Nonce(epoch, id)
			require.False(t, seen[n], "duplicate nonce for epoch %d block %d", epoch, id)
			seen[n] = true
		}
	}
}

// TestEpochBlockIDBothAuthenticate proves epoch and blockID changes break
// authentication (they change the nonce).
func TestEpochBlockIDBothAuthenticate(t *testing.T) {
	c := testCipher(t)
	h := testHeader()
	u := testUUID()
	ct, err := c.Seal(0, h.BlockID, u, h, []byte("payload"))
	require.NoError(t, err)
	_, err = c.Open(1, h.BlockID, u, h, ct)
	require.Error(t, err, "changed epoch accepted")
	hh := testHeader()
	hh.BlockID++
	_, err = c.Open(0, hh.BlockID, u, hh, ct)
	require.Error(t, err, "changed block id accepted")
}

// TestNonceLayout locks the exact nonce byte layout.
func TestNonceLayout(t *testing.T) {
	n := Nonce(0x01020304, 0x0807060504030201)
	want := [fileformat.EncNonceLen]byte{
		0x04, 0x03, 0x02, 0x01, // epoch LE
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // blockID LE
	}
	require.Equal(t, want, n, "nonce = %x, want %x", n, want)
}
