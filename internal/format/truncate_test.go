package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// truncationCases lists every fixed-size structure's Unmarshal with a zero
// value destination. For each, every prefix of the serialized form (including
// the empty input) must be rejected: the decoders are the first line of
// defence against truncated or hostile files.
func TestFixedStructsResistTruncation(t *testing.T) {
	mkHeader := func(size int, f func(dst []byte)) []byte {
		dst := make([]byte, size)
		f(dst)
		return dst
	}
	cases := []struct {
		name string
		size int
		fill func(dst []byte)
		is   func(src []byte) error
	}{
		{"DataFileHeader", DataFileHeaderSize, func(d []byte) {
			var h DataFileHeader
			if err := h.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var h DataFileHeader
			return h.Unmarshal(s)
		}},
		{"SnapshotHeader", SnapshotHeaderSize, func(d []byte) {
			var h SnapshotHeader
			if err := h.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var h SnapshotHeader
			return h.Unmarshal(s)
		}},
		{"SnapshotFooter", SnapshotFooterSize, func(d []byte) {
			var f SnapshotFooter
			if err := f.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var f SnapshotFooter
			return f.Unmarshal(s)
		}},
		{"BlockHeader", BlockHeaderSize, func(d []byte) {
			var h BlockHeader
			if err := h.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var h BlockHeader
			return h.Unmarshal(s)
		}},
		{"RowsBlockHeader", RowsBlockHeaderSize, func(d []byte) {
			h := RowsBlockHeader{PageCount: 1, DirectoryBytes: RowsPageDirEntrySize, TotalRecords: 1}
			if err := h.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var h RowsBlockHeader
			return h.Unmarshal(s)
		}},
		{"IndexTxnHeader", IndexTxnHeaderSize, func(d []byte) {
			h := IndexTxnHeader{TxnSequence: 1, SnapshotID: 1, RowEntryCount: 1}
			if err := h.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var h IndexTxnHeader
			return h.Unmarshal(s)
		}},
		{"IndexTxnFooter", IndexTxnFooterSize, func(d []byte) {
			f := IndexTxnFooter{TxnSequence: 1, SnapshotID: 1}
			if err := f.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var f IndexTxnFooter
			return f.Unmarshal(s)
		}},
		{"IndexChunkHeader", IndexChunkHeaderSize, func(d []byte) {
			h := IndexChunkHeader{EntryKind: IndexChunkKindRow, EntryCount: 1, RawBytes: 1, StoredBytes: 1}
			if err := h.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var h IndexChunkHeader
			return h.Unmarshal(s)
		}},
		{"SnapshotIndexEntry", SnapshotIndexEntrySize, func(d []byte) {
			e := SnapshotIndexEntry{SnapshotID: 1, SnapshotType: SnapshotFull}
			if err := e.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var e SnapshotIndexEntry
			return e.Unmarshal(s)
		}},
		{"MetadataIndexEntry", MetadataIndexEntrySize, func(d []byte) {
			e := MetadataIndexEntry{SnapshotID: 1, ObjectID: 1}
			if err := e.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var e MetadataIndexEntry
			return e.Unmarshal(s)
		}},
		{"BlockIndexEntry", BlockIndexEntrySize, func(d []byte) {
			e := BlockIndexEntry{BlockID: 1, SnapshotID: 1}
			if err := e.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var e BlockIndexEntry
			return e.Unmarshal(s)
		}},
		{"RowIndexEntry", RowIndexEntrySize, func(d []byte) {
			e := RowIndexEntry{SnapshotID: 1, TableID: 1, RowID: 1}
			if err := e.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var e RowIndexEntry
			return e.Unmarshal(s)
		}},
		{"RowIndexFenceEntry", IndexFenceEntrySize, func(d []byte) {
			e := RowIndexFenceEntry{SnapshotID: 1, EntryCount: 1}
			if err := e.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var e RowIndexFenceEntry
			return e.Unmarshal(s)
		}},
		{"RowsPageDirEntry", RowsPageDirEntrySize, func(d []byte) {
			e := RowsPageDirEntry{PageOrdinal: 1, RecordCount: 1}
			if err := e.MarshalTo(d); err != nil {
				t.Fatal(err)
			}
		}, func(s []byte) error {
			var e RowsPageDirEntry
			return e.Unmarshal(s)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := mkHeader(tc.size, tc.fill)
			for l := 0; l < tc.size; l++ {
				if err := tc.is(full[:l]); err == nil {
					t.Fatalf("%s: accepted %d/%d bytes", tc.name, l, tc.size)
				}
			}
			// The full encoding must of course parse.
			if err := tc.is(full); err != nil {
				t.Fatalf("%s: full encoding rejected: %v", tc.name, err)
			}
		})
	}
}

