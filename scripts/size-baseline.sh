#!/usr/bin/env bash
#
# RowPack 体积基线运行器
#
# 生成大型复杂样本（cmd/rowpack-sizesample），把体积报告归档到
# testdata/size-baseline/。与 scripts/baseline.sh（性能）互补：那边量
# ns/op，这边量字节。
#
# 用法:
#   scripts/size-baseline.sh [label]
#
# 输出:
#   testdata/size-baseline/<YYYY-MM-DD>[-label][-n].txt
#   （同名文件已存在时自动追加 -2/-3…，不会覆盖历史基线）
#
# 环境变量:
#   SIZE_SCALE       行数缩放（默认 1，约 42 万逻辑行 / 33 万次增量写）
#   SIZE_SNAPSHOTS   DELTA 快照数（默认 20，加上 FULL 共 21）
#   SAMPLE_DIR       样本落盘目录（默认 testdata/size-sample，已 gitignore）
#
# 样本约 2 秒生成、约 18 MB 落盘。入版本库的是这份体积报告（几 KB），
# 不是样本本身——样本字节含 StoreUUID 与时间戳，本来就不可复现。
set -euo pipefail

cd "$(dirname "$0")/.."

label="${1:-${SIZE_LABEL:-}}"
out_dir="testdata/size-baseline"
sample_dir="${SAMPLE_DIR:-testdata/size-sample}"
mkdir -p "$out_dir"

date_str="$(date +%Y-%m-%d)"
stem="$out_dir/${date_str}${label:+-$label}"
out="$stem.txt"
n=2
while [ -e "$out" ]; do out="$stem-$n.txt"; n=$((n + 1)); done

echo "==> 生成样本: $sample_dir"
go run ./cmd/rowpack-sizesample \
  -out "$sample_dir" \
  -scale "${SIZE_SCALE:-1}" \
  -snapshots "${SIZE_SNAPSHOTS:-20}" 2>&1 | tee "$out"

echo
echo "==> 已写入 $out"
echo "==> 对比: scripts/size-diff.sh <旧基线> $out"
