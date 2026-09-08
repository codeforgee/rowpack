package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/seal"
	"github.com/stretchr/testify/require"
)

// ---- V2-M6: 加密 IndexTxn（R11 nonce 域分离 / R12 密文 CRC）----

// TestSealNonceDomainsAreDisjoint is the R11 gate: index-domain nonces always
// carry bit 31 of the epoch word; block-domain nonces never do — no counter
// value can make a block nonce collide with an index nonce.
func TestSealNonceDomainsAreDisjoint(t *testing.T) {
	for _, epoch := range []uint32{0, 1, 42, ^uint32(0) >> 1} {
		for _, ctr := range []uint64{0, 1, 1 << 40, ^uint64(0)} {
			block := seal.Nonce(epoch, ctr)
			index := seal.NonceIndex(epoch, ctr)
			require.NotEqual(t, block, index, "epoch %d ctr %d", epoch, ctr)
			// The domain bit is exactly bit 31 of the epoch word.
			require.Zero(t, block[3]&0x80, "block nonce must have bit31 clear")
			require.NotZero(t, index[3]&0x80, "index nonce must have bit31 set")
		}
	}
	// Same plaintext under both domains still authenticates independently.
	key := testKey("dom")
	c, err := seal.NewCipher(key)
	require.NoError(t, err)
	pt := []byte("payload")
	ctBlock := c.SealWith(seal.Nonce(0, 7), []byte("aad-block"), pt)
	ctIndex := c.SealWith(seal.NonceIndex(0, 7), []byte("aad-index"), pt)
	// Cross-domain opens must fail (AAD and nonce both differ).
	aadB := []byte("aad-block")
	aadI := []byte("aad-index")
	_, err = c.OpenWith(seal.NonceIndex(0, 7), aadB, ctBlock)
	require.Error(t, err)
	_, err = c.OpenWith(seal.Nonce(0, 7), aadI, ctIndex)
	require.Error(t, err)
	_, err = c.OpenWith(seal.NonceIndex(0, 7), aadI, ctBlock)
	require.Error(t, err)
	_, err = c.OpenWith(seal.Nonce(0, 7), aadB, ctIndex)
	require.Error(t, err)
}

// TestEncryptedIndexTxnTamper flips a byte inside a stored encrypted index
// txn body: the footer-bound CRC (over stored ciphertext, R12) detects the
// tear without a key, and open rebuilds that snapshot's index in memory.
func TestEncryptedIndexTxnTamper(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc-txn-tamper")
	keyID := "tx"
	enc := func() Options { return encOptions(keyID) }
	db, _ := buildConcurrentStore(t, base, enc())
	w, _ := db.BeginSnapshot(context.Background(), SnapshotDelta, SnapshotOptions{Parent: 1})
	require.NoError(t, w.Insert(context.Background(), 1, 7000, 1, Row{Uint64(7000), String("t")}))
	_, err := w.Commit(context.Background())
	require.NoError(t, err)
	db.Close()

	// Locate the FIRST stored IndexTxn and flip one ciphertext body byte.
	data, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	first := -1
	for i := 0; i+8 <= len(data); i++ {
		if string(data[i:i+8]) == fileformat.MagicIndexTxnHdr {
			first = i
			break
		}
	}
	require.Greater(t, first, 0, "no IndexTxnHeader found")
	var h fileformat.IndexTxnHeader
	require.NoError(t, h.Unmarshal(data[first : first+fileformat.IndexTxnHeaderSize]))
	require.NotZero(t, h.BodyBytes, "encrypted txn must carry a nonzero stored body")
	body := first + fileformat.IndexTxnHeaderSize + 10 // inside the ciphertext
	data[body] ^= 0xFF
	require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))

	db2, err := Open(base, enc())
	require.NoError(t, err, "open with tampered encrypted txn: %v", err)
	defer db2.Close()
	require.Equal(t, uint64(1), db2.Stats().Recovery.SnapshotsRebuilt, "SnapshotsRebuilt = %d, want 1", db2.Stats().Recovery.SnapshotsRebuilt)
	r, err := db2.Get(context.Background(), 1, 1, 42, nil)
	require.NoError(t, err)
	v, _ := r[1].String()
	require.Equal(t, "n-42", v, "row 42 after in-memory rebuild")
}

// countingKeyProvider counts Key calls: the M6 gate requires batch/scan reads
// not to re-resolve keys per row or per block.
type countingKeyProvider struct {
	staticKeyProvider
	calls atomic.Int64
}

func (p *countingKeyProvider) Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error) {
	p.calls.Add(1)
	return p.staticKeyProvider.Key(ctx, keyID, epoch)
}

// TestEncryptedBatchKeyResolution verifies an encrypted batch read resolves
// the key a bounded number of times (once per epoch cipher), not once per
// row or block.
func TestEncryptedBatchKeyResolution(t *testing.T) {
	base := filepath.Join(tmpdb(t), "enc-batch")
	keyID := "bk"
	p := &countingKeyProvider{staticKeyProvider: staticKeyProvider{keyID: keyID, key: testKey(keyID)}}
	opts := Options{Encryption: &EncryptionConfig{KeyProvider: p, KeyID: keyID}}
	db, full := buildConcurrentStore(t, base, opts)

	ids := make([]RowID, 0, 500)
	for i := uint64(1); i <= 500; i++ {
		ids = append(ids, i)
	}
	it, err := db.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{})
	require.NoError(t, err)
	n := 0
	for {
		_, _, ok := it.Next(nil)
		if !ok {
			break
		}
		n++
	}
	require.NoError(t, it.Err())
	require.Equal(t, 500, n)
	before := p.calls.Load()
	require.NoError(t, db.Close())

	// Reopen and read again: one new key resolution at Open (write-path
	// cipher check), none added per row/block during the batch.
	db2, err := Open(base, opts)
	require.NoError(t, err)
	defer db2.Close()
	afterOpen := p.calls.Load()
	it2, err := db2.ReadRowsByIDs(context.Background(), full, 1, ids, BatchReadOptions{})
	require.NoError(t, err)
	for {
		_, _, ok := it2.Next(nil)
		if !ok {
			break
		}
	}
	require.NoError(t, it2.Err())
	require.LessOrEqual(t, p.calls.Load()-afterOpen, int64(2),
		"batch reads must not re-resolve keys per row/block: %d -> %d", before, p.calls.Load())
}
