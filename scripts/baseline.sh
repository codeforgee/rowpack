#!/usr/bin/env bash
#
# RowPack 性能基线运行器
#
# 用法:
#   scripts/baseline.sh [full|quick|1m]      # 默认 full
#
# 输出:
#   testdata/baseline/<YYYY-MM-DD>[-label][-n].txt
#   （同名文件已存在时自动追加 -2/-3…，不会覆盖历史基线）
#
# 环境变量:
#   BASELINE_LABEL   文件名后缀标签，如 BASELINE_LABEL=m1pro
#   BENCHTIME        基准时长（默认 1s）
#   BENCHCOUNT       重复次数（默认 1）
#   ROWPACK_BENCH_ROWS / ROWPACK_BENCH_ROWS1M  数据集行数
#
# 统一标注：文件开头固定写入日期 / git / Go / OS-arch / CPU / 内存 / 格式版本 /
# 基准参数，随后追加对应档位的完整 `go test -bench` 输出。
set -euo pipefail

cd "$(dirname "$0")/.."

mode="${1:-full}"
case "$mode" in
  full)  make_target="bench";        benchtime="${BENCHTIME:-1s}"; count="${BENCHCOUNT:-1}" ;;
  quick) make_target="bench-quick";  benchtime="3x";                count="1" ;;
  1m)    make_target="bench-1m";     benchtime="${BENCHTIME:-1s}";  count="${BENCHCOUNT:-1}" ;;
  *) echo "usage: $0 [full|quick|1m]" >&2; exit 2 ;;
esac

# ---- 输出文件：testdata/baseline/<date>[-label][-n].txt ----
date_str="$(date +%Y-%m-%d)"
label="${BASELINE_LABEL:-}"
out_dir="testdata/baseline"
mkdir -p "$out_dir"
stem="$out_dir/${date_str}${label:+-$label}"
out="$stem.txt"
n=2
while [ -e "$out" ]; do out="$stem-$n.txt"; n=$((n + 1)); done

# ---- 机器信息（macOS / Linux 兼容）----
cpu="$( { sysctl -n machdep.cpu.brand_string 2>/dev/null \
          || awk -F: '/model name/{gsub(/^ +/, "", $2); print $2; exit}' /proc/cpuinfo 2>/dev/null \
          || uname -m; } | head -n1 )"
mem_bytes="$( { sysctl -n hw.memsize 2>/dev/null \
              || awk '/MemTotal/{print $2 * 1024; exit}' /proc/meminfo 2>/dev/null; } | head -n1 )"
if [ -n "${mem_bytes:-}" ] && [ "$mem_bytes" -gt 0 ] 2>/dev/null; then
  mem="$(awk -v b="$mem_bytes" 'BEGIN{ if (b>=1073741824) printf "%.0f GiB", b/1073741824; else printf "%.0f MiB", b/1048576 }')"
else
  mem="unknown"
fi

if git rev-parse --short HEAD >/dev/null 2>&1; then
  git_rev="$(git rev-parse --short HEAD)"
  git rev-parse --verify HEAD >/dev/null 2>&1 && git diff --quiet 2>/dev/null || git_rev="$git_rev-dirty"
else
  git_rev="unknown"
fi

zstd_ver="$(go list -m -f '{{.Version}}' github.com/klauspost/compress 2>/dev/null || echo unknown)"

# 归档头所需的稳定字段（人类可读 + 机器可读）
now_utc="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
go_short="$(go env GOVERSION 2>/dev/null || go version)"
go_os="$(go env GOOS)"
go_arch="$(go env GOARCH)"
rows="${ROWPACK_BENCH_ROWS:-100000}"
rows1m="${ROWPACK_BENCH_ROWS1M:-1000000}"

{
  echo "# RowPack 性能基线"
  echo "# date: $now_utc"
  echo "# mode: $mode  (make $make_target)"
  echo "# git: $git_rev"
  echo "# go: $(go version)"
  echo "# os/arch: $go_os/$go_arch"
  echo "# cpu: ${cpu:-unknown}"
  echo "# mem: ${mem:-unknown}"
  echo "# zstd: ${zstd_ver}"
  echo "# format: v1 single-file (Magic ROWPACK1, Major 1)"
  echo "# config: BlockSize 256K / PageSize 32K / Zstd L3 / mmap / SyncCommit"
  echo "# benchtime: $benchtime  count: $count"
  echo "# rows: $rows  rows1m: $rows1m"
  echo "# command: make $make_target BENCHTIME=$benchtime BENCHCOUNT=$count"
  # 机器可读：scripts/baseline-diff.sh 解析这一行做元数据一致性校验。
  printf '# json: {"ts":"%s","mode":"%s","git":"%s","go":"%s","os":"%s","arch":"%s","cpu":"%s","mem":"%s","zstd":"%s","format":"v1","magic":"ROWPACK1","benchtime":"%s","count":%s,"rows":%s,"rows1m":%s}\n' \
    "$now_utc" "$mode" "$git_rev" "$go_short" "$go_os" "$go_arch" "${cpu:-unknown}" "${mem:-unknown}" "$zstd_ver" "$benchtime" "$count" "$rows" "$rows1m"
  echo
} >"$out"

echo "==> 基线输出: $out"

# `make` 目标自身会把原始输出写到 bench/results*.txt；这里同时追加到归档文件。
make "$make_target" BENCHTIME="$benchtime" BENCHCOUNT="$count" 2>&1 | tee -a "$out"

echo
echo "==> 已写入 $out"
