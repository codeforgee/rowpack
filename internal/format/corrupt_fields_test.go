package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// mustMarshalInto marshals via fill into a fresh exact-size buffer, failing
// the test on error. The returned buffer is a valid encoding.
func mustMarshalInto(t *testing.T, size int, fill func(dst []byte) error) []byte {
	t.Helper()
	dst := make([]byte, size)
	if err := fill(dst); err != nil {
		t.Fatalf("marshal into %d bytes: %v", size, err)
	}
	return dst
}

// corrupt returns a copy of src with mutation applied.
func corrupt(src []byte, mut func(b []byte)) []byte {
	cp := append([]byte(nil), src...)
	mut(cp)
	return cp
}

// TestMarshalToRejectsShortDestination exercises the destination-too-short
// guard of every fixed-size MarshalTo. A decoder-side guard would be useless
// without the encoder refusing to under-write.
func TestMarshalToRejectsShortDestination(t *testing.T) {
	cases := []struct {
		name  string
		size  int
		fill  func(dst []byte) error
		isErr func(err error) bool
	}{
		{"DataFileHeader", DataFileHeaderSize, func(d []byte) error {
			return (&DataFileHeader{}).MarshalTo(d)
		}, nil},
		{"SnapshotHeader", SnapshotHeaderSize, func(d []byte) error {
			return (&SnapshotHeader{SnapshotID: 1}).MarshalTo(d)
		}, nil},
		{"SnapshotFooter", SnapshotFooterSize, func(d []byte) error {
			return (&SnapshotFooter{SnapshotID: 1}).MarshalTo(d)
		}, nil},
		{"IndexTxnHeader", IndexTxnHeaderSize, func(d []byte) error {
			return (&IndexTxnHeader{SnapshotID: 1}).MarshalTo(d)
		}, nil},
		{"IndexTxnFooter", IndexTxnFooterSize, func(d []byte) error {
			return (&IndexTxnFooter{SnapshotID: 1}).MarshalTo(d)
		}, nil},
		{"IndexChunkHeader", IndexChunkHeaderSize, func(d []byte) error {
			return (&IndexChunkHeader{EntryKind: IndexChunkKindRow, EntryCount: 1, RawBytes: 1, StoredBytes: 1}).MarshalTo(d)
		}, nil},
		{"IndexChunkDirEntry", IndexChunkDirEntrySize, func(d []byte) error {
			return (&IndexChunkDirEntry{ChunkSequence: 1}).MarshalTo(d)
		}, nil},
		{"SnapshotIndexEntry", SnapshotIndexEntrySize, func(d []byte) error {
			return (&SnapshotIndexEntry{SnapshotID: 1}).MarshalTo(d)
		}, nil},
		{"MetadataIndexEntry", MetadataIndexEntrySize, func(d []byte) error {
			return (&MetadataIndexEntry{SnapshotID: 1, ObjectID: 1}).MarshalTo(d)
		}, nil},
		{"BlockIndexEntry", BlockIndexEntrySize, func(d []byte) error {
			return (&BlockIndexEntry{BlockID: 1, SnapshotID: 1}).MarshalTo(d)
		}, nil},
		{"RowIndexEntry", RowIndexEntrySize, func(d []byte) error {
			return (&RowIndexEntry{SnapshotID: 1, RowID: 1}).MarshalTo(d)
		}, nil},
		{"RowIndexFenceEntry", IndexFenceEntrySize, func(d []byte) error {
			return (&RowIndexFenceEntry{SnapshotID: 1, EntryCount: 1}).MarshalTo(d)
		}, nil},
		{"RowsPageHeader", RowsPageHeaderSize, func(d []byte) error {
			return (&RowsPageHeader{EntryCount: 1, ChangeBitsBytes: 1}).MarshalTo(d)
		}, nil},
		{"RowsPageDirEntry", RowsPageDirEntrySize, func(d []byte) error {
			return (&RowsPageDirEntry{PageOrdinal: 1, RecordCount: 1}).MarshalTo(d)
		}, nil},
		{"RowIndexPageHeader", IndexPageHeaderSize, func(d []byte) error {
			return (&RowIndexPageHeader{EntryCount: 1, ChangeBitsBytes: 1}).MarshalTo(d)
		}, nil},
		{"RowDirectoryEntry", RowDirectoryEntrySize, func(d []byte) error {
			return (&RowDirectoryEntry{RowID: 1}).MarshalTo(d)
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, n := range []int{0, tc.size - 1} {
				err := tc.fill(make([]byte, n))
				require.NotNil(t, err, "%s: accepted %d-byte destination (want >= %d)", tc.name, n, tc.size)
				require.Contains(t, err.Error(), "too short", "%s: unexpected error for %d-byte dst: %v", tc.name, n, err)
			}
			// Exact size must succeed.
			if err := tc.fill(make([]byte, tc.size)); err != nil {
				t.Fatalf("exact size %d rejected: %v", tc.size, err)
			}
		})
	}
}

