package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatErrorMessage(t *testing.T) {
	e := formatError("RowsBlockHeader", 12, "bad %s", "geometry")
	if got := e.Error(); got != "RowsBlockHeader at offset 12: bad geometry" {
		t.Fatalf("Error() = %q", got)
	}
	e = formatError("SnapshotFooter", -1, "no offset")
	if got := e.Error(); got != "SnapshotFooter: no offset" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestIndexChunkDirEntryRoundtrip(t *testing.T) {
	e := IndexChunkDirEntry{
		ChunkSequence:     3,
		EntryCount:        40,
		FirstEntryOrdinal: 80,
		RawBytes:          1000,
		StoredBytes:       600,
		EntryKind:         uint8(IndexChunkKindRow),
		RegionOffset:      4096,
	}
	if e.Size() != IndexChunkDirEntrySize {
		t.Fatalf("Size = %d, want %d", e.Size(), IndexChunkDirEntrySize)
	}
	dst := make([]byte, IndexChunkDirEntrySize)
	err := e.MarshalTo(dst)
	require.NoError(t, err, "MarshalTo")
	require.Error(t, e.MarshalTo(make([]byte, IndexChunkDirEntrySize-1)), "short destination should error")

	var got IndexChunkDirEntry
	err = got.Unmarshal(dst)
	require.NoError(t, err, "Unmarshal")
	require.Equal(t, e, got, "roundtrip mismatch: %+v vs %+v", got, e)
	require.Error(t, got.Unmarshal(dst[:IndexChunkDirEntrySize-1]), "truncated input should error")
}

func TestParseIndexChunkDirectory(t *testing.T) {
	entries := []IndexChunkDirEntry{
		{ChunkSequence: 0, EntryCount: 10, RawBytes: 100, StoredBytes: 60, EntryKind: uint8(IndexChunkKindRow)},
		{ChunkSequence: 1, EntryCount: 10, RawBytes: 100, StoredBytes: 76, EntryKind: uint8(IndexChunkKindSnapshot)},
	}
	dir := make([]byte, 2*IndexChunkDirEntrySize)
	for i := range entries {
		if err := entries[i].MarshalTo(dir[i*IndexChunkDirEntrySize:]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ParseIndexChunkDirectory(dir)
	require.NoError(t, err, "ParseIndexChunkDirectory")
	if len(got) != 2 || got[1].ChunkSequence != 1 || got[0].EntryCount != 10 {
		t.Fatalf("parsed directory: %+v", got)
	}

	if _, err := ParseIndexChunkDirectory(nil); err == nil {
		t.Fatal("empty directory should error")
	}
	if _, err := ParseIndexChunkDirectory([]byte{1, 2, 3}); err == nil {
		t.Fatal("non-multiple length should error")
	}
}

func TestIndexChunkHeaderCheckLimits(t *testing.T) {
	valid := IndexChunkHeader{
		EntryKind:   uint8(IndexChunkKindRow),
		Compression: IndexChunkCompressionZstd,
		Encryption:  IndexChunkEncryptionNone,
		EntryCount:  10,
		RawBytes:    100,
		StoredBytes: 60,
	}
	err := valid.CheckLimits()
	require.NoError(t, err, "valid header")

	zero := valid
	zero.EntryCount = 0
	require.Error(t, zero.CheckLimits(), "zero entry count should fail")
	huge := valid
	huge.EntryCount = IndexChunkMaxEntries + 1
	require.Error(t, huge.CheckLimits(), "oversized entry count should fail")
	zeroRaw := valid
	zeroRaw.RawBytes = 0
	require.Error(t, zeroRaw.CheckLimits(), "zero raw bytes should fail")
	hugeRaw := valid
	hugeRaw.RawBytes = IndexChunkMaxRawBytes + 1
	require.Error(t, hugeRaw.CheckLimits(), "oversized raw bytes should fail")
	zeroStored := valid
	zeroStored.StoredBytes = 0
	require.Error(t, zeroStored.CheckLimits(), "zero stored bytes should fail")
	hugeStored := valid
	hugeStored.StoredBytes = IndexChunkMaxStoredBytes + 1
	require.Error(t, hugeStored.CheckLimits(), "oversized stored bytes should fail")

	// Encrypted chunk smaller than the GCM tag.
	short := valid
	short.Encryption = IndexChunkEncryptionAESGCM
	short.StoredBytes = AESGCMTagLen - 1
	require.Error(t, short.CheckLimits(), "encrypted stored < tag should fail")

	// Uncompressed chunk must have stored == raw (+tag when encrypted).
	plain := valid
	plain.Compression = IndexChunkCompressionNone
	plain.StoredBytes = plain.RawBytes + 1
	require.Error(t, plain.CheckLimits(), "none-compression stored != raw should fail")
	plain.StoredBytes = plain.RawBytes
	err = plain.CheckLimits()
	require.NoError(t, err, "none-compression stored == raw")
	enc := valid
	enc.Compression = IndexChunkCompressionNone
	enc.Encryption = IndexChunkEncryptionAESGCM
	enc.StoredBytes = enc.RawBytes + AESGCMTagLen
	err = enc.CheckLimits()
	require.NoError(t, err, "encrypted none-compression stored == raw + tag")
}

func TestFrozenStructSizes(t *testing.T) {
	if (&IndexChunkHeader{}).Size() != IndexChunkHeaderSize {
		t.Fatal("IndexChunkHeader.Size mismatch")
	}
	var h RowIndexPageHeader
	if h.Size() != IndexPageHeaderSize {
		t.Fatal("RowIndexPageHeader.Size mismatch")
	}
	var f RowIndexFenceEntry
	if f.Size() != IndexFenceEntrySize {
		t.Fatal("RowIndexFenceEntry.Size mismatch")
	}
	// RowsPageDirEntry is varint-encoded and has no fixed size; see
	// TestRowsPageDirEntryRoundtripAndRejects.
	var rb RowsBlockHeader
	if rb.Size() != RowsBlockHeaderSize {
		t.Fatal("RowsBlockHeader.Size mismatch")
	}
}

func TestSnapshotFooterOffsetsAreConsistent(t *testing.T) {
	valid := SnapshotFooter{
		SnapshotStartOffset: 100,
		BlocksStartOffset:   100 + SnapshotHeaderSize,
		BlocksEndOffset:     1000,
		IndexTxnStartOffset: 1000,
		IndexTxnEndOffset:   2000,
		SnapshotEndOffset:   2000 + SnapshotFooterSize,
	}
	if !valid.OffsetsAreConsistent() {
		t.Fatal("valid geometry rejected")
	}

	breakIt := func(mutate func(*SnapshotFooter) bool, name string) {
		t.Helper()
		f := valid
		broke := mutate(&f)
		if !broke && f.OffsetsAreConsistent() {
			t.Fatalf("%s: expected geometry to break", name)
		}
		if broke && f.OffsetsAreConsistent() {
			t.Fatalf("%s: broken geometry accepted", name)
		}
	}
	_ = breakIt

	check := func(name string, mutate func(*SnapshotFooter)) {
		t.Helper()
		f := valid
		mutate(&f)
		if f.OffsetsAreConsistent() {
			t.Fatalf("%s: broken geometry accepted", name)
		}
	}
	check("zero start", func(f *SnapshotFooter) { f.SnapshotStartOffset = 0 })
	check("blocks start drift", func(f *SnapshotFooter) { f.BlocksStartOffset-- })
	check("blocks end < start", func(f *SnapshotFooter) {
		f.BlocksEndOffset = f.BlocksStartOffset - 1
	})
	check("blocks end > txn start", func(f *SnapshotFooter) {
		f.BlocksEndOffset = f.IndexTxnStartOffset + 1
	})
	check("txn start > end", func(f *SnapshotFooter) {
		f.IndexTxnStartOffset = f.IndexTxnEndOffset + 1
	})
	check("snapshot end <= txn end", func(f *SnapshotFooter) {
		f.SnapshotEndOffset = f.IndexTxnEndOffset
	})
	check("snapshot end drift", func(f *SnapshotFooter) {
		f.SnapshotEndOffset += SnapshotFooterSize
	})

	// Empty snapshot: blocks region degenerates to the header boundary.
	empty := valid
	empty.BlocksEndOffset = empty.BlocksStartOffset
	empty.IndexTxnStartOffset = empty.BlocksStartOffset
	if !empty.OffsetsAreConsistent() {
		t.Fatal("empty-snapshot geometry rejected")
	}
}

// TestRowsBlockHeaderDirectoryBytesBounds pins the bounds the header puts on
// DirectoryBytes. Entries are varint-encoded, so the length is no longer
// derivable from PageCount — only the range is, and that range is what stops a
// hostile header from asking for a huge allocation.
func TestRowsBlockHeaderDirectoryBytesBounds(t *testing.T) {
	n, ok := maxDirectoryBytes(3)
	require.True(t, ok, "three pages must have a representable bound")
	require.Equal(t, uint32(3*MaxRowsPageDirEntrySize), n, "maxDirectoryBytes(3)")
	// A directory of N pages cannot be shorter than N one-byte-per-field
	// entries, and the header rejects anything below that.
	inRange := RowsBlockHeader{PageCount: 3, DirectoryBytes: 3 * MaxRowsPageDirEntrySize}
	require.NoError(t, inRange.MarshalTo(make([]byte, RowsBlockHeaderSize)), "upper bound accepted")
	tooShort := RowsBlockHeader{PageCount: 3, DirectoryBytes: 3*MinRowsPageDirEntrySize - 1}
	require.ErrorContains(t, tooShort.MarshalTo(make([]byte, RowsBlockHeaderSize)), "outside", "below the lower bound")
	tooLong := RowsBlockHeader{PageCount: 3, DirectoryBytes: 3*MaxRowsPageDirEntrySize + 1}
	require.ErrorContains(t, tooLong.MarshalTo(make([]byte, RowsBlockHeaderSize)), "outside", "above the upper bound")
	// Overflow guard: page count whose directory bound exceeds uint32.
	if _, ok := maxDirectoryBytes(1 << 31); ok {
		t.Fatal("overflowing page count should be rejected")
	}
}
