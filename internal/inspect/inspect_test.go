package inspect

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rowpack/rowpack"
	"github.com/stretchr/testify/require"
)

// goldenBase 是 testdata/golden 下样本的 base 路径（不含 .rpk）。
func goldenBase(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "golden", name))
	require.NoError(t, err)
	_, statErr := os.Stat(p + ".rpk")
	require.NoError(t, statErr, "golden sample must exist")
	return p
}

func run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	err := Run(context.Background(), args, &out, &errb)
	code := ExitOK
	if err != nil {
		var ee *ExitError
		require.True(t, errors.As(err, &ee), "Run must report an exit status, got %T: %v", err, err)
		code = ee.Code
	}
	return out.String(), errb.String(), code
}

func TestRunUsageErrors(t *testing.T) {
	out, errb, code := run(t)
	require.Equal(t, ExitUsage, code)
	require.Empty(t, out)
	require.Contains(t, errb, "usage: rowpack-inspect")

	_, errb, code = run(t, "header")
	require.Equal(t, ExitUsage, code, "command without a store path")
	require.Contains(t, errb, "usage: rowpack-inspect")

	_, _, code = run(t, "nope", "x")
	require.Equal(t, ExitUsage, code, "unknown command")

	var out2 bytes.Buffer
	err := Run(context.Background(), []string{"--bogus"}, &out2, &bytes.Buffer{})
	require.Error(t, err)
	var ee *ExitError
	require.True(t, errors.As(err, &ee))
	require.Equal(t, ExitUsage, ee.Code, "flag parse failure is a usage error")
}

func TestHeaderListVerifyOnGoldenStore(t *testing.T) {
	ctx := context.Background()
	var out, errb bytes.Buffer
	require.NoError(t, Run(ctx, []string{"header", goldenBase(t, "full-delta-store")}, &out, &errb))
	body := out.String()
	require.Contains(t, body, "path: "+goldenBase(t, "full-delta-store")+".rpk")
	require.Contains(t, body, "readonly: true")
	// 5 个 Rows/Metadata 块 + 2 个快照 Meta 块（FULL 与 DELTA 各一个）。
	require.Contains(t, body, "snapshots=3 blocks=7 tables=3")
	// 最新快照是空 DELTA：31 = 继承下来的全部可见行（Stats 口径回归）。
	require.Contains(t, body, "logicalRows=31")
	require.Contains(t, body, "recovery: performed=false dataTail=0 indexTail=0 rebuilt=0")
	require.Contains(t, body, "pageIO: loads=")
	require.Empty(t, errb.String())

	out.Reset()
	require.NoError(t, Run(ctx, []string{"list", goldenBase(t, "full-delta-store")}, &out, &errb))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Contains(t, lines[0], "snapshot 1 FULL parent=0")
	require.Contains(t, out.String(), "snapshot 2 DELTA parent=1")
	require.Contains(t, out.String(), "snapshot 3 DELTA parent=2")
	require.Contains(t, out.String(), `table 1 "users" latestVersion=1 ns=user address=users`)
	require.Contains(t, out.String(), `table 2 "empty"`)
	require.Empty(t, errb.String())

	out.Reset()
	require.NoError(t, Run(ctx, []string{"verify", goldenBase(t, "full-delta-store")}, &out, &errb))
	require.Contains(t, out.String(), "OK snapshots=3 blocks=7 rows=34 bytes=")
	require.Empty(t, errb.String())
}

func TestEmptyStoreCommands(t *testing.T) {
	ctx := context.Background()
	var out, errb bytes.Buffer
	require.NoError(t, Run(ctx, []string{"list", goldenBase(t, "empty-store")}, &out, &errb))
	require.Empty(t, strings.TrimSpace(out.String()), "no snapshots to print")
	require.NoError(t, Run(ctx, []string{"verify", goldenBase(t, "empty-store")}, &out, &errb))
	require.Contains(t, out.String(), "OK snapshots=0 blocks=0 rows=0")
}

func TestOpenFailures(t *testing.T) {
	ctx := context.Background()
	var out, errb bytes.Buffer
	err := Run(ctx, []string{"header", filepath.Join(t.TempDir(), "missing")}, &out, &errb)
	var ee *ExitError
	require.True(t, errors.As(err, &ee))
	require.Equal(t, ExitFailure, ee.Code)
	require.Contains(t, errb.String(), "open:")

	// 加密样本没有 key：必须失败退出而不是打印空统计。
	out.Reset()
	errb.Reset()
	err = Run(ctx, []string{"header", goldenBase(t, "encrypted-store")}, &out, &errb)
	require.True(t, errors.As(err, &ee))
	require.Equal(t, ExitFailure, ee.Code, "an encrypted store needs a key")
	require.Contains(t, errb.String(), "key")
}

