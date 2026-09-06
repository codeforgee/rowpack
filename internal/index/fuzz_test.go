package index

import (
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

func FuzzParseTxn(f *testing.F) {
	good := buildTxnFuzz(1, fullSnap(1, 128, 4096), nil, nil, []fileformat.RowIndexEntry{
		{SnapshotID: 1, TableID: 1, ChangeType: fileformat.ChangeInsert, RowID: 1, BlockID: 1, ItemOrdinal: 0},
	}, 4096, 0)
	f.Add([]byte(nil))
	f.Add(good)
	f.Add(good[:len(good)/2])
	f.Add([]byte{0xFF, 0xFE, 0xFD, 0xFC})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseTxn(data)
	})
}

func buildTxnFuzz(seq uint64, snap fileformat.SnapshotIndexEntry, meta []fileformat.MetadataIndexEntry, blocks []fileformat.BlockIndexEntry, rows []fileformat.RowIndexEntry, dataEnd uint64, footerCRC uint32) []byte {
	b := NewBuilder(seq)
	_ = b.SetSnapshot(snap)
	for _, e := range meta {
		_ = b.AddMetadata(e)
	}
	for _, e := range blocks {
		_ = b.AddBlock(e)
	}
	for _, e := range rows {
		_ = b.AddRow(e)
	}
	out, err := b.Build(snap.DataStart, dataEnd, footerCRC)
	if err != nil {
		panic(err)
	}
	return out
}
