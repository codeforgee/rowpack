# RowPack v1 性能基线

> 冻结日期：2026-09-10
> 归档运行：`testdata/baseline/2026-09-10.txt`（`make baseline` 生成，含统一环境标注）
> 格式：v1 单文件（Magic `ROWPACK1`，Major 1，Minor 0）
> 运行器：`scripts/baseline.sh` · 入口 `make baseline [MODE=full|quick|1m]`
> 状态：**v1 首个性能基线**；后续格式/算法变更需与本表对照，回退须有说明

## 0. 如何建立新基线

```sh
make baseline                    # 标准档（MODE=full），输出 testdata/baseline/<YYYY-MM-DD>.txt
make baseline MODE=quick         # 20k 行冒烟（~15s），不与基线比数值
make baseline MODE=1m            # 1M 行档（scan1m/getrand1m）
BASELINE_LABEL=m1pro make baseline   # 文件名加标签：<date>-m1pro.txt
BENCHTIME=2s BENCHCOUNT=5 make baseline   # 覆盖时长/重复次数

# 对比两个归档，标出超过阈值的回退/改进（有回退时退出码 1，可作为 CI 闸门）
make baseline-diff OLD=2026-09-10 NEW=2026-09-11
THRESHOLD=5 VERBOSE=1 make baseline-diff OLD=2026-09-10 NEW=2026-09-11
```

- 输出直接写入 `testdata/baseline/`，**按日期命名**，同名自动追加 `-2/-3…`，不覆盖历史；
- 每个文件开头固定写入统一标注：日期 / git rev（dirty 标记）/ Go / OS-arch / CPU / 内存 /
  zstd 版本 / 格式版本 / 基准参数 / 数据集行数 / 复现命令，随后是完整 `go test -bench` 输出；
- 标注最后一行是机器可读的 `# json: {...}`，供 `baseline-diff` 做元数据一致性校验与脚本化解析；
- `baseline-diff` 按指标方向（ns/op·B/op·p50/p95/p99 越小越好，kget/s·krows/s·MB/s·hitpct 越大越好）
  计算变化率，并校验两次运行的 go/CPU/config/rows 等字段是否一致；
- 归档文件纳入版本库，作为该日期的可比基线；`bench/results*.txt` 仍是可丢弃的临时输出。

## 1. 环境与口径（2026-09-10 运行）

| 项 | 值 |
| --- | --- |
| 机器 | Apple M1 Pro · 16 GiB |
| OS / Arch | darwin / arm64 |
| Go | go1.27.0 |
| 压缩 | klauspost/compress zstd v1.20.0（默认 level 3） |
| 文件格式 | 单文件 `.rpk`，内嵌 IndexTxn（排序 Row Index Page + Fence） |
| 默认 BlockSize / PageSize | 256 KiB / 32 KiB |
| 默认 I/O | mmap；另测 ReadAt |
| 默认持久化 | SyncCommit |
| 数据集 | 100k 行 × 7 列（`geomMixed`，确定性负载）；1M 档另标注 |
| 计时 | `-benchtime=1s -benchmem -count=1`（单次，未做方差档） |

吞吐口径：行吞吐 krows/s = 行/秒；点读 kget/s = 次/秒；延迟为 ns/op。

## 2. 默认档关键指标（100k 行）

来源：`BenchmarkMainMatrix` 256K/mmap 档 + 独立套件 `Benchmark*`（默认 mmap、sync）。