// TestFileHeaderRejectsCorruptFields covers the FileHeader validation ladder:
// bad magic, wrong major version, wrong size word, payload CRC mismatch and
// the over-long key-id guards on both encode and decode.
func TestFileHeaderRejectsCorruptFields(t *testing.T) {
	valid := func() []byte {
		var h DataFileHeader
		h.DefaultBlockSize = 1 << 16
		return mustMarshalInto(t, DataFileHeaderSize, h.MarshalTo)
	}
	err := (&DataFileHeader{}).Unmarshal(valid())
	require.NoError(t, err, "valid header rejected")

	is := func(src []byte) error {
		var h DataFileHeader
		return h.Unmarshal(src)
	}
	// Bad magic.
	require.Error(t, is(corrupt(valid(), func(b []byte) { copy(b[0:8], "XXXXXXXX") })), "bad magic accepted")
	// Wrong major version (this also drives the error-path version fetch).
	require.Error(t, is(corrupt(valid(), func(b []byte) { b[8] = VersionMajor + 1 })), "wrong major version accepted")
	// Wrong size word.
	require.Error(t, is(corrupt(valid(), func(b []byte) { putU32(b[12:], DataFileHeaderSize+4) })), "wrong size word accepted")
	// Payload CRC mismatch.
	require.Error(t, is(corrupt(valid(), func(b []byte) { b[40] ^= 0xFF })), "CRC mismatch accepted")
	// Over-long key id length byte with a matching CRC.
	badKL := corrupt(valid(), func(b []byte) {
		b[FileHeaderKeyIDLenOffset] = FileHeaderKeyIDMaxLen + 1
		finalizeCRC(b[:DataFileHeaderSize], DataFileHeaderCRC32COffset)
	})
	require.Error(t, is(badKL), "over-long key id length accepted")
	// Marshal must refuse a key id above the maximum before writing anything.
	var h DataFileHeader
	h.KeyID = make([]byte, FileHeaderKeyIDMaxLen+1)
	require.Error(t, h.MarshalTo(make([]byte, DataFileHeaderSize)), "over-long key id accepted on marshal")
}

