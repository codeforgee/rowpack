package index

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
)

// chunk_arms_test.go 覆盖 chunked body 的构建与解析两端:被拒绝的 Seal、叛逆的快照
// 封片大小、截断的 chunk 头、以及 sink 自己的失败都必须成为错误而不是被静默吞掉。

// refusingSink is a TxnSink that refuses whatever arm its owner targets.
type refusingSink struct{ err error }

func (s *refusingSink) SetSnapshot(format.SnapshotIndexEntry) error { return s.err }
func (s *refusingSink) AddMetadata(format.MetadataIndexEntry) error { return nil }
func (s *refusingSink) AddBlock(format.BlockIndexEntry) error       { return nil }
func (s *refusingSink) AddRows([]format.RowIndexEntry) error        { return nil }

func armMetaEntry(objectID uint64) format.MetadataIndexEntry {
	return format.MetadataIndexEntry{
		SnapshotID: 1, ObjectID: objectID, Revision: 1,
		RecordType: 1, BlockID: 2, Operation: format.OperationUpsert,
	}
}

// armBody builds one stored txn body (snapshot + one row index page) under the
// given crypto; nil crypto yields a plaintext body.
func armBody(tb testing.TB, crypto *ChunkCrypto) []byte {
	tb.Helper()
	body, _, _, err := testBuilder(tb, 1).BuildStoredBody(crypto, 3, nil)
	require.NoError(tb, err, "the fixture body must build")
	return body
}

func TestBuildStoredBodyRequiresSnapshot(t *testing.T) {
	_, _, _, err := NewBuilder(1).BuildStoredBody(nil, 0, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no snapshot entry")
}

func TestBuildStoredBodyPropagatesSealFailure(t *testing.T) {
	sentinel := errors.New("keyring refused the chunk")
	crypto := &ChunkCrypto{Seal: func(uint32, uint8, uint32, int, []byte) ([]byte, error) {
		return nil, sentinel
	}}

	t.Run("tail chunk", func(t *testing.T) {
		b := testBuilder(t, 1)
		for i := 0; i < 4; i++ {
			require.NoError(t, b.AddMetadata(armMetaEntry(uint64(i)+1)))
		}
		_, _, _, err := b.BuildStoredBody(crypto, 3, nil)
		require.ErrorIs(t, err, sentinel)
	})

	t.Run("chunk cut inside the stream", func(t *testing.T) {
		// Exactly one target worth of entries: the cut fires mid-stream, so the
		// refusal surfaces from the cut rather than from the tail add.
		b := testBuilder(t, 1)
		for i := 0; i < format.IndexChunkTargetEntries; i++ {
			require.NoError(t, b.AddMetadata(armMetaEntry(uint64(i)+1)))
		}
		_, _, _, err := b.BuildStoredBody(crypto, 3, nil)
		require.ErrorIs(t, err, sentinel)
	})
}

// TestBuildStoredBodyEndsOnAChunkCut: a stream landing exactly on the cut
// threshold must not leave an empty trailing chunk behind.
func TestBuildStoredBodyEndsOnAChunkCut(t *testing.T) {
	b := testBuilder(t, 1)
	for i := 0; i < format.IndexChunkTargetEntries; i++ {
		require.NoError(t, b.AddMetadata(armMetaEntry(uint64(i)+1)))
	}
	sb, err := (&bodyParser{region: armBodyOfBuilder(t, b), snapshotID: 1}).parse()
	require.NoError(t, err)
	require.EqualValues(t, 2, sb.chunkCount, "snapshot plus exactly one metadata chunk")
	require.True(t, sb.hasSnap)
}

func armBodyOfBuilder(tb testing.TB, b *Builder) []byte {
	tb.Helper()
	body, _, _, err := b.BuildStoredBody(nil, 0, nil)
	require.NoError(tb, err)
	return body
}

// TestBuildStoredBodyRejectsFixedSnapshotSize: the snapshot chunk occupies a
// fixed 72-byte payload (+16B tag when sealed), so a seal returning any other
// size is a bug rather than a negotiated length.
func TestBuildStoredBodyRejectsFixedSnapshotSize(t *testing.T) {
	crypto := &ChunkCrypto{
		Seal: func(seq uint32, kind uint8, firstOrdinal uint32, rawBytes int, stored []byte) ([]byte, error) {
			if kind == format.IndexChunkKindSnapshot {
				return append(append([]byte(nil), stored...), make([]byte, 4)...), nil
			}
			return stored, nil
		},
	}
	_, _, _, err := testBuilder(t, 1).BuildStoredBody(crypto, 3, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "snapshot chunk stored")
}

// TestBuildStoredBodyPropagatesPageFailure: row index pages are built after the
// streamed chunks, so their failure still has to reach the caller.
func TestBuildStoredBodyPropagatesPageFailure(t *testing.T) {
	b := testBuilder(t, 1)
	e := riEntry(1, 2, 2, 3, format.ChangeType(9))
	e.SnapshotID = 1
	b.rows = append(b.rows, e)
	_, _, _, err := b.BuildStoredBody(nil, 3, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "change type 9 not packable")
}

func TestBufferedSinkRejectsDuplicateSnapshot(t *testing.T) {
	err := bufferedSink{sb: &storedBody{hasSnap: true}}.SetSnapshot(format.SnapshotIndexEntry{SnapshotID: 1})
	require.Error(t, err)
	require.Contains(t, err.Error(), "duplicate snapshot")
}

// TestParseBodyRejectsTruncatedChunkHeader: the magic prefix still matches, so
// only the length guard can catch a body cut inside a chunk header.
func TestParseBodyRejectsTruncatedChunkHeader(t *testing.T) {
	body := armBody(t, nil)
	_, err := (&bodyParser{region: body[:format.IndexChunkHeaderSize-1], snapshotID: 1}).parse()
	require.Error(t, err)
	require.Contains(t, err.Error(), "header truncated")
}

func TestParseBodyPropagatesSinkError(t *testing.T) {
	sentinel := errors.New("sink refused the snapshot")
	_, err := (&bodyParser{
		region:     armBody(t, nil),
		snapshotID: 1,
		rowCount:   1,
		sink:       &refusingSink{err: sentinel},
	}).parse()
	require.ErrorIs(t, err, sentinel)
}

// TestParseFencesEmptyPageRegion: parseFences owns the "no pages" answer for its
// callers; the pages region ends where the fence directory would have begun.
func TestParseFencesEmptyPageRegion(t *testing.T) {
	fences, pageEnd, err := (&pageParser{pageStart: 7, snapshotID: 1}).parseFences()
	require.NoError(t, err)
	require.Nil(t, fences)
	require.Equal(t, 7, pageEnd, "an empty pages region ends where it started")
}
