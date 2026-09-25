package rowpack

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/rowpack/rowpack/internal/format"
)

// options_arms_test.go 覆盖 diskCompression 的兜底臂:这个映射必须是全函数,任何本
// 版本不认识的枚举值(或还没被 resolved 填上的默认值)都要落到「不压缩」,而不是让
// 写入路径拿到一个零值枚举写出无人能读的块。默认值归 resolved 管,这里只保证兜底。

func TestDiskCompressionFallsBackToNone(t *testing.T) {
	require.Equal(t, format.CompressionNone, Options{}.diskCompression(),
		"the unresolved default maps to none")
	require.Equal(t, format.CompressionNone, Options{Compression: Compression(99)}.diskCompression(),
		"a compression this build does not know maps to none")
}
