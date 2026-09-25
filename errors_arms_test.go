package rowpack

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// errors_arms_test.go 覆盖错误分类/渲染的两个边界:分类器必须是个全函数(nil 原因不
// 是错误,不包装),以及 Reason 遇到「内层也是 CorruptionError 但没有 Reason」时递归
// 到内层的 Cause,而不是把整条消息再抄一遍(file/offset/block 已经在外层)。

func TestCorruptErrorNilCause(t *testing.T) {
	require.Nil(t, corruptError("store.rpk", 0, 1, 1, 1, nil),
		"nothing to classify is not an error")
}

func TestReasonOf(t *testing.T) {
	require.Equal(t, "", reasonOf(nil))

	// A corruption carrying no reason of its own contributes its cause's text.
	nested := &CorruptionError{Kind: ErrSchemaMismatch, Cause: errors.New("boom")}
	require.Equal(t, "boom", reasonOf(&CorruptionError{Kind: ErrCorruptData, Cause: nested}))

	// A reason already set wins: no recursion into the cause.
	outer := &CorruptionError{Kind: ErrCorruptData, Reason: "already described", Cause: nested}
	require.Equal(t, "already described", reasonOf(outer))
}
