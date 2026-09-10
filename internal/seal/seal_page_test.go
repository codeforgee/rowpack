package seal

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func testCipher(t *testing.T) *Cipher {
	t.Helper()
	var key [32]byte
	for i := range key {
		key[i] = byte(i * 7)
	}
	c, err := NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var testUUID = [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}

func pageDir(pageOrdinal, firstOrdinal, recordCount, storedSize, rawSize uint32, minID, maxID uint64) fileformat.RowsPageDirEntry {
	return fileformat.RowsPageDirEntry{
		PageOrdinal:        pageOrdinal,
		FirstRecordOrdinal: firstOrdinal,
		RecordCount:        recordCount,
		StoredOffset:       uint64(storedSize), // arbitrary; not used by AAD
		StoredSize:         storedSize,
		RawSize:            rawSize,
		MinRowID:           minID,
		MaxRowID:           maxID,
		PageCRC32C:         0xCAFEBABE,
	}
}

func blockHdr(blockID uint64, snap uint64, table uint32) fileformat.BlockHeader {
	return fileformat.BlockHeader{
		BlockKind:   fileformat.BlockKindRows,
		Compression: fileformat.CompressionZstd,
		BlockID:     blockID,
		SnapshotID:  snap,
		TableID:     table,
		ItemCount:   100,
		RawSize:     32 << 10,
		StoredSize:  16 << 10,
		KeyEpoch:    0,
	}
}

// TestPageNonceDomainSeparation: the Rows Page nonce is derived on its own
// domain (HMAC subkey + prefix) so it collides with neither the block nonce
// nor the index-txn / index-chunk nonce regardless of counter values.
func TestPageNonceDomainSeparation(t *testing.T) {
	c := testCipher(t)
	blockID := uint64(42)
	snap := uint64(7)
	epoch := uint32(0)
	ctx := func(uuid *[16]byte, snapshotID, block uint64, pageOrdinal, keyEpoch uint32) PageContext {
		return PageContext{
			UUID: uuid, BlockID: block, SnapshotID: snapshotID,
			Page: fileformat.RowsPageDirEntry{PageOrdinal: pageOrdinal}, Epoch: keyEpoch,
		}
	}
	pageN := c.NoncePage(ctx(&testUUID, snap, blockID, 0, epoch))

	if pageN == Nonce(epoch, blockID) {
		t.Fatal("page nonce equals block nonce")
	}
	if pageN == NonceIndex(epoch, blockID) {
		t.Fatal("page nonce equals index nonce")
	}
	if pageN == c.NonceIndexChunk(blockID, 0) {
		t.Fatal("page nonce equals index chunk nonce")
	}
	// Different pages / blocks / snapshots / epochs all differ.
	if c.NoncePage(ctx(&testUUID, snap, blockID, 0, epoch)) == c.NoncePage(ctx(&testUUID, snap, blockID, 1, epoch)) {
		t.Fatal("page 0 and page 1 share a nonce")
	}
	if c.NoncePage(ctx(&testUUID, snap, blockID, 0, epoch)) == c.NoncePage(ctx(&testUUID, snap, blockID+1, 0, epoch)) {
		t.Fatal("block 42 and block 43 share a page nonce")
	}
	// Different store UUIDs differ.
	var uuid2 [16]byte
	uuid2[0] = 0xFF
	if c.NoncePage(ctx(&testUUID, snap, blockID, 0, epoch)) == c.NoncePage(ctx(&uuid2, snap, blockID, 0, epoch)) {
		t.Fatal("two store UUIDs share a page nonce")
	}
}

// TestPageAADBinding: the AAD binds every field that determines the page's
// identity and parse semantics, so a sealed page moved to another block,
// page ordinal, table, or sealed size cannot authenticate.
func TestPageAADBinding(t *testing.T) {
	ctx := func(uuid *[16]byte, p fileformat.RowsPageDirEntry, h fileformat.BlockHeader) PageContext {
		return PageContext{
			UUID:        uuid,
			BlockID:     h.BlockID,
			SnapshotID:  h.SnapshotID,
			TableID:     h.TableID,
			Compression: h.Compression,
			Page:        p,
			Epoch:       h.KeyEpoch,
		}
	}
	baseAAD := ctx(&testUUID, pageDir(0, 0, 32, 16000, 8000, 1, 32), blockHdr(42, 7, 1)).AAD()
	// Any single-field change must flip the AAD.
	mutate := func(f func(*fileformat.RowsPageDirEntry, *fileformat.BlockHeader)) {
		page := pageDir(0, 0, 32, 16000, 8000, 1, 32)
		h := blockHdr(42, 7, 1)
		f(&page, &h)
		aad := ctx(&testUUID, page, h).AAD()
		if aad == baseAAD {
			t.Fatal("AAD did not bind the mutated field")
		}
	}
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.PageOrdinal = 1 })
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.FirstRecordOrdinal = 1 })
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.RecordCount = 33 })
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.StoredSize = 16001 })
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.RawSize = 8001 })
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.MinRowID = 2 })
	mutate(func(p *fileformat.RowsPageDirEntry, _ *fileformat.BlockHeader) { p.MaxRowID = 33 })
	mutate(func(_ *fileformat.RowsPageDirEntry, h *fileformat.BlockHeader) { h.BlockID = 43 })
	mutate(func(_ *fileformat.RowsPageDirEntry, h *fileformat.BlockHeader) { h.SnapshotID = 8 })
	mutate(func(_ *fileformat.RowsPageDirEntry, h *fileformat.BlockHeader) { h.TableID = 2 })
	mutate(func(_ *fileformat.RowsPageDirEntry, h *fileformat.BlockHeader) { h.KeyEpoch = 1 })
}

