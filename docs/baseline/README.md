# S0 基线归档（格式重构前冻结基线）

> 冻结日期：2026-09-09 · Apple M1 Pro · Go 1.27.0 darwin/arm64 · zstd v1.20.0
> 对应计划：`FILE_FORMAT_REFACTOR_PLAN.md` §12 阶段 0 / `REFACTOR_EXECUTION_PLAN.md` §3

## 文件

| 文件 | 内容 | 复现命令 |
| --- | --- | --- |
| `bench-s0-full.txt` | `make bench` 完整输出（矩阵 + 直读档 + 内存档） | `make bench` |
| `bench-s0-variance.txt` | 关键指标 ×5 次方差档 | 见下 |
| `bench-results-10m.txt` | 10M 行 Open 内存档 | `make bench-10m` |
| profile `*.out` | CPU/heap profile（gitignore，不入库） | `make bench-profile` |

```sh
# 方差档（本文件记录的 5 次连续运行）
go test -run '^$' -bench 'Benchmark(WriteFull|GetHot|GetCold|GetColdUnpooled|Scan|OpenReplay|OpenMemory)$' \
  -benchtime=1s -benchmem -count=5 .
# 10M 行 Open 内存档
make bench-10m
# profile（产物 *.out 被 gitignore，查看：go tool pprof docs/bench-getcold-cpu.out）
make bench-profile
```

## 指标口径（双口径定义，禁止混用）

- **readB/op**：单次冷读从文件拉取的字节（header + stored 密文/压缩字节）。
- **rawB/op**：单次冷读解压产生的 raw 字节——读取放大口径，S0 实测 327,095 B/op
  ≈ 319 KiB，即读一行也要解压并校验整个 256 KiB Block。这是 S2 Page 化的直接目标。
- **B/op**：分配口径。池化态（BenchmarkGetCold）≈ 稳态摊销；无池态
  （BenchmarkGetColdUnpooled，`block.SetPoolDisabled` 旁路 scratch 池）= 每读临时分配，
  S0 实测 328,645 B/op ≈ 321 KiB。§3.1 门槛"≤ 64 KiB/op"以此口径度量。
- **idxB/row**：Eager Row Index 常驻内存/行（`Stats.IndexMemoryBytes` / 行数）。
  §3.1 门槛 ≤ 16，期望 ~12；Lazy ≤ 0.25。

## 冻结数值（256 KiB / mmap / sync 档，100k 行 × 7 列）

| 指标 | 数值 | 备注 |
| --- | --- | --- |
| FULL 顺序写 | 104.2 ms/次提交 ≈ 95.99 万行/s | `BenchmarkWriteFull` |
| 热点读 | 277 ns/op · 16 B/op · 2 allocs | `BenchmarkGetHot` |
| 冷点读（池化） | 341.4 µs/op · 318 B/op · 6 allocs | `BenchmarkGetCold` |
| 冷点读（无池） | 349.3 µs/op · **328,645 B/op** · 8 allocs | `BenchmarkGetColdUnpooled` |
| 冷读 readB/op / rawB/op | 62,843 / 327,095 B | 读取放大 ≈ 319 KiB raw |
| Scan 100k | 13.6 ms · 134 allocs | 0 alloc/row 已由 strArena 达成 |
| Open 索引重放 100k | 1.80 ms · 6.95 MB/op | `BenchmarkOpenReplay` |
| Open 1M | 18.3 ms · 68.7 MB/op | `BenchmarkOpenMemory` |
| Open 10M | 222.6 ms · **2.02 GB/op** 临时分配 | `make bench-10m`，S8.3 峰值目标重点 |
| Eager 索引常驻 | **24.03 B/row**（22.91 MiB @1M / 229.1 MiB @10M，线性） | `BenchmarkOpenMemory` idxB/row |
| 冷读 CPU 构成 | zstd 解码 51.5% + CRC32C 18.2% | `bench-getcold-cpu.out` |

## 方差（5 次连续，同机）

| 基准 | 区间 | 相对波动 |
| --- | --- | --- |
| WriteFull | 103.5–107.0 ms | ±1.7% |
| GetHot | 272.3–273.0 ns | ±0.13% |
| GetCold | 329.8–333.0 µs | ±0.5% |
| GetColdUnpooled | 343.3–345.8 µs（B/op 恒定） | ±0.4% |
| Scan | 13.65–14.28 ms | ±2.2% |
| OpenReplay | 1.62–1.88 ms | ±3.7% |
| OpenMemory | 18.1–18.4 ms（idxB/row 恒定 24.03） | ±0.8% |

结论：全部主要指标方差 ≤ ±3.7%，分配类指标逐字节恒定，基线可作验收对照物。

## 固定数据集几何（benchspec_test.go）

| 几何 | 用途 |
| --- | --- |
| `geomMixed` | 顺序 RowID + 小负载（历史口径，全部直读基准） |
| `geomRandomID` | 固定种子乱序 RowID（文件大小 +15% 约束、索引排序） |
| `geomBigBytes` | 4 KiB Bytes 列（arena/拷贝路径） |
| `geomOversized` | 320 KiB 单行（超大行 Page，独占 Block） |

所有几何共用 7 列 schema 与固定种子（`benchSeed`）；新基准禁止临时拼数据。
