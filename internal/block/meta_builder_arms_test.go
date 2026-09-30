package block

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/metadata"
)

// meta_builder_arms_test.go 覆盖 Metadata Block 构建的两条错误传播臂:压缩算法不被支持
// (配置错误,不该悄悄变成明文块)与下游拒绝接收(块已经成形但消费者不要,错误要原样
// 交回调用方)。
//
// 到不了的八条,都是「自己造的东西不可能不成立」:
//   - builder.go 164、175、202:RowsBuilder 自己 Add 出来的页,Finish 不可能失败。
//   - builder.go 225:同一页的重新解析失败——注释里也说明了这是编码错误而非数据错误。
//   - builder.go 257:entries 非空时目录必非空(每条 entry 要么进了当前页、要么已经作为
//     超大页单独存下),所以 Flush 里那个「没有页可发」的提前返回没有输入。
//   - meta_builder.go 83:Build 组装的是刚刚 Add 进来的记录,不会失败。
//   - rows_page.go 149:页头写入的是按 StreamsBytes() 精确分配的缓冲,MarshalTo 不会失败。
//   - rows_page.go 328:schema RLE 的读取循环每次迭代都会把 runEnd 往前推,结束时它要么
//     等于 count、要么被「run 超过剩余记录数」先拦下,覆盖不足这一支没有输入。

// TestMetadataBuilderRejectsUnsupportedCompression: an unknown algorithm is a
// configuration error — the block must not silently fall back to plaintext
// (92).
func TestMetadataBuilderRejectsUnsupportedCompression(t *testing.T) {
	var sink containerSink
	b := NewMetadataBuilder(1, 1, Config{
		BlockSize:   1 << 20,
		Compression: format.Compression(99),
		Limits:      DefaultLimits(),
		OnFlush:     sink.flush,
	})

	require.NoError(t, b.Add(metadata.DirectoryEntry{}, []byte("record")))
	require.ErrorContains(t, b.Flush(), "unsupported compression")
	require.Empty(t, sink.blocks, "no block is emitted for an unsupported algorithm")
}

// TestMetadataBuilderPropagatesSinkFailure: a block the consumer refuses is an
// error, not a dropped block — the writer must not advance past it (105).
func TestMetadataBuilderPropagatesSinkFailure(t *testing.T) {
	var sink failingSink
	sink.fail = true
	sink.err = errFault
	b := NewMetadataBuilder(1, 1, Config{
		BlockSize:   1 << 20,
		Compression: format.CompressionNone,
		Limits:      DefaultLimits(),
		OnFlush:     sink.onFlush,
	})

	require.NoError(t, b.Add(metadata.DirectoryEntry{}, []byte("record")))
	require.ErrorIs(t, b.Flush(), errFault)
	require.Len(t, sink.blocks, 1, "the block was assembled and offered, then refused")
}
