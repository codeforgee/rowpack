package fileformat

import "testing"

// mustMarshal2 marshals a fixed structure into a fresh buffer, panicking on
// programmer error. It is used by fuzz seeds where no *testing.T exists.
func mustMarshal2(m interface{ MarshalTo([]byte) error }) []byte {
	buf := make([]byte, 1024)
	if err := m.MarshalTo(buf); err != nil {
		panic(err)
	}
	return buf
}

// Fuzz decoders must never panic on arbitrary input; they must return a
// FormatError instead.

func fuzzSeed(f *testing.F, valid []byte) {
	f.Add([]byte(nil))
	f.Add(valid)
	f.Add(valid[:len(valid)/2])
	f.Add([]byte{0xFF, 0xFE, 0xFD, 0xFC})
}

func validDataHeader() []byte {
	var h DataFileHeader
	h.StoreUUID = [16]byte{1, 2, 3}
	h.CreatedUnixNano = 1700000000000000000
	h.RequiredFeatures = RequiredFeaturesV1
	h.DefaultBlockSize = 256 << 10
	h.DefaultCompression = CompressionZstd
	h.DefaultRowEncoding = RowEncodingTypedTuple
	return mustMarshal2(&h)
}

func validIndexHeader() []byte {
	var h IndexFileHeader
	h.StoreUUID = [16]byte{9, 8, 7}
	h.CreatedUnixNano = 1700000000000000000
	h.RequiredFeatures = RequiredFeaturesV1
	h.DefaultBlockSize = 256 << 10
	h.DefaultCompression = CompressionZstd
	h.DefaultRowEncoding = RowEncodingTypedTuple
	return mustMarshal2(&h)
}

func FuzzDataFileHeader(f *testing.F) {
	fuzzSeed(f, validDataHeader())
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&DataFileHeader{}).Unmarshal(data)
	})
}

func FuzzIndexFileHeader(f *testing.F) {
	fuzzSeed(f, validIndexHeader())
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&IndexFileHeader{}).Unmarshal(data)
	})
}

func FuzzSnapshotHeader(f *testing.F) {
	h := &SnapshotHeader{SnapshotType: SnapshotFull, SnapshotID: 42,
		CreatedUnixNano: 1700000000000000000, FirstBlockID: 1}
	fuzzSeed(f, mustMarshal2(h))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&SnapshotHeader{}).Unmarshal(data)
	})
}

func FuzzSnapshotFooter(f *testing.F) {
	ft := &SnapshotFooter{SnapshotType: SnapshotFull, SnapshotID: 42,
		SnapshotStartOffset: 128, SnapshotEndOffset: 4096, BlockCount: 4, RowRecordCount: 10}
	fuzzSeed(f, mustMarshal2(ft))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&SnapshotFooter{}).Unmarshal(data)
	})
}

func FuzzBlockHeader(f *testing.F) {
	h := &BlockHeader{BlockKind: BlockKindRows, Compression: CompressionZstd,
		BlockID: 7, SnapshotID: 42, TableID: 3, ItemCount: 10, RawSize: 100, StoredSize: 50, RawCRC32C: 1}
	fuzzSeed(f, mustMarshal2(h))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&BlockHeader{}).Unmarshal(data)
	})
}

func FuzzIndexTxnHeader(f *testing.F) {
	h := &IndexTxnHeader{TxnSequence: 1, SnapshotID: 42, DataSnapshotStart: 128, DataSnapshotEnd: 4096}
	fuzzSeed(f, mustMarshal2(h))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&IndexTxnHeader{}).Unmarshal(data)
	})
}

func FuzzIndexTxnFooter(f *testing.F) {
	ft := &IndexTxnFooter{TxnSequence: 1, SnapshotID: 42, TxnStartOffset: 128,
		TxnEndOffset: 4096, DataSnapshotEnd: 4096, BodyCRC32C: 1, DataFooterCRC32C: 2}
	fuzzSeed(f, mustMarshal2(ft))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&IndexTxnFooter{}).Unmarshal(data)
	})
}

func FuzzSnapshotIndexEntry(f *testing.F) {
	e := &SnapshotIndexEntry{SnapshotID: 1, SnapshotType: SnapshotFull, BlockCount: 1}
	fuzzSeed(f, mustMarshal2(e))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&SnapshotIndexEntry{}).Unmarshal(data)
	})
}

func FuzzMetadataIndexEntry(f *testing.F) {
	e := &MetadataIndexEntry{SnapshotID: 1, ObjectID: 2, RecordType: 3, BlockID: 4}
	fuzzSeed(f, mustMarshal2(e))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&MetadataIndexEntry{}).Unmarshal(data)
	})
}

func FuzzBlockIndexEntry(f *testing.F) {
	e := &BlockIndexEntry{BlockID: 1, SnapshotID: 2, TableID: 3, DataOffset: 4}
	fuzzSeed(f, mustMarshal2(e))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&BlockIndexEntry{}).Unmarshal(data)
	})
}

func FuzzRowIndexEntry(f *testing.F) {
	e := &RowIndexEntry{SnapshotID: 1, TableID: 2, RowID: 3, BlockID: 4}
	fuzzSeed(f, mustMarshal2(e))
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&RowIndexEntry{}).Unmarshal(data)
	})
}
