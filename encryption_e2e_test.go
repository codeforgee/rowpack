package rowpack

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

// tamperFirstRowsPayload flips one byte mid-ciphertext of the first Rows
// block so block/rows reads and verify must fail with ErrAuthFailed (or, for
// a plain store, a CRC mismatch).
func tamperFirstRowsPayload(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer f.Close()
	off := int64(fileformat.DataFileHeaderSize + fileformat.SnapshotHeaderSize)
	for {
		var bh fileformat.BlockHeader
		var bhBuf [fileformat.BlockHeaderSize]byte
		if _, err := f.ReadAt(bhBuf[:], off); err != nil {
			require.NoError(t, err)
		}
		require.NoError(t, bh.Unmarshal(bhBuf[:]))
		if bh.BlockKind == fileformat.BlockKindRows {
			flip := off + fileformat.BlockHeaderSize + int64(bh.StoredSize)/2
			b := make([]byte, 1)
			if _, err := f.ReadAt(b, flip); err != nil {
				require.NoError(t, err)
			}
			b[0] ^= 0xFF
			if _, err := f.WriteAt(b, flip); err != nil {
				require.NoError(t, err)
			}
			return
		}
		if string(bhBuf[0:8]) == "RPKSNAPF" {
			require.Fail(t, "no rows block found")
		}
		off += fileformat.BlockHeaderSize + int64(bh.StoredSize)
	}
}

// TestEncryptedStoreReadAfterReopen is the end-to-end round trip: FULL +
// DELTA (updates/deletes) written to an encrypted store, read back via
// Get/Scan/ReadBatch/Exists, closed, reopened with the key, and verified
// against both the FULL and the DELTA-head snapshots.
func TestEncryptedStoreReadAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := tmpdb(t)
	keyID := "k1"
	base := dir + "/db"
	cfg := func() *EncryptionConfig {
		return &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: keyID, key: testKey(keyID)},
			KeyID:       keyID,
		}
	}

	db, err := Create(base, Options{Encryption: cfg()}) // default Zstd compression
	require.NoError(t, err)
	full := writeFullSnapshot(t, db, 100)
	require.NoError(t, assertRows(t, db, full.ID, 100))

	// DELTA: update row 10, delete row 20.
	w, err := db.BeginSnapshot(ctx, SnapshotDelta, SnapshotOptions{Parent: full.ID})
	require.NoError(t, err)
	require.NoError(t, w.Update(ctx, 1, 10, 1, row1(10_000)))
	require.NoError(t, w.Delete(ctx, 1, 20))
	head, err := w.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, checkHeadRows(t, db, head.ID, full.ID))

	// Batch read against the head.
	rows, err := db.ReadBatch(ctx, head.ID, 1, []RowID{1, 2, 10})
	require.NoError(t, err)
	require.Len(t, rows, 3, "batch rows = %d, want 3", len(rows))
	name, _ := rows[2][1].String()
	require.Equal(t, "row-10000", name, "updated row name = %q, want row-10000", name)
	db.Close()

	// Reopen with the key: key id comes from the disk header, the config's
	// KeyID is ignored at Open.
	db2, err := Open(base, Options{Encryption: cfg()})
	require.NoError(t, err)
	defer db2.Close()

	row, err := db2.Get(ctx, full.ID, 1, 5, nil)
	require.NoError(t, err)
	n, _ := row[1].String()
	require.Equal(t, "row-5", n, "row 5 at FULL = %q, want row-5", n)
	row, err = db2.Get(ctx, head.ID, 1, 10, nil)
	require.NoError(t, err)
	n, _ = row[1].String()
	require.Equal(t, "row-10000", n, "row 10 at head = %q, want row-10000", n)
	_, err = db2.Get(ctx, head.ID, 1, 20, nil)
	require.ErrorIs(t, err, ErrNotFound, "deleted row 20 = %v, want ErrNotFound", err)
	ok, err := db2.Exists(ctx, head.ID, 1, 20)
	require.NoError(t, err, "Exists(deleted) = %v/%v, want false/nil", ok, err)
	require.False(t, ok, "Exists(deleted) = %v/%v, want false/nil", ok, err)
	// Full-table scan at the head: 99 rows.
	count := 0
	it, err := db2.Scan(ctx, head.ID, 1, ScanOptions{})
	require.NoError(t, err)
	for {
		_, ok := it.Next()
		if !ok {
			break
		}
		count++
	}
	require.NoError(t, it.Err())
	require.Equal(t, 99, count, "scan rows = %d, want 99", count)
	it.Close()
}