func TestDumpCommands(t *testing.T) {
	ctx := context.Background()
	base := goldenBase(t, "full-delta-store")
	var out, errb bytes.Buffer
	require.NoError(t, Run(ctx, []string{"dump", base, "1", "1"}, &out, &errb))
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	require.Greater(t, len(lines), 5, "the FULL snapshot must dump several rows")
	require.True(t, strings.HasPrefix(lines[0], "1\t"), "row key then tab-separated values: %q", lines[0])
	require.Contains(t, lines[0], "user-1")
	require.Empty(t, errb.String())

	// DELTA 上的视图与 FULL 不同（删除生效）。
	out.Reset()
	require.NoError(t, Run(ctx, []string{"dump", base, "2", "1"}, &out, &bytes.Buffer{}))
	require.NotEqual(t, out.String(), strings.Join(lines, "\n"))

	// 参数错误：缺参数、非数字、表不存在、快照不存在。
	_, _, code := run(t, "dump", base, "1")
	require.Equal(t, ExitUsage, code, "dump needs both numeric args")
	_, eb, code := run(t, "dump", base, "abc", "1")
	require.Equal(t, ExitUsage, code, "bad snapshotID must be reported, not defaulted to 0")
	require.Contains(t, eb, "bad snapshotID")
	_, eb, code = run(t, "dump", base, "1", "xyz")
	require.Equal(t, ExitUsage, code, "bad tableID must be reported")
	require.Contains(t, eb, "bad tableID")
	_, eb, code = run(t, "dump", base, "1", "999")
	require.Equal(t, ExitFailure, code)
	require.Contains(t, eb, "not found")
	_, eb, code = run(t, "dump", base, "99", "1")
	require.Equal(t, ExitFailure, code, "unknown snapshot")
	require.Contains(t, eb, "not found")
}

// TestDumpResolvesNonDefaultNamespaceTables 是 CLI 的真实缺陷回归：dump 曾把
// Tables() 返回的裸名传给 Scan，而不是 (ns, name) 地址，于是任何非默认 ns 的表
// 都报「表不存在」而读不出来。
func TestDumpResolvesNonDefaultNamespaceTables(t *testing.T) {
	ctx := context.Background()
	base := filepath.Join(t.TempDir(), "nsstore")
	writeNsStore(t, base)

	var out, errb bytes.Buffer
	require.NoError(t, Run(ctx, []string{"list", base}, &out, &errb))
	listing := out.String()
	require.Contains(t, listing, `table 1 "plain"`)
	require.Contains(t, listing, `table 2 "hidden" latestVersion=1 ns=secret address=secret.hidden`)

	// 表 2 只在 ns=secret 下可见：按地址解析才能 dump。
	out.Reset()
	errb.Reset()
	require.NoError(t, Run(ctx, []string{"dump", base, "1", "2"}, &out, &errb),
		"dump of a namespaced table must succeed: %s", errb.String())
	require.Contains(t, out.String(), "1\tshh-1")
	require.Contains(t, out.String(), "2\tshh-2")
	require.Empty(t, errb.String())
}

func writeNsStore(t *testing.T, base string) {
	t.Helper()
	ctx := context.Background()
	db, err := rowpack.Create(base, rowpack.Options{})
	require.NoError(t, err)
	tx, err := db.Begin(ctx, rowpack.NoParent)
	require.NoError(t, err)
	cols := []rowpack.Column{{Name: "id", Type: rowpack.TypeUint64}, {Name: "v", Type: rowpack.TypeString}}
	require.NoError(t, tx.DefineTable("plain", cols))
	require.NoError(t, tx.DefineTableIn("secret", "hidden", cols))
	for i := rowpack.RowID(1); i <= 3; i++ {
		require.NoError(t, tx.Insert("plain", i, rowpack.Row{rowpack.Uint64(uint64(i)), rowpack.String("pl-" + itoa(i))}))
		require.NoError(t, tx.Insert("secret.hidden", i, rowpack.Row{rowpack.Uint64(uint64(i)), rowpack.String("shh-" + itoa(i))}))
	}
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

func itoa(n rowpack.RowID) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestValueRendering 覆盖每种类型的展示形态（含 NULL 与 Decimal 标度）。
func TestValueRendering(t *testing.T) {
	tod, err := rowpack.NewTimeOfDay(13, 45, 6, 123456789)
	require.NoError(t, err)
	dt := time.Date(2026, time.February, 3, 4, 5, 6, 7, time.UTC)
	dec := rowpack.Decimal{Unscaled: big.NewInt(123450), Scale: 4}
	cases := []struct {
		v    rowpack.Value
		want string
	}{
		{rowpack.Null(), "NULL"},
		{rowpack.Bool(true), "true"},
		{rowpack.Int8(-8), "-8"},
		{rowpack.Int16(-16), "-16"},
		{rowpack.Int32(-32), "-32"},
		{rowpack.Int64(-64), "-64"},
		{rowpack.Uint8(8), "8"},
		{rowpack.Uint16(16), "16"},
		{rowpack.Uint32(32), "32"},
		{rowpack.Uint64(64), "64"},
		{rowpack.Float32(1.5), "1.5"},
		{rowpack.Float64(2.25), "2.25"},
		{rowpack.String("hi"), "hi"},
		{rowpack.Bytes([]byte{0xde, 0xad}), "dead"},
		{rowpack.DateValue(rowpack.NewDate(dt)), "2026-02-03"},
		{rowpack.TimeValue(tod), "13:45:06.123456789"},
		{rowpack.DateTime(dt), "2026-02-03T04:05:06.000000007Z"},
		{rowpack.DecimalValue(dec), "123450/4"},
		{rowpack.DecimalValue(rowpack.Decimal{Unscaled: big.NewInt(0), Scale: 0}), "0/0"},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, Value(tc.v), "type %d", tc.v.Type())
	}
}