| 场景 | 延迟 | 吞吐 | 分配 | 备注 |
| --- | --- | --- | --- | --- |
| FULL 顺序写（sync） | 90.2 ms / 100k | 1109 krows/s | 402k allocs | 压缩比 0.1911 |
| FULL 顺序写（async） | 86.2 ms / 100k | 1160 krows/s | 402k allocs | |
| 同构行写（sync） | 46.5 ms / 100k | 2149 krows/s | 2.2k allocs | 压缩比 0.00639 |
| Get 热读（矩阵，256K） | 363.8 ns | 2748 kget/s | 1 alloc · 16 B | 缓存命中 100% |
| Get 热读（独立套件） | 232.3 ns | ~4305 kget/s | 1 alloc · 16 B | |
| Get 热读 into（矩阵） | 385.9 ns | 2591 kget/s | 1 alloc · 19 B | 复用调用方 dst |
| Get 冷读（矩阵，256K） | 49.75 µs | 20.10 kget/s | 14 allocs · 58 kB/op | rawB/op 32,776（≈1 页） |
| Get 冷读（独立套件） | 52.45 µs | ~19.07 kget/s | 14 allocs | readB/op 5,862 |
| Get 冷读（无池口径） | 52.02 µs | ~19.22 kget/s | 14 allocs | 与池化档差异在噪声内 |
| 并发 Get（64 goroutine） | 361.5 ns | 2766 kget/s | 1 alloc | |
| Scan 热（256K） | 14.35 ms / 100k | 6970 krows/s | 63 allocs | 扫描窗口命中 98.81% |
| Scan 冷（256K） | 25.16 ms / 100k | 3974 krows/s | 1606 allocs | |
| Scan 独立套件 | 14.71 ms / 100k | ~6798 krows/s | 77 allocs | |
| ReadBatch(1000) | 254.6 µs | ~3.93 M rows/s | **4 allocs** | 按块聚合 |
| Get ×1000（逐行基线） | 235.9 µs | ~4.24 M rows/s | 1000 allocs | 批量同延迟、少 250× 分配 |
| Open / 索引重放 | 2.36 ms | — | 382 allocs · 1.59 MB | 100k 行 |
| IndexTxn 损坏内存重建 | 17.59 ms | — | 3088 allocs · 28.8 MB | 不改写文件 |
| 深链（32 层）Get（矩阵） | 1.97 µs | 507.4 kget/s | 3 allocs · 720 B | |
| 深链（32 层）Get（独立） | 985 ns | ~1015 kget/s | 1 alloc · 16 B | |
| 深链（32 层）Scan | 40.10 ms | 3291 krows/s | 265 allocs | |

## 3. 延迟分位数（`BenchmarkLatency`，256K/mmap）

| 场景 | p50 | p95 | p99 |
| --- | --- | --- | --- |
| Get 热读 | 375 ns | 417 ns | 500 ns |
| Get 热读 into | 375 ns | 417 ns | 541 ns |
| Get 冷读 | 51.0 µs | 96.3 µs | 156.5 µs |
| 深链 Get | 1.875 µs | 2.541 µs | 5.792 µs |

## 4. 1M 行档（`ROWPACK_BENCH_ROWS1M=1M`）

| 场景（256K/mmap） | 延迟 | 吞吐 | 文件/索引 |
| --- | --- | --- | --- |
| Scan 1M | 245.3 ms | 4077 krows/s | dataMB 12.93 · idxMB 12.42 |
| 随机 Get 1M | 31.5 µs | 31.70 kget/s | 缓存命中 84.11% |
| 随机 Get 1M（1M Block） | 31.5 µs | 31.71 kget/s | 缓存命中 95.31% |
| 深链 Get（1M 档几何） | 1.97 µs | 507.4 kget/s | |

## 5. BlockSize / I/O 路径对比（100k，mmap）

| 场景 | 64K | 256K | 1M |
| --- | --- | --- | --- |
| Get 热读 | 364.2 ns | 363.8 ns | 363.8 ns |
| Get 冷读 | 49.45 µs | 49.75 µs | 50.45 µs |
| Scan 热 | 14.14 ms | 14.35 ms | 14.58 ms |
| Scan 冷 | 25.22 ms | 25.16 ms | 25.31 ms |
| FULL 写（sync） | 1100 krows/s | 1109 krows/s | 1093 krows/s |
| Open 重放 | 2.41 ms | 2.36 ms | 2.42 ms |

I/O 路径（mmap vs ReadAt）在热读/扫描上差异在噪声内。

## 6. 加密（standalone，**测试配置 BlockSize=1 KiB，不与默认档直接比**）

| 场景 | 值 |
| --- | --- |
| EncryptedWrite | 208.6 ms / 100k · 30.68 MB/s |
| EncryptedGetHot（20k 行） | 275.7 ns · 1 alloc · 17 B |

说明：独立加密基准使用 1 KiB 测试块，其 BlockSize 与默认档不同，不能与上表默认档直接比较；
`ENCRYPTION_V1.md` 的“热读 ≤1%”门槛需在同配置矩阵下复测（当前基线无同配置证据）。

## 7. 文件几何（100k 行 × 7 列，默认档）