// TestEncryptedStoreKeyRequired covers the key contract at Open.
func TestEncryptedStoreKeyRequired(t *testing.T) {
	dir := tmpdb(t)
	base := dir + "/db"
	db, err := Create(base, encOptions("k2"))
	require.NoError(t, err)
	writeFullSnapshot(t, db, 10)
	db.Close()

	_, err = Open(base, Options{})
	require.ErrorIs(t, err, ErrKeyRequired, "open without key = %v, want ErrKeyRequired", err)
	_, err = Open(base, Options{ReadOnly: true})
	require.ErrorIs(t, err, ErrKeyRequired, "read-only open without key = %v, want ErrKeyRequired", err)
	// Provider that does not know the disk key id.
	_, err = Open(base, Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "wrong", key: testKey("wrong")},
		KeyID:       "wrong",
	}})
	require.Error(t, err, "open with wrong key provider = %v, want key error", err)
	require.False(t, isErr(err, ErrKeyRequired), "open with wrong key provider = %v, want key error", err)
}

// TestEncryptedStoreWrongKeyCoverage double-checks the wrong-key path resolves
// cleanly (provider rejects the disk key id).
func TestEncryptedStoreWrongKeyCoverage(t *testing.T) {
	dir := tmpdb(t)
	base := dir + "/db"
	db, err := Create(base, encOptions("k3"))
	require.NoError(t, err)
	writeFullSnapshot(t, db, 10)
	db.Close()

	_, err = Open(base, Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k3", key: testKey("k3")[:31]}, // wrong length key
		KeyID:       "k3",
	}})
	require.Error(t, err, "open with malformed key succeeded")
}

// TestEncryptedStoreTamper flips one byte inside the first Rows block's
// ciphertext on disk and requires ErrAuthFailed on read.
func TestEncryptedStoreTamper(t *testing.T) {
	dir := tmpdb(t)
	base := dir + "/db"
	db, err := Create(base, Options{
		Encryption: &EncryptionConfig{
			KeyProvider: &staticKeyProvider{keyID: "k4", key: testKey("k4")},
			KeyID:       "k4",
		},
		Compression: CompressionNone,
	})
	require.NoError(t, err)
	writeFullSnapshot(t, db, 50)
	db.Close()

	// Flip a byte in the middle of the first Rows block payload.
	tamperFirstRowsPayload(t, base+".rpk")

	db2, err := Open(base, Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k4", key: testKey("k4")},
		KeyID:       "k4",
	}})
	require.NoError(t, err)
	defer db2.Close()
	_, err = db2.Get(context.Background(), 1, 1, 1, nil)
	require.ErrorIs(t, err, ErrAuthFailed, "read tampered block = %v, want ErrAuthFailed", err)
}

// TestEncryptedStoreConcurrentReads exercises the decrypter's per-epoch
// cipher under concurrent readers (run with -race).
func TestEncryptedStoreConcurrentReads(t *testing.T) {
	ctx := context.Background()
	dir := tmpdb(t)
	base := dir + "/db"
	db, err := Create(base, Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k5", key: testKey("k5")},
		KeyID:       "k5",
	}})
	require.NoError(t, err)
	writeFullSnapshot(t, db, 2000)
	db.Close()

	db2, err := Open(base, Options{Encryption: &EncryptionConfig{
		KeyProvider: &staticKeyProvider{keyID: "k5", key: testKey("k5")},
		KeyID:       "k5",
	}})
	require.NoError(t, err)
	defer db2.Close()
	const workers = 8
	done := make(chan error, workers)
	for w := 0; w < workers; w++ {
		start := RowID(w*250 + 1)
		go func() {
			for i := uint64(0); i < 250; i++ {
				id := start + RowID(i)
				row, err := db2.Get(ctx, 1, 1, id, nil)
				if err != nil {
					done <- err
					return
				}
				if n, _ := row[1].String(); n != "row-"+itoa(uint64(id)) {
					done <- errorf("row %d name %q", id, n)
					return
				}
			}
			done <- nil
		}()
	}
	for w := 0; w < workers; w++ {
		require.NoError(t, <-done)
	}
}

// assertRows checks rows 1..n against Get on a snapshot.
func assertRows(t *testing.T, db *Store, snap SnapshotID, n uint64) error {
	t.Helper()
	for i := uint64(1); i <= n; i++ {
		row, err := db.Get(context.Background(), snap, 1, i, nil)
		if err != nil {
			return err
		}
		if s, _ := row[1].String(); s != "row-"+itoa(i) {
			return errorf("row %d name %q", i, s)
		}
	}
	return nil
}

// checkHeadRows verifies the DELTA head view: row 10 updated, row 20 gone,
// all others equal to FULL.
func checkHeadRows(t *testing.T, db *Store, head, full SnapshotID) error {
	t.Helper()
	for i := uint64(1); i <= 100; i++ {
		if i == 20 {
			if _, err := db.Get(context.Background(), head, 1, i, nil); !isErr(err, ErrNotFound) {
				return errorf("row %d at head = %v, want ErrNotFound", i, err)
			}
			continue
		}
		want := "row-" + itoa(i)
		if i == 10 {
			want = "row-10000"
		}
		row, err := db.Get(context.Background(), head, 1, i, nil)
		if err != nil {
			return err
		}
		if s, _ := row[1].String(); s != want {
			return errorf("row %d at head name %q, want %q", i, s, want)
		}
	}
	return nil
}

func errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