// TestCRCStructsRejectCorruptFields hits the bad-magic / bad-size / CRC
// validation arms of every CRC-carrying structure at full length (the
// truncation ladder is covered by TestFixedStructsResistTruncation).
func TestCRCStructsRejectCorruptFields(t *testing.T) {
	is := func(src []byte) error { // placeholder overwritten per case
		_ = src
		return nil
	}
	_ = is
	type mut struct {
		name string
		fn   func(b []byte)
	}
	cases := []struct {
		name  string
		build func() []byte
		is    func(src []byte) error
		muts  []mut
	}{
		{"SnapshotHeader", func() []byte {
			return mustMarshalInto(t, SnapshotHeaderSize, (&SnapshotHeader{SnapshotID: 1}).MarshalTo)
		}, func(src []byte) error {
			var h SnapshotHeader
			return h.Unmarshal(src)
		}, []mut{
			{"bad magic", func(b []byte) { copy(b[0:8], "XXXXXXXX") }},
			{"bad size", func(b []byte) { putU32(b[8:], SnapshotHeaderSize+4) }},
			{"crc mismatch", func(b []byte) { b[20] ^= 0xFF }},
		}},
		{"SnapshotFooter", func() []byte {
			return mustMarshalInto(t, SnapshotFooterSize, (&SnapshotFooter{SnapshotID: 1}).MarshalTo)
		}, func(src []byte) error {
			var f SnapshotFooter
			return f.Unmarshal(src)
		}, []mut{
			{"bad magic", func(b []byte) { copy(b[0:8], "XXXXXXXX") }},
			{"bad size", func(b []byte) { putU32(b[8:], SnapshotFooterSize+4) }},
			{"crc mismatch", func(b []byte) { b[24] ^= 0xFF }},
		}},
		{"IndexTxnHeader", func() []byte {
			return mustMarshalInto(t, IndexTxnHeaderSize, (&IndexTxnHeader{SnapshotID: 1, RowEntryCount: 1}).MarshalTo)
		}, func(src []byte) error {
			var h IndexTxnHeader
			return h.Unmarshal(src)
		}, []mut{
			{"bad magic", func(b []byte) { copy(b[0:8], "XXXXXXXX") }},
			{"bad size", func(b []byte) { putU32(b[8:], IndexTxnHeaderSize+4) }},
			{"crc mismatch", func(b []byte) { b[24] ^= 0xFF }},
		}},
		{"IndexTxnFooter", func() []byte {
			return mustMarshalInto(t, IndexTxnFooterSize, (&IndexTxnFooter{SnapshotID: 1}).MarshalTo)
		}, func(src []byte) error {
			var f IndexTxnFooter
			return f.Unmarshal(src)
		}, []mut{
			{"bad magic", func(b []byte) { copy(b[0:8], "XXXXXXXX") }},
			{"bad size", func(b []byte) { putU32(b[8:], IndexTxnFooterSize+4) }},
			{"crc mismatch", func(b []byte) { b[24] ^= 0xFF }},
		}},
		{"IndexChunkHeader", func() []byte {
			return mustMarshalInto(t, IndexChunkHeaderSize, (&IndexChunkHeader{
				EntryKind: IndexChunkKindBlock, EntryCount: 1, RawBytes: 4, StoredBytes: 4,
			}).MarshalTo)
		}, func(src []byte) error {
			var h IndexChunkHeader
			return h.Unmarshal(src)
		}, []mut{
			{"bad magic", func(b []byte) { copy(b[0:8], "XXXXXXXX") }},
			{"bad size", func(b []byte) { putU16(b[8:], IndexChunkHeaderSize+2) }},
			{"crc mismatch", func(b []byte) { b[20] ^= 0xFF }},
			// Unsupported flags: patch the byte, then re-stamp the CRC so the
			// flags check (not the CRC check) is what fires.
			{"unsupported flags", func(b []byte) {
				b[13] = 0x01
				finalizeCRC(b[:IndexChunkHeaderSize], 44)
			}},
		}},
		{"SnapshotIndexEntry", func() []byte {
			return mustMarshalInto(t, SnapshotIndexEntrySize, (&SnapshotIndexEntry{SnapshotID: 1}).MarshalTo)
		}, func(src []byte) error {
			var e SnapshotIndexEntry
			return e.Unmarshal(src)
		}, []mut{{"crc mismatch", func(b []byte) { b[0] ^= 0xFF }}}},
		{"MetadataIndexEntry", func() []byte {
			return mustMarshalInto(t, MetadataIndexEntrySize, (&MetadataIndexEntry{SnapshotID: 1, ObjectID: 1}).MarshalTo)
		}, func(src []byte) error {
			var e MetadataIndexEntry
			return e.Unmarshal(src)
		}, []mut{{"crc mismatch", func(b []byte) { b[0] ^= 0xFF }}}},
		{"BlockIndexEntry", func() []byte {
			return mustMarshalInto(t, BlockIndexEntrySize, (&BlockIndexEntry{BlockID: 1, SnapshotID: 1}).MarshalTo)
		}, func(src []byte) error {
			var e BlockIndexEntry
			return e.Unmarshal(src)
		}, []mut{{"crc mismatch", func(b []byte) { b[0] ^= 0xFF }}}},
		{"RowIndexEntry", func() []byte {
			return mustMarshalInto(t, RowIndexEntrySize, (&RowIndexEntry{SnapshotID: 1, RowID: 1}).MarshalTo)
		}, func(src []byte) error {
			var e RowIndexEntry
			return e.Unmarshal(src)
		}, []mut{{"crc mismatch", func(b []byte) { b[0] ^= 0xFF }}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := tc.build()
			err := tc.is(full)
			require.NoError(t, err, "valid encoding rejected")
			for _, m := range tc.muts {
				if err := tc.is(corrupt(full, m.fn)); err == nil {
					t.Fatalf("%s: corrupt input accepted", m.name)
				}
			}
		})
	}
}