| 指标 | 值 |
| --- | --- |
| Rows Block 数 | 27 |
| 文件大小 | 1.235 MB |
| 索引常驻内存 | 1.242 MB |
| 压缩比（存储/原始） | 0.1911 |
| 原始字节/行 | 67.40 B |
| 落盘字节/行 | 12.88 B |

## 8. 与旧基线的对照

本基线与仓库历史 README 参考档一致：FULL 写 ~100–110 万行/秒、Get 热读 ~270 万次/秒
（矩阵档）、Scan 100k ~14 ms。格式 v2→v1 的改动只涉及 Magic/主版本号字节，不改变布局，
因此性能与 v2 冻结时等同。旧 `docs/baseline/`（S0 双口径、方差档、1000 万行 Open 内存档）
已移除；`BenchmarkOpenMemory10M` 基准不存在，故本基线不含 10M Open 内存档。

<details>
<summary>附录：原始输出摘要（testdata/baseline/2026-09-10.txt）</summary>

```
BenchmarkMainMatrix/get_hot/bs=256K/io=mmap-8        363.8 ns/op   2748 kget/s   16 B/op   1 allocs/op
BenchmarkMainMatrix/get_hot_into/bs=256K/io=mmap-8   385.9 ns/op   2591 kget/s   19 B/op   1 allocs/op
BenchmarkMainMatrix/get_cold/bs=256K/io=mmap-8      49750 ns/op     20.10 kget/s  57901 B/op  14 allocs/op
BenchmarkMainMatrix/scan/bs=256K/cache=hot/io=mmap  14.35 ms/op     6970 krows/s  63 allocs/op
BenchmarkMainMatrix/scan/bs=256K/cache=cold/io=mmap 25.16 ms/op     3974 krows/s  1606 allocs/op
BenchmarkMainMatrix/getrand1m/bs=256K                31546 ns/op    31.70 kget/s  8 allocs/op
BenchmarkMainMatrix/scan1m/bs=256K                   245.3 ms/op    4077 krows/s  15474 allocs/op
BenchmarkMainMatrix/open_replay/bs=256K               2.36 ms/op    382 allocs/op
BenchmarkMainMatrix/index_rebuild/bs=256K            17.59 ms/op    3088 allocs/op
BenchmarkLatency/get_hot-8                           375/417/500 ns (p50/p95/p99)
BenchmarkLatency/get_cold-8                          51.0/96.3/156.5 µs
BenchmarkWriteFull-8                                 95.2 ms/op    67.26 MB/s   402138 allocs/op
BenchmarkGetHot-8                                    232.3 ns/op    16 B/op   1 allocs/op
BenchmarkGetCold-8                                   52.45 µs/op    rawB/op 32773  readB/op 5862
BenchmarkGetColdUnpooled-8                           52.02 µs/op    rawB/op 32774
BenchmarkScan-8                                      14.71 ms/op    77 allocs/op
BenchmarkReadBatch1000-8                             254.6 µs/op    4 allocs/op
BenchmarkGetLoop1000-8                               235.9 µs/op    1000 allocs/op
BenchmarkOpenReplay-8                                 2.36 ms/op    382 allocs/op
BenchmarkDeepChainGet-8                              985.2 ns/op    1 allocs/op
BenchmarkEncryptedWrite-8                            208.6 ms/op    30.68 MB/s
BenchmarkEncryptedGetHot-8                           275.7 ns/op    17 B/op   1 allocs/op
```

完整 108 行原始输出见 `testdata/baseline/2026-09-10.txt`。

</details>

## 9. 维护约定

- 修改磁盘布局、编码、压缩级别、缓存策略或索引结构后，必须 `make baseline` 重新归档并更新本表；
- 新旧对比用 `make baseline-diff OLD=<date> NEW=<date>`；热读/冷读/扫描/写入任一核心指标
  回退 >10% 需在提交说明中给出原因（回退 >10% 时 diff 退出码为 1）；
- 本表为单机单次结果，用于相对回归，不用于跨机绝对比较；需要方差档时用
  `BENCHCOUNT=5 make baseline` 重跑，归档文件即方差证据；
- 对照新旧基线时，先看 diff 的「元数据」段：环境不一致时数值差异不能直接归因于代码变更。

SIMD/SWAR 候选内核另有 `make bench-simd` 微基准；设计边界、当前内核和实施路线见
[SIMD_OPTIMIZATION.md](SIMD_OPTIMIZATION.md)。微基准只用于定位内核变化，最终验收仍以本基线为准。
