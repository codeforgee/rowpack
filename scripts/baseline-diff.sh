#!/usr/bin/env bash
#
# 对比两个归档性能基线，标出超过阈值的回退/改进。
#
# 用法:
#   scripts/baseline-diff.sh <old> <new> [threshold%]
#   <old>/<new> 可以是日期（2026-09-10）或文件路径。
#
# 环境变量:
#   VERBOSE=1   同时打印阈值内的变化
#
# 退出码: 0 = 无超阈值回退；1 = 存在超阈值回退；2 = 用法/文件错误。
set -euo pipefail

cd "$(dirname "$0")/.."

usage() { echo "usage: $0 <old> <new> [threshold%]" >&2; exit 2; }
resolve() {
  local a="$1"
  if [ -f "$a" ]; then printf '%s\n' "$a"; return; fi
  if [ -f "testdata/baseline/$a.txt" ]; then printf '%s\n' "testdata/baseline/$a.txt"; return; fi
  if [ -f "testdata/baseline/$a" ]; then printf '%s\n' "testdata/baseline/$a"; return; fi
  echo "找不到基线文件: $a" >&2; exit 2
}

[ $# -ge 2 ] || usage
old_file="$(resolve "$1")"
new_file="$(resolve "$2")"
thr="${3:-10}"
thr="${thr:-10}"

echo "== 基线对比 =="
echo "  old: $old_file"
echo "  new: $new_file"
echo "  阈值: ${thr}%"
echo

# 元数据一致性 + 指标 diff（awk 单遍处理两个文件）。
awk -v thr="$thr" -v verbose="${VERBOSE:-0}" '
function trim(s) { gsub(/^[ \t]+|[ \t]+$/, "", s); return s }
function norm(n) { sub(/-[0-9]+$/, "", n); return n }

FNR == 1 { side++ }

/^# / {
  line = $0; sub(/^# /, "", line)
  if (line ~ /^json:/) { next }
  p = index(line, ":"); if (p == 0) next
  k = trim(substr(line, 1, p - 1))
  v = trim(substr(line, p + 1))
  hdr[side, k] = v
  hkeys[k] = 1
  next
}

/^Benchmark/ {
  name = norm($1)
  names[name] = 1
  for (i = 3; i + 1 <= NF; i += 2) {
    metric = $(i + 1)
    if (!(metric in dir)) continue
    val[side, name, metric] = $i + 0
    metricset[name, metric] = 1
  }
  next
}

END {
  # --- 元数据 ---
  print "-- 元数据 --"
  split("mode go os/arch cpu mem zstd config benchtime count rows rows1m git", kk, " ")
  meta_bad = 0
  for (i in kk) {
    k = kk[i]
    a = hdr[1, k]; b = hdr[2, k]
    if (a == "" && b == "") continue
    if (a != b) {
      printf "  DIFF  %-10s %s  ->  %s\n", k, (a == "" ? "-" : a), (b == "" ? "-" : b)
      if (k != "git") meta_bad = 1
    }
  }
  if (meta_bad) print "  ! 环境/配置不一致，数值差异不能直接归因于代码变更"
  print ""

  # --- 指标 ---
  nreg = 0; nimp = 0; nok = 0; nskipped = 0
  printf "-- 指标（仅列 |Δ| >= %s%%；VERBOSE=1 显示全部）--\n", thr
  for (name in names) {
    for (k in metricset) {
      split(k, parts, SUBSEP)
      if (parts[1] != name) continue
      metric = parts[2]
      # 报告型基准（Latency 的 p50ns、Env 的 ratio/blocks）的 ns/op 是无意义的兜底值。
      if (metric == "ns/op" && (((name, "p50ns") in metricset) || ((name, "ratio") in metricset) || ((name, "blocks") in metricset))) continue
      if (!((1, name, metric) in val) || !((2, name, metric) in val)) { nskipped++; continue }
      o = val[1, name, metric]; n = val[2, name, metric]
      if (o == 0 && n == 0) { nok++; continue }
      if (o == 0) {  # 百分比未定义：0 -> 非 0 对小为优的指标即回退
        if (dir[metric] < 0) {
          nreg++
          printf "  REGRESS  %-52s %-10s %12.4g -> %-12.4g     n/a\n", name, metric, o, n
        } else {
          nimp++
        }
        continue
      }
      d = (n - o) / o * 100
      # dir: -1 = 越小越好, +1 = 越大越好
      worse = (dir[metric] < 0 && d > thr) || (dir[metric] > 0 && d < -thr)
      better = (dir[metric] < 0 && d < -thr) || (dir[metric] > 0 && d > thr)
      if (worse) nreg++
      else if (better) nimp++
      else nok++
      if (!worse && !better && verbose != "1") continue
      tag = worse ? "REGRESS" : (better ? "IMPROVE" : "OK     ")
      printf "  %s  %-52s %-10s %12.4g -> %-12.4g %+8.1f%%\n", tag, name, metric, o, n, d
    }
  }
  print ""
  printf "-- 汇总: 回退 %d · 改进 %d · 阈值内 %d · 缺测 %d --\n", nreg, nimp, nok, nskipped
  if (meta_bad) print "警告: 两次基线环境/配置不一致"
  exit (nreg > 0 ? 1 : 0)
}

BEGIN {
  # 方向：-1 越小越好，+1 越大越好；未列出的指标不参与对比。
  dir["ns/op"] = -1; dir["B/op"] = -1; dir["allocs/op"] = -1
  dir["p50ns"] = -1; dir["p95ns"] = -1; dir["p99ns"] = -1
  dir["rawB/op"] = -1; dir["readB/op"] = -1; dir["rssdMB"] = -1
  dir["blocks"] = -1; dir["rawB/row"] = -1
  dir["bytePerRow"] = -1; dir["dataMB"] = -1; dir["idxMB"] = -1; dir["ratio"] = -1
  dir["kget/s"] = 1; dir["krows/s"] = 1; dir["MB/s"] = 1
  dir["hitpct"] = 1; dir["scanhitpct"] = 1
}
' "$old_file" "$new_file"