// TestPageSealOpenRoundTrip seals and opens a page with matching identity,
// then verifies tampering and cross-context reuse both fail authentication.
func TestPageSealOpenRoundTrip(t *testing.T) {
	c := testCipher(t)
	page := pageDir(0, 0, 32, 16000+fileformat.AESGCMTagLen, 8000, 1, 32)
	ctx := func(uuid *[16]byte, blockID, snap uint64, table uint32, p fileformat.RowsPageDirEntry, epoch uint32) PageContext {
		return PageContext{
			UUID:        uuid,
			BlockID:     blockID,
			SnapshotID:  snap,
			TableID:     table,
			Compression: fileformat.CompressionZstd,
			Page:        p,
			Epoch:       epoch,
		}
	}
	// AAD binds the on-disk (sealed) StoredSize.
	sealed, err := c.SealPage(ctx(&testUUID, 42, 7, 1, page, 0), []byte("compressed page payload"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sealed) != len("compressed page payload")+fileformat.AESGCMTagLen {
		t.Fatalf("sealed %d bytes, want %d", len(sealed), len("compressed page payload")+fileformat.AESGCMTagLen)
	}
	pt, err := c.OpenPage(ctx(&testUUID, 42, 7, 1, page, 0), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if string(pt) != "compressed page payload" {
		t.Fatalf("opened %q", pt)
	}

	// Tampered ciphertext must fail authentication.
	bad := append([]byte(nil), sealed...)
	bad[0] ^= 0x01
	if _, err := c.OpenPage(ctx(&testUUID, 42, 7, 1, page, 0), bad); err == nil {
		t.Fatal("tampered page accepted")
	}

	// Same ciphertext with a different page ordinal / block / epoch must fail.
	page1 := page
	page1.PageOrdinal = 1
	if _, err := c.OpenPage(ctx(&testUUID, 42, 7, 1, page1, 0), sealed); err == nil {
		t.Fatal("page moved to another ordinal authenticated")
	}
	if _, err := c.OpenPage(ctx(&testUUID, 43, 7, 1, page, 0), sealed); err == nil {
		t.Fatal("page moved to another block authenticated")
	}
	if _, err := c.OpenPage(ctx(&testUUID, 42, 7, 1, page, 1), sealed); err == nil {
		t.Fatal("page authenticated under another epoch")
	}
	var wrong [16]byte
	wrong[0] = 0xAA
	if _, err := c.OpenPage(ctx(&wrong, 42, 7, 1, page, 0), sealed); err == nil {
		t.Fatal("page authenticated under another store")
	}
	if _, err := c.OpenPage(ctx(&testUUID, 42, 7, 2, page, 0), sealed); err == nil {
		t.Fatal("page authenticated under another table")
	}
	// Wrong key.
	c2, _ := NewCipher(bytes.Repeat([]byte{0x11}, 32))
	if _, err := c2.OpenPage(ctx(&testUUID, 42, 7, 1, page, 0), sealed); err == nil {
		t.Fatal("page authenticated under another key")
	}
}
