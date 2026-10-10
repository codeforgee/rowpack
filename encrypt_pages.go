package rowpack

import (
	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/seal"
)

// pageSealer carries the store's encryption context across the pages of a Rows
// block container: the cipher, the store UUID, and the container limits.
type pageSealer struct {
	cipher *seal.Cipher
	uuid   *[16]byte
	limits block.Limits
}

// seal re-encrypts a Rows page container for an encrypted
// store: instead of sealing the whole container as one blob, each stored page
// is sealed independently (BINARY_FORMAT_V1 §5.1). It parses the container's
// page directory, seals each page's stored (compressed) bytes with a
// domain-separated nonce/AAD, updates the directory StoredSize to the sealed
// length (compressed + AESGCMTagLen), and recomputes the container
// header/directory CRC on the returned payload.
//
// Only the payload and the block header's StoredSize/RawCRC32C change; RawSize
// keeps describing the sum of page raw sizes, the per-page CRC still covers the
// uncompressed page, and the page directory stays plaintext so a reader can
// locate a page without decrypting the whole container.
func (s *pageSealer) seal(h *format.BlockHeader, container []byte) ([]byte, error) {
	// The incoming container is the unsealed builder output: pages are
	// compressed (not sealed), so it must be parsed as a plain container
	// (Encrypted=false). The caller marks h.Encrypted true for the final
	// sealed block; ParseContainer's StoredSize interpretation differs
	// (whole-seal subtracts the tag, per-page does not).
	inputH := *h
	inputH.Encrypted = false
	rc, err := block.ParseContainer(container, inputH, s.limits)
	if err != nil {
		return nil, err
	}
	n := len(rc.Dir)
	sealedPages := make([][]byte, n)
	for i := range n {
		d := &rc.Dir[i]
		src := container[int(d.StoredOffset) : int(d.StoredOffset)+int(d.StoredSize)]
		// The AAD binds the on-disk (sealed) StoredSize, so set it before sealing.
		sealedD := *d
		sealedD.StoredSize = d.StoredSize + format.AESGCMTagLen
		sealed, err := s.cipher.SealPage(seal.PageContext{
			UUID:        s.uuid,
			BlockID:     h.BlockID,
			SnapshotID:  h.SnapshotID,
			TableID:     h.TableID,
			Compression: h.Compression,
			Page:        sealedD,
			Epoch:       h.KeyEpoch,
		}, src)
		if err != nil {
			return nil, err
		}
		sealedPages[i] = sealed
	}
	// Recompute the page directory with sealed sizes and re-run the stored
	// offsets: each sealed page is 16 bytes larger, so the offsets shift.
	// Entries are varint-encoded, so the new directory length is measured from
	// the entries instead of derived from the page count.
	dir := make([]format.RowsPageDirEntry, n)
	dirBytes := 0
	for i := range n {
		d := rc.Dir[i]
		d.StoredSize = uint32(len(sealedPages[i]))
		dir[i] = d
		dirBytes += d.EncodedLen()
	}
	rc.Header.DirectoryBytes = uint32(dirBytes)
	dataStart := format.RowsBlockHeaderSize + dirBytes
	total := dataStart
	for _, sp := range sealedPages {
		total += len(sp)
	}
	newContainer := make([]byte, 0, total)
	var hdr [format.RowsBlockHeaderSize]byte
	_ = rc.Header.MarshalTo(hdr[:])
	newContainer = append(newContainer, hdr[:]...)
	off := dataStart
	for i := range dir {
		dir[i].StoredOffset = uint64(off)
		off += int(dir[i].StoredSize)
		newContainer = dir[i].AppendTo(newContainer)
	}
	for _, sp := range sealedPages {
		newContainer = append(newContainer, sp...)
	}
	h.RawCRC32C = format.CRC32C(newContainer[:dataStart])
	h.StoredSize = uint32(len(newContainer))
	return newContainer, nil
}