// TestRowIndexPageHeaderSemanticRejects covers the non-length validation
// branches: bad magic, unknown version, non-zero reserved bits, zero entry
// count and change-bits mismatch.
func TestRowIndexPageHeaderSemanticRejects(t *testing.T) {
	build := func(entryCount uint32, changeBits uint32) []byte {
		dst := make([]byte, IndexPageHeaderSize)
		h := RowIndexPageHeader{
			EntryCount:      entryCount,
			ChangeBitsBytes: changeBits,
			FirstRowID:      1,
			MinRowID:        1,
			MaxRowID:        10,
		}
		h.CRC32C = CRC32C(dst[:60])
		if err := h.MarshalTo(dst); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	is := func(src []byte) error {
		var h RowIndexPageHeader
		return h.Unmarshal(src, 0) // totalLen 0 skips the streams-sum check
	}
	valid := build(4, 1) // 4 entries -> ceil(4/4) = 1 change-bits byte
	err := is(valid)
	require.NoError(t, err, "valid header rejected")

	badMagic := append([]byte(nil), valid...)
	copy(badMagic[0:8], "XXXXXXXX")
	require.Error(t, is(badMagic), "bad magic should be rejected")

	badVersion := append([]byte(nil), valid...)
	badVersion[8] = IndexPageVersion + 1
	require.Error(t, is(badVersion), "unknown version should be rejected")

	reserved := append([]byte(nil), valid...)
	reserved[9] = 1
	require.Error(t, is(reserved), "non-zero reserved bits should be rejected")

	zero := build(0, 0)
	require.Error(t, is(zero), "zero entry count should be rejected")

	mismatch := build(5, 1) // 5 entries need ceil(5/4) = 2 bytes
	require.Error(t, is(mismatch), "change-bits mismatch should be rejected")
}

// TestIndexChunkHeaderSemanticRejects covers unknown kind/compression/
// encryption enum rejection.
func TestIndexChunkHeaderSemanticRejects(t *testing.T) {
	build := func(kind, comp, enc uint8) []byte {
		dst := make([]byte, IndexChunkHeaderSize)
		h := IndexChunkHeader{
			EntryKind:   kind,
			Compression: comp,
			Encryption:  enc,
			EntryCount:  1,
			RawBytes:    4,
			StoredBytes: 4,
		}
		h.PayloadCRC32C = CRC32C([]byte("abcd"))
		if err := h.MarshalTo(dst); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	is := func(src []byte) error {
		var h IndexChunkHeader
		return h.Unmarshal(src)
	}
	err := is(build(IndexChunkKindRow, IndexChunkCompressionNone, IndexChunkEncryptionNone))
	require.NoError(t, err, "valid header rejected")
	require.Error(t, is(build(99, IndexChunkCompressionNone, IndexChunkEncryptionNone)), "unknown entry kind should be rejected")
	require.Error(t, is(build(IndexChunkKindRow, 99, IndexChunkEncryptionNone)), "unknown compression should be rejected")
	require.Error(t, is(build(IndexChunkKindRow, IndexChunkCompressionNone, 99)), "unknown encryption should be rejected")
}

// TestBlockHeaderMarshalsWithEnums confirms the block header accepts both
// block kinds and both compressions (enum validation lives in the reader's
// checkHeader, not in Unmarshal).
func TestBlockHeaderMarshalsWithEnums(t *testing.T) {
	build := func(kind BlockKind, comp Compression) []byte {
		dst := make([]byte, BlockHeaderSize)
		h := BlockHeader{
			BlockKind:   kind,
			Compression: comp,
			BlockID:     1,
			SnapshotID:  1,
			ItemCount:   1,
			RawSize:     4,
			StoredSize:  4,
			RawCRC32C:   CRC32C([]byte("abcd")),
		}
		if err := h.MarshalTo(dst); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	is := func(src []byte, want BlockKind, wantComp Compression) BlockHeader {
		var h BlockHeader
		if err := h.Unmarshal(src); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if h.BlockKind != want || h.Compression != wantComp {
			t.Fatalf("kind/comp = %d/%d, want %d/%d", h.BlockKind, h.Compression, want, wantComp)
		}
		return h
	}
	for _, c := range []struct {
		kind BlockKind
		comp Compression
	}{
		{BlockKindRows, CompressionNone},
		{BlockKindMetadata, CompressionZstd},
	} {
		h := is(build(c.kind, c.comp), c.kind, c.comp)
		if h.BlockKind != c.kind || h.Compression != c.comp {
			t.Fatalf("roundtrip mismatch: %+v", h)
		}
	}
}
