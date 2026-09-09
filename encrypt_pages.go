package rowpack

import (
	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/seal"
)

// sealRowContainerPages re-encrypts a Rows page container for an encrypted
// store: instead of sealing the whole container as one blob, each stored page
// is sealed independently (BINARY_FORMAT_V2 §5.1). It parses the container's
// page directory, seals each page's stored (compressed) bytes with a
// domain-separated nonce/AAD, updates the directory StoredSize to the sealed
// length (compressed + AESGCMTagLen), and recomputes the container
// header/directory CRC on the returned payload.
//
// Only the payload and the block header's StoredSize/RawCRC32C change; RawSize
// keeps describing the sum of page raw sizes, the per-page CRC still covers the
// uncompressed page, and the page directory stays plaintext so a reader can
// locate a page without decrypting the whole container.
func sealRowContainerPages(h *fileformat.BlockHeader, container []byte, c *seal.Cipher, uuid *[16]byte, limits block.Limits) ([]byte, error) {
	// The incoming container is the unsealed builder output: pages are
	// compressed (not sealed), so it must be parsed as a plain container
	// (Encrypted=false). The caller marks h.Encrypted true for the final
	// sealed block; ParseRowsContainer's StoredSize interpretation differs
	// (whole-seal subtracts the tag, per-page does not).
	inputH := *h
	inputH.Encrypted = false
	rc, err := block.ParseRowsContainer(container, inputH, limits)
	if err != nil {
		return nil, err
	}
	n := len(rc.Dir)
	sealedPages := make([][]byte, n)
	for i := 0; i < n; i++ {
		d := &rc.Dir[i]
		src := container[int(d.StoredOffset):int(d.StoredOffset)+int(d.StoredSize)]
		// The AAD binds the on-disk (sealed) StoredSize, so set it before sealing.
		sealedD := *d
		sealedD.StoredSize = d.StoredSize + fileformat.AESGCMTagLen
		sealed, err := c.SealPage(uuid, h.BlockID, h.SnapshotID, h.TableID, h.Compression, sealedD, h.KeyEpoch, src)
		if err != nil {
			return nil, err
		}
		sealedPages[i] = sealed
	}
	// Recompute the page directory with sealed sizes and re-run the stored
	// offsets: each sealed page is 16 bytes larger, so the offsets shift.
	dataStart := fileformat.RowsBlockHeaderSize + n*fileformat.RowsPageDirEntrySize
	total := dataStart
	for _, sp := range sealedPages {
		total += len(sp)
	}
	newContainer := make([]byte, 0, total)
	var hdr [fileformat.RowsBlockHeaderSize]byte
	_ = rc.Header.MarshalTo(hdr[:])
	newContainer = append(newContainer, hdr[:]...)
	off := dataStart
	for i := 0; i < n; i++ {
		d := rc.Dir[i]
		d.StoredOffset = uint64(off)
		d.StoredSize = uint32(len(sealedPages[i]))
		off += len(sealedPages[i])
		var e [fileformat.RowsPageDirEntrySize]byte
		_ = d.MarshalTo(e[:])
		newContainer = append(newContainer, e[:]...)
	}
	for _, sp := range sealedPages {
		newContainer = append(newContainer, sp...)
	}
	h.RawCRC32C = fileformat.CRC32C(newContainer[:dataStart])
	h.StoredSize = uint32(len(newContainer))
	return newContainer, nil
}
