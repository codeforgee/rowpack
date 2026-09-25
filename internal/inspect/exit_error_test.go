package inspect

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestExitErrorMessageBranches 钉住 ExitError 的两个身份:命令失败时它就是被包装
// 的那个错误(Error 转发、Unwrap 可被 errors.Is 命中);用法错误没有底层错误时退
// 化为「状态码+退出码」的稳定文案。main 只做code → os.Exit 的映射,这层语义必须
// 由 ExitError 自己保持。
func TestExitErrorMessageBranches(t *testing.T) {
	sentinel := errors.New("open: missing.rpk: no such file or directory")

	wrapped := &ExitError{Code: ExitFailure, Err: sentinel}
	require.Equal(t, sentinel.Error(), wrapped.Error(), "a wrapped error is reported verbatim")
	require.ErrorIs(t, wrapped, sentinel, "Unwrap must expose the underlying failure")
	require.Same(t, sentinel, wrapped.Unwrap())

	bare := &ExitError{Code: ExitUsage}
	require.Contains(t, bare.Error(), "rowpack-inspect: exit")
	require.Contains(t, bare.Error(), "2", "the usage exit code is part of the fallback message")
	require.Nil(t, bare.Unwrap(), "a usage error carries no underlying error")
	require.NotErrorIs(t, bare, sentinel)

	// 退出码在两种形态下都不能被改写。
	require.Equal(t, ExitFailure, wrapped.Code)
	require.Equal(t, ExitUsage, bare.Code)
}

// TestEveryCommandPropagatesOpenFailure 覆盖四条命令共用的 openStore 失败臂:
// 打不开 store 时它们都必须以 ExitFailure 退出,而不是打印空结果冒充成功。
func TestEveryCommandPropagatesOpenFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-store")
	cases := []struct {
		name string
		args []string
	}{
		{"header", []string{"header", missing}},
		{"list", []string{"list", missing}},
		{"verify", []string{"verify", missing}},
		{"dump", []string{"dump", missing, "1", "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			err := Run(context.Background(), tc.args, &out, &errb)
			var ee *ExitError
			require.True(t, errors.As(err, &ee), "%s must report an exit status", tc.name)
			require.Equal(t, ExitFailure, ee.Code, "%s: unexpected exit code", tc.name)
			require.Contains(t, errb.String(), "open:", "%s must diagnose the failed open", tc.name)
			require.Empty(t, out.String(), "%s must not print output for a store it cannot open", tc.name)
		})
	}
}
