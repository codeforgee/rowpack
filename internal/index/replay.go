package index

import (
	"errors"
	"fmt"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// ErrDataFooterMismatch is returned by a DataFooterReader when an index
// transaction references data that is missing, truncated or corrupt. Replay
// treats it as an invalid index tail and stops, restoring to the last
// consistent commit point.
var ErrDataFooterMismatch = errors.New("rowpack: index txn references invalid data footer")

// DataFooterReader supplies the data file SnapshotFooter CRC for the
// cross-file verification of each index transaction. Implemented by the store
// layer (M5/M8); may be nil to skip the cross-check.
type DataFooterReader interface {
	// DataFooterCRC returns the SnapshotFooter FooterCRC32C of the snapshot
	// located at dataStart, or an error if it cannot be read/validated.
	DataFooterCRC(snapshotID uint64, dataStart, dataEnd uint64) (uint32, error)
}

// ReplayResult reports how much of the index was successfully replayed.
type ReplayResult struct {
	Txns        int
	LastOffset  int64 // offset just after the last fully valid txn
	LastSeq     uint64
	TailIgnored int64 // bytes at the tail that were not part of a valid txn
	View        *View
}

// Replay sequentially replays index transactions from the .rpi content after
// its 128-byte header. Transactions are only accepted when header, body,
// footer, all entry CRCs and (when verifier is non-nil) the data footer CRC
// are valid; an invalid or truncated tail stops the replay and is reported,
// never partially applied. A txn whose snapshot duplicates an existing one
// terminates replay (duplicate commit).
func Replay(indexData []byte, startOffset int64, maxDepth uint32, verifier DataFooterReader) (*ReplayResult, error) {
	view := EmptyView()
	res := &ReplayResult{View: view}
	pos := int(startOffset)
	var lastSeq uint64
	for pos < len(indexData) {
		remaining := indexData[pos:]
		if len(remaining) < fileformat.IndexTxnHeaderSize+fileformat.IndexTxnFooterSize {
			// Incomplete tail (truncated write).
			res.TailIgnored = int64(len(remaining))
			break
		}
		txnBytes, txnLen, err := peekTxn(remaining)
		if err != nil {
			// Tail is not a valid txn; stop replay here.
			res.TailIgnored = int64(len(remaining))
			break
		}
		txn, err := ParseTxn(txnBytes)
		if err != nil {
			res.TailIgnored = int64(len(remaining))
			break
		}
		if txn.Header.TxnSequence <= lastSeq {
			res.TailIgnored = int64(len(remaining))
			break
		}
		lastSeq = txn.Header.TxnSequence
		// Data footer cross-check.
		if verifier != nil {
			crc, err := verifier.DataFooterCRC(txn.Snapshot.SnapshotID, txn.Snapshot.DataStart, txn.Snapshot.DataEnd)
			if err != nil {
				if errors.Is(err, ErrDataFooterMismatch) {
					res.TailIgnored = int64(len(remaining))
					break
				}
				return nil, fmt.Errorf("rowpack: replay snapshot %d: %w", txn.Snapshot.SnapshotID, err)
			}
			if crc != txn.Footer.DataFooterCRC32C {
				res.TailIgnored = int64(len(remaining))
				break
			}
		}
		nv, err := view.Apply(txn, maxDepth)
		if err != nil {
			// Duplicate snapshot or inconsistent entries: the index tail is
			// considered invalid from here (could be a partially applied
			// commit); stop replay.
			res.TailIgnored = int64(len(remaining))
			break
		}
		view = nv
		res.Txns++
		res.LastSeq = txn.Header.TxnSequence
		pos += txnLen
		res.LastOffset = int64(pos)
	}
	res.View = view
	return res, nil
}

// peekTxn reports the slice and total length of the txn starting at the head
// of data (without validating contents).
func peekTxn(data []byte) ([]byte, int, error) {
	if len(data) < fileformat.IndexTxnHeaderSize {
		return nil, 0, errors.New("rowpack: short txn header")
	}
	var h fileformat.IndexTxnHeader
	if err := h.Unmarshal(data); err != nil {
		return nil, 0, err
	}
	total := fileformat.IndexTxnHeaderSize + int(h.BodyBytes) + fileformat.IndexTxnFooterSize
	if total > len(data) {
		return nil, 0, errors.New("rowpack: truncated txn")
	}
	if total > 1<<31 {
		return nil, 0, errors.New("rowpack: txn too large")
	}
	return data[:total], total, nil
}