// TestRowsPageHeaderRejectsZeroEntries covers the EntryCount==0 arm; the
// zeroed header must also carry a matching (zero) change-bits size so the
// entry-count check fires before the change-bits geometry check.
func TestRowsPageHeaderRejectsZeroEntries(t *testing.T) {
	h := RowsPageHeader{EntryCount: 0, ChangeBitsBytes: 0}
	buf := mustMarshalInto(t, RowsPageHeaderSize, h.MarshalTo)
	var out RowsPageHeader
	require.Error(t, out.Unmarshal(buf, 0), "zero-entry page header accepted")
}

// TestRowDirectoryEntryResistsTruncation extends the truncation ladder to
// RowDirectoryEntry, which the fixed-structs table does not include.
func TestRowDirectoryEntryResistsTruncation(t *testing.T) {
	full := mustMarshalInto(t, RowDirectoryEntrySize, (&RowDirectoryEntry{
		RowID: 7, RecordOffset: 64, RecordLength: 32, ChangeType: ChangeInsert, SchemaVersion: 1,
	}).MarshalTo)
	var e RowDirectoryEntry
	err := e.Unmarshal(full)
	require.NoError(t, err, "full encoding rejected")
	if e.RowID != 7 || e.RecordOffset != 64 || e.RecordLength != 32 || e.ChangeType != ChangeInsert || e.SchemaVersion != 1 {
		t.Fatalf("roundtrip mismatch: %+v", e)
	}
	for l := range RowDirectoryEntrySize {
		if err := e.Unmarshal(full[:l]); err == nil {
			t.Fatalf("accepted %d/%d bytes", l, RowDirectoryEntrySize)
		}
	}
}

// TestHeaderHelpersShortInput covers the bounds-guarded little-endian
// readers and the header patch/epoch helpers on undersized input.
func TestHeaderHelpersShortInput(t *testing.T) {
	if _, ok := getU32([]byte{0x01, 0x02, 0x03}); ok {
		t.Fatal("getU32 accepted 3 bytes")
	}
	if _, err := verifyCRC(make([]byte, 12), 12); err == nil {
		t.Fatal("verifyCRC accepted a buffer shorter than the CRC field")
	}
	require.EqualValues(t, 0, IndexTxnHeaderKeyEpoch(make([]byte, 8)), "short input must not yield a key epoch")
	require.Error(t, PatchIndexTxnHeaderForStorage(make([]byte, 8), 1, 1), "PatchIndexTxnHeaderForStorage accepted a short header")
	// And on a full header the patch must round-trip.
	hdr := mustMarshalInto(t, IndexTxnHeaderSize, (&IndexTxnHeader{SnapshotID: 1}).MarshalTo)
	err := PatchIndexTxnHeaderForStorage(hdr, 999, 7)
	require.NoError(t, err, "patch")
	var h IndexTxnHeader
	err = h.Unmarshal(hdr)
	require.NoError(t, err, "re-unmarshal")
	require.EqualValues(t, 999, h.BodyBytes, "BodyBytes = %d, want 999", h.BodyBytes)
	require.Equal(t, uint32(7), IndexTxnHeaderKeyEpoch(hdr), "KeyEpoch = %d, want 7", IndexTxnHeaderKeyEpoch(hdr))
}
