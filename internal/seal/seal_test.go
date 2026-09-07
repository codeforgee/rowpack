package seal

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
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
		if err != nil {
			t.Fatalf("seal %d: %v", n, err)
		}
		if len(ct) != n+fileformat.AESGCMTagLen {
			t.Fatalf("seal %d: ciphertext %d, want %d", n, len(ct), n+fileformat.AESGCMTagLen)
		}
		got, err := c.Open(0, h.BlockID, u, h, ct)
		if err != nil {
			t.Fatalf("open %d: %v", n, err)
		}
		if !bytes.Equal(got, pt) {
			t.Fatalf("open %d: payload mismatch", n)
		}
	}
}

// TestWrongKey covers a wrong key being rejected.
func TestWrongKey(t *testing.T) {
	c := testCipher(t)
	other, err := NewCipher(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	h := testHeader()
	u := testUUID()
	ct, err := c.Seal(0, h.BlockID, u, h, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Open(0, h.BlockID, u, h, ct); err == nil {
		t.Fatal("wrong key accepted")
	}
}

// TestCiphertextTamper flips every byte of a ciphertext and requires
// authentication failure for each position.
func TestCiphertextTamper(t *testing.T) {
	c := testCipher(t)
	h := testHeader()
	u := testUUID()
	pt := bytes.Repeat([]byte{0x77}, 300)
	ct, err := c.Seal(0, h.BlockID, u, h, pt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ct {
		mut := append([]byte(nil), ct...)
		mut[i] ^= 0x01
		if _, err := c.Open(0, h.BlockID, u, h, mut); err == nil {
			t.Fatalf("tampered byte %d accepted", i)
		}
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
	if err != nil {
		t.Fatal(err)
	}
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
		if got, err := c.Open(0, base.BlockID, u, &hh, ct); err == nil {
			t.Fatalf("tampered %s accepted (got %x)", tc.name, got)
		}
	}

	// Store UUID change also breaks authentication (AAD includes it).
	u2 := [16]byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := c.Open(0, base.BlockID, &u2, base, ct); err == nil {
		t.Fatal("changed store uuid accepted")
	}

	// The Encrypted flag and KeyEpoch are NOT part of the AAD: with the same
	// nonce arguments (epoch, blockID) they must not affect authentication.
	hh := *base
	hh.Encrypted = false
	hh.KeyEpoch = 9
	if _, err := c.Open(0, base.BlockID, u, &hh, ct); err != nil {
		t.Fatalf("flag/epoch must not be AAD-bound: %v", err)
	}
}

// TestNonceUnique ensures distinct (epoch, blockID) pairs produce distinct
// nonces.
func TestNonceUnique(t *testing.T) {
	seen := map[[fileformat.EncNonceLen]byte]bool{}
	for _, epoch := range []uint32{0, 1, 0xFFFFFFFF} {
		for _, id := range []uint64{0, 1, 2, 1000, 0xFFFFFFFFFFFFFFFF} {
			n := Nonce(epoch, id)
			if seen[n] {
				t.Fatalf("duplicate nonce for epoch %d block %d", epoch, id)
			}
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
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Open(1, h.BlockID, u, h, ct); err == nil {
		t.Fatal("changed epoch accepted")
	}
	hh := testHeader()
	hh.BlockID++
	if _, err := c.Open(0, hh.BlockID, u, hh, ct); err == nil {
		t.Fatal("changed block id accepted")
	}
}

// TestNonceLayout locks the exact nonce byte layout.
func TestNonceLayout(t *testing.T) {
	n := Nonce(0x01020304, 0x0807060504030201)
	want := [fileformat.EncNonceLen]byte{
		0x04, 0x03, 0x02, 0x01, // epoch LE
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, // blockID LE
	}
	if n != want {
		t.Fatalf("nonce = %x, want %x", n, want)
	}
}