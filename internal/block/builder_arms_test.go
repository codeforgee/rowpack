package block

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
)

// builder_arms_test.go 覆盖 RowsBuilder 的构建错误传播臂:页面/块的每一次搬运都
// 可能在中途失败(未知压缩算法、回调拒绝),而 Add/Flush 必须把失败原样交回调用
// 方,不能留下半成品状态预售给 downstream。

// failingSink records every flushed block and rejects them once armed, so the
// "block reaches the sink but the consumer refuses it" arm can be driven.
type failingSink struct {
	blocks []*FlushedBlock
	fail   bool
	err    error
}

func (s *failingSink) onFlush(fb *FlushedBlock) error {
	s.blocks = append(s.blocks, fb)
	if s.fail {
		return s.err
	}
	return nil
}

// rowsBuilderWith returns a builder whose page/block targets are small enough
// that both page-full and block-full transitions fire on demand.
func rowsBuilderWith(pageSize, blockSize int, sink func(*FlushedBlock) error) *RowsBuilder {
	b := NewRowsBuilder(1, 1, Config{
		BlockSize:   blockSize,
		Compression: format.CompressionNone,
		Limits:      DefaultLimits(),
		OnFlush:     sink,
	})
	b.SetPageSize(pageSize)
	return b
}

// bodyBytes is an opaque record body of n bytes: the page builder treats tuples
// as opaque byte strings at this layer.
func bodyBytes(n int) []byte { return bytes.Repeat([]byte{0x41}, n) }

func TestRowsBuilderSetPageSizeGuards(t *testing.T) {
	b := NewRowsBuilder(1, 1, Config{
		BlockSize:   1024,
		Compression: format.CompressionNone,
		Limits:      DefaultLimits(),
		OnFlush:     func(*FlushedBlock) error { return nil },
	})
	require.Equal(t, 1024, b.pageSize, "the page target is clamped to the block target")

	b.SetPageSize(0)
	require.Equal(t, 1024, b.pageSize, "a non-positive target is ignored, not zeroed")
	require.Equal(t, 1024, b.page.target, "the page builder keeps the previous target")

	b.SetPageSize(4096)
	require.Equal(t, 1024, b.pageSize, "a page target above the block is clamped")
	require.Equal(t, 1024, b.page.target)

	b.SetPageSize(256)
	require.Equal(t, 256, b.pageSize)
	require.Equal(t, 256, b.page.target, "the accepted target reaches the page builder")
}

func TestRowsBuilderAddPropagatesPageErrors(t *testing.T) {
	// Change types are 1..3 (Insert/Update/Delete); anything outside that set
	// is unrepresenable in the 2-bit page stream and only the page builder can
	// reject it — both Add paths must surface the failure.
	const illegal = format.ChangeType(9)

	t.Run("normal path", func(t *testing.T) {
		sink := &failingSink{}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		err := b.Add(1, 1, illegal, bodyBytes(16))
		require.Error(t, err)
		require.Contains(t, err.Error(), "change type 9 not packable")
		require.Empty(t, b.entries, "a rejected record leaves no directory entry")
	})

	t.Run("oversized path", func(t *testing.T) {
		sink := &failingSink{}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		err := b.Add(1, 1, illegal, bodyBytes(512))
		require.Error(t, err)
		require.Contains(t, err.Error(), "change type 9 not packable")
		require.Empty(t, b.dirEntries, "no page is stored for a rejected oversized record")
	})
}

func TestRowsBuilderCompressionFailurePropagates(t *testing.T) {
	// An unsupported algorithm makes every page store fail deterministically,
	// which is what brings out the propagation arms. The builder cannot be
	// constructed this way (Config validates it), so the field is set directly.
	const bad = format.Compression(42)

	t.Run("page filled by Add", func(t *testing.T) {
		sink := &failingSink{}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		b.compress = bad
		err := b.Add(1, 1, format.ChangeInsert, bodyBytes(256))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported compression")
		require.Empty(t, sink.blocks, "a page that cannot be stored must not emit a block")
	})

	t.Run("oversized record", func(t *testing.T) {
		sink := &failingSink{}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		b.compress = bad
		err := b.Add(1, 1, format.ChangeInsert, bodyBytes(512))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported compression")
	})

	t.Run("pending page ahead of an oversized record", func(t *testing.T) {
		// The oversized path finishes the pending page first, so its store
		// failure is the one that surfaces.
		sink := &failingSink{}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		require.NoError(t, b.Add(1, 1, format.ChangeInsert, bodyBytes(16)))
		b.compress = bad
		err := b.Add(2, 1, format.ChangeInsert, bodyBytes(512))
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported compression")
		require.Empty(t, sink.blocks)
	})

	t.Run("Flush", func(t *testing.T) {
		sink := &failingSink{}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		b.compress = bad
		require.NoError(t, b.Add(1, 1, format.ChangeInsert, bodyBytes(16)))
		err := b.Flush()
		require.Error(t, err)
		require.Contains(t, err.Error(), "unsupported compression")
	})
}

func TestRowsBuilderFlushPropagatesSinkErrors(t *testing.T) {
	sentinel := errors.New("sink rejected the block")

	t.Run("explicit Flush", func(t *testing.T) {
		sink := &failingSink{err: sentinel}
		b := rowsBuilderWith(256, 1<<16, sink.onFlush)
		require.NoError(t, b.Add(1, 1, format.ChangeInsert, bodyBytes(16)))
		sink.fail = true
		require.ErrorIs(t, b.Flush(), sentinel)
		require.Len(t, sink.blocks, 1, "the block reaches the sink even when the sink rejects it")
	})

	t.Run("block-full inside Add", func(t *testing.T) {
		// Two page-sized records overflow the block target, so Add itself
		// flushes; the sink's refusal must reach the caller of Add.
		sink := &failingSink{err: sentinel}
		b := rowsBuilderWith(256, 512, sink.onFlush)
		require.NoError(t, b.Add(1, 1, format.ChangeInsert, bodyBytes(256)))
		sink.fail = true
		require.ErrorIs(t, b.Add(2, 1, format.ChangeInsert, bodyBytes(256)), sentinel)
	})

	t.Run("oversized record carries its own block", func(t *testing.T) {
		// A record at least as large as the block target also flushes the
		// pending pages first, so the sink's refusal surfaces from Add even
		// though the page that filled is already stored.
		sink := &failingSink{err: sentinel}
		b := rowsBuilderWith(256, 1024, sink.onFlush)
		require.NoError(t, b.Add(1, 1, format.ChangeInsert, bodyBytes(256)))
		sink.fail = true
		require.ErrorIs(t, b.Add(2, 1, format.ChangeInsert, bodyBytes(1024)), sentinel)
		require.Len(t, sink.blocks, 1, "the pending page is emitted as a block before the oversized one")
	})
}
