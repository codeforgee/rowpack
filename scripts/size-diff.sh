#!/usr/bin/env bash
#
# 对比两份体积基线报告，标出超过阈值的回退/改进。
#
# 用法:
#   scripts/size-diff.sh <old> <new> [threshold%]
#   <old>/<new> 可以是日期（2026-10-10）或文件路径。
#
# 环境变量:
#   VERBOSE=1   同时打印阈值内的变化
#
# 退出码: 0 = 无超阈值回退；1 = 存在超阈值回退；2 = 用法/文件错误。
#
# 体积报告全部指标都是"越小越好"。样本形状（tables/snapshots/blocks/
# logicalRows/writes）先比对：形状变了说明生成器改过，此时体积差异不
# 能直接归因于格式变更。
set -euo pipefail

cd "$(dirname "$0")/.."

usage() { echo "usage: $0 <old> <new> [threshold%]" >&2; exit 2; }
resolve() {
  local a="$1"
  if [ -f "$a" ]; then printf '%s\n' "$a"; return; fi
  if [ -f "testdata/size-baseline/$a.txt" ]; then printf '%s\n' "testdata/size-baseline/$a.txt"; return; fi
  if [ -f "testdata/size-baseline/$a" ]; then printf '%s\n' "testdata/size-baseline/$a"; return; fi
  echo "找不到体积基线: $a" >&2; exit 2
}

[ $# -ge 2 ] || usage
old_file="$(resolve "$1")"
new_file="$(resolve "$2")"
thr="${3:-0.5}"

echo "== 体积对比 =="
echo "  old: $old_file"
echo "  new: $new_file"
echo "  阈值: ${thr}%"
echo

awk -v thr="$thr" -v verbose="${VERBOSE:-0}" '
FNR == 1 { side++ }

# 形状行: "# tables: 8  snapshots: 21  blocks: 291  logicalRows: 420298"
/^# tables: / {
  shape[side,"tables"] = $3 + 0
  shape[side,"snapshots"] = $5 + 0
  shape[side,"blocks"] = $7 + 0
  shape[side,"logicalRows"] = $9 + 0
  next
}
/^# writes: / { shape[side,"writes"] = $3 + 0; next }
/^# seed: /   { seed[side] = $3; next }
/^# scale: /  { scale[side] = $3; next }

# 指标行: "dataBytes            18657987"
$0 !~ /^#/ && NF == 2 {
  keys[$1] = 1
  val[side,$1] = $2 + 0
  next
}

END {
  print "-- 样本形状 --"
  shape_bad = 0
  split("tables snapshots blocks logicalRows writes", sk, " ")
  for (i = 1; i <= 5; i++) {
    k = sk[i]
    a = shape[1,k]; b = shape[2,k]
    if (a == "" && b == "") continue
    if (a != b) {
      shape_bad = 1
      printf "  DIFF  %-12s %s  ->  %s\n", k, a, b
    }
  }
  if (seed[1] != seed[2] || scale[1] != scale[2]) {
    shape_bad = 1
    printf "  DIFF  %-12s seed %s / scale %s  ->  seed %s / scale %s\n", \
      "params", seed[1], scale[1], seed[2], scale[2]
  }
  if (shape_bad) print "  ! 样本形状不一致：体积差异不能归因于格式变更"
  print ""

  print "-- 体积（越小越好；仅列 |Δ| >= " thr "%，VERBOSE=1 显示全部）--"
  nreg = 0; nimp = 0; nok = 0; nskip = 0
  # 固定顺序输出，避免 for-in 的随机次序。未列出的指标（如报告格式新增）
  # 会在末尾按收集顺序补上，不会静默消失。
  nk = split("dataBytes rawBytes storedBytes indexMemoryBytes idxRowIDsBytes idxOrdinalsBytes idxChangesBytes idxRunStartBytes idxBlockIDsBytes idxShardFixedBytes idxAccountedBytes idxSlackBytes heapAfterOpenBytes oversizedPages ratio bytePerRow bytePerWrittenRow idxBytePerRow", mk, " ")
  for (i = 1; i <= nk; i++) {
    k = mk[i]
    if (!(k in keys)) continue
    done[k] = 1
    if (!((1,k) in val) || !((2,k) in val)) { nskip++; continue }
    o = val[1,k]; n = val[2,k]
    if (o == 0 && n == 0) { nok++; continue }
    if (o == 0) {   # 0 -> 非 0：全部指标越小越好，即回退
      nreg++
      printf "  REGRESS  %-20s %14s -> %-14s     n/a\n", k, o, n
      continue
    }
    d = (n - o) / o * 100
    # 堆读数受 GC 时机影响，同一次代码两次跑也有 ~1% 抖动；体积是确定的。
    mthr = (k == "heapAfterOpenBytes" && thr < 5) ? 5 : thr
    if (d > mthr) { tag = "REGRESS"; nreg++ }
    else if (d < -mthr) { tag = "IMPROVE"; nimp++ }
    else { tag = "OK     "; nok++ }
    if (tag == "OK     " && verbose != "1") continue
    printf "  %s  %-20s %14s -> %-14s %+8.2f%%\n", tag, k, o, n, d
  }
  # 报告格式新增的指标：上面的清单没列，这里补上，免得静默漏掉。
  for (k in keys) {
    if (done[k]) continue
    if (!((1,k) in val) || !((2,k) in val)) { nskip++; continue }
    o = val[1,k]; n = val[2,k]
    if (o == 0 && n == 0) { nok++; continue }
    if (o == 0) {
      nreg++
      printf "  REGRESS  %-20s %14s -> %-14s     n/a\n", k, o, n
      continue
    }
    d = (n - o) / o * 100
    mthr = (k == "heapAfterOpenBytes" && thr < 5) ? 5 : thr
    if (d > mthr) { tag = "REGRESS"; nreg++ }
    else if (d < -mthr) { tag = "IMPROVE"; nimp++ }
    else { tag = "OK     "; nok++ }
    if (tag == "OK     " && verbose != "1") continue
    printf "  %s  %-20s %14s -> %-14s %+8.2f%%  (清单外)\n", tag, k, o, n, d
  }
  print ""
  printf "-- 汇总: 回退 %d · 改进 %d · 阈值内 %d · 缺测 %d --\n", nreg, nimp, nok, nskip
  if (shape_bad) print "警告: 两份报告的样本形状不一致"
  exit (nreg > 0 ? 1 : 0)
}
' "$old_file" "$new_file"
