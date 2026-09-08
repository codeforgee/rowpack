# RowPack V1 性能基线整理 & V2 对比（实机复测）

> 状态：V1 基线（v1.2 P1）实机复测 + V1↔V2 同机对比
> 日期：2026-09-08（当日主会话内连续执行）
> Commit：**V1 = `2673fb4`**（v1.2 P1，与 perf-report-v2.md 引用的 v1 基线
> `bb17f98` 代码相同——`bb17f98`/`9fd27e8`/`d77e446` 之间仅 docs）；**V2 = `5764a25`**（main HEAD）
> 环境：Go 1.27.0 / darwin arm64（Apple M1 Pro 8 核，后台有 IDE/浏览器负载）/ klauspost zstd v1.20.0
> 数据：100,000 行 × 7 列（uint64+string+bool+int32+float64+datetime+decimal(scale=2)）
> 方法：同机同会话 V2→V1 顺序执行；`go test -run '^$' -bench 'Benchmark(MainMatrix|Latency)' -benchtime=1s -benchmem -count=5 -v`；
> benchstat 对比；正文数值为 5 次计数的中位数。原始输出：`/tmp/v2-1s-main.txt`、`/tmp/v1-matrix-1s.txt`。

## 0. 一句话结论

V2 相对 V1 在 **写路径**（-12% ~ -19% FULL / -21% ~ -32% 隔离写）、Open 索引重放（-66% ~ -69%）、
落盘（-65.6%，2 文件 6.1 MB → 1 文件 2.1 MB）三项上有结构性优势；读路径全线持平或小幅
更快（冷读 -1.3% ~ -4.3%，热读 -1.6% ~ -3.1%，统计显著），**没有回退项**；写路径 B/op -11% ~ -37%。

## 1. 测量口径与注意事项

- **为什么不用 `make bench` 默认 `-benchtime=3x`**：亚微秒基准（Get 热读等）在 3 次迭代/计数下
  首迭代的 page fault/GC 会污染均值（实测同子测试 5 个计数从 667ns 到 4708ns 抖动）。本次统一
  用 `-benchtime=1s`（每个计数 ≥1s 稳定采样），重场景（写/1M/链）单次执行时间足够，同样受益。
- **BenchmarkEnv 陷阱**：该基准忽略 `b.N`，在 wall-time benchtime 下驱动循环自动倍增迭代到
  `1e9` 上限（一次全矩阵跑出 2 万行重复日志）；报告附录不含它，环境信息见 §2。下次全矩阵
  复跑请对 BenchmarkEnv 单独用 `-benchtime=3x` 或 `-bench 'Benchmark(MainMatrix|Latency)'` 排除之。
- **BenchmarkLatency 的 ns/op 是驱动伪影**（子基准固定 4096 次采样、忽略 b.N，报出 1e9 次/
  0.5ns 的假值），**只读其 p50/p95/p99**（内部 `measureLatency` 固定样本，稳定可信）。
- **rebuild ↔ index_rebuild 语义不同**：V1 `rebuild` = `.rpi` 缺失后 RebuildIndex **落盘重建**索引文件
  （27–30ms / 68MB）；V2 `index_rebuild` = 内嵌 IndexTxn 位腐后 **打开时内存重建**（17.4–18.0ms /
  31.6MB，文件永不改写）。两者不可直接比较，分别列示。
- **dataMB 口径**：V1 只统计 `.rpk`（1.990MB），索引在独立 `.rpi`；V2 为单文件口径（1.995MB，
  含内嵌 IndexTxn）。可见 V2 单文件 ≈ V1 数据文件 + 5KB，而省掉了 4MB 的 `.rpi`。
- 样本 n=5，benchstat 95% CI 需 ≥6 样本处的 `±∞` 属预期；显著性以 p 值（<0.05 记显著）判断，
  附录完整列出。

## 2. V1 基线（2673fb4 实测值）

> 环境行（BenchmarkEnv）：`go=go1.27.0 zstd=v1.20.0 os=darwin/arm64 cacheBytes=67108864
> blockSize=262144 dataset=100k rows x 7 cols dataMB=2.0 ratio=0.168 bytePerRow=20.9 indexMB=2.3`

参考档（256K / mmap / sync，5 计数中位数；async 与 readat 见附录）：

| 基准 | V1 结果 | 吞吐/备注 |
| --- | --- | --- |
| FULL 顺序写（sync） | 94.39 ms/op | 1059 krows/s · 121.1 MiB/op · 501K allocs |
| FULL 顺序写（async） | 83.48 ms/op | 1198 krows/s |
| 隔离写（sync，100k 行单行事务） | 49.05 ms/op | 2039 krows/s · 39.8 MiB/op · 1.44K allocs |
| 隔离写（async） | 38.39 ms/op | 2605 krows/s |
| Get 热读 | 387.7 ns/op | 2579 kget/s · 2 allocs · 104 B/op · 99.0–100% 命中 |
| Get 热读（复用 dst） | 393.7 ns/op | 2539 kget/s · 2 allocs · 116 B/op |
| Get 冷读（缓存关，256K 块） | 266.8 µs/op | 整块解压 320.7 KiB/op · 6 allocs |
| Scan 100k（热/冷） | 13.79 ms / 24.24 ms | 7254 / 4126 krows/s · 171 allocs · 2.66 MiB |
| Scan 1M | 219.4 ms | 4557 krows/s |
| 随机读 1M（缓存受限） | 133.4 µs/op | 8 allocs |
| Get DeepChain（32 层） | 2.41 µs/op | 5 allocs · 99.96% 命中 |
| Scan DeepChain（132k 行） | 28.14 ms | 4695 krows/s |
| Open 索引重放（100k 行） | 5.39 ms/op | 27.4 MiB/op · 447 allocs |
| RebuildIndex（.rpi 缺失重建） | 29.2 ms/op | 68.3 MiB/op · 2.23K allocs |
| 点读延迟 p50/p95/p99（热，256K） | 250 / 292 / 583 ns | 4096 样本 |
| 点读延迟 p50/p95/p99（冷） | 281 / 345 / 569 µs | 4096 样本 |
| 点读延迟 p50/p95/p99（32 层链） | 2.00 / 3.63 / 5.83 µs | 4096 样本 |
| 落盘（100k 行，zip 后） | `.rpk` 2,086,959 B + `.rpi` 4,002,984 B | **2 文件 6,089,943 B（5.81 MB）** |
| 压缩率 / 索引常驻 | ratio 0.168 / idxMB 2.29（100k）、22.92（1M） | zstd |

与历史记录对照：v1.1+ 末批（docs/benchmarks-v1.md）的 Get 热读 ~330–388ns、Scan 100k 15.6ms、
OpenReplay 5.1ms、RebuildIndex 27.8ms 与本次实测一致量级（本机当日负载略低，略有偏差属正常）。
docs/perf-report-v2.md 旧 v1 对照值（118.5ms FULL / 7.34ms Open / 16.0ms Scan @10x）均在本机常态
波动范围内，趋势一致。

## 3. V1 ↔ V2 对比（5764a25 vs 2673fb4）

### 3.1 耗时（参考档 256K/mmap/sync，中位数；全矩阵见附录）

| 场景 | V1 | V2 | 变化 | 显著 |
| --- | --- | --- | --- | --- |
| FULL 顺序写 | 94.39 ms | 79.40 ms | **-15.9%**（吞吐 +19%） | ✓ p=0.008 |
| 隔离写 | 49.05 ms | 34.94 ms | **-28.8%**（吞吐 +40%） | ✓ p=0.008 |
| Get 热读 / 复用 | 387.7 / 393.7 ns | 377.3 / 388.1 ns | -2.7% / -1.4% | ✓（reuse ≈） |
| Get 冷读 | 266.8 µs | 255.4 µs | -4.3% | ✓ p=0.008 |
| Scan 100k 热/冷 | 13.79 / 24.24 ms | 13.47 / 23.60 ms | -2.3% / -2.6% | ✓ p=0.008 |
| Scan 1M | 219.4 ms | 218.9 ms | -0.2% | ≈ |
| 随机读 1M（bs=1M 档） | 591.8 µs | 571.1 µs | -3.5% | ✓ p=0.008 |
| Get DeepChain（256K 档） | 2.41 µs | 2.06 µs | -14.5% | ✓ p=0.008（仅 256K 档） |
| Scan DeepChain | 28.14 ms | 27.29 ms | -3.0% | ✓ p=0.008 |
| **Open 索引重放** | 5.39 ms | 1.65 ms | **-69.5%** | ✓ p=0.008 |
| 并发热读 g8/g64（mmap） | 483 / 483 ns | 453 / 447 ns | -6.2% / -7.5% | ✓（64 路持平 readat） |
| 全矩阵 geomean（64 子测试） | 360.4 µs | 313.5 µs | **-11.6%** | 见附录 |

### 3.2 分配（B/op / allocs/op，中位数）

| 场景 | V1 | V2 | 变化 |
| --- | --- | --- | --- |
| write_full B/op | 121.1 MiB | 107.1 MiB | **-11.6%** |
| write_iso B/op | 39.8 MiB | 25.3 MiB | **-36.5%** |
| write_iso allocs | 1.44K | 1.31K | **-9.0%** |
| open_replay B/op | 27.4 MiB | 6.6 MiB | **-75.8%**（按区段读 vs 整 .rpi ReadAll） |
| open_replay allocs | 447 | 424 | -3.5% |
| Get 热读 B/op/allocs | 104 B / 2 | 104 B / 2 | 持平 |
| Get 冷读 | 320.7 KiB / 6 | 320.7 KiB / 6 | 持平 |
| Scan 100k 热/冷 | 2.66–2.90 MiB / 171–297 | 同 | 持平（-1.5% 冷 B/op） |
| Get DeepChain allocs | 5 | 4 | -1 |
| scan1m / getrand1m | 35.2 MiB / 158 KiB | 同 | 持平 |

### 3.3 延迟分位数（BenchmarkLatency，4096 样本/次 × 5 计数中位数）

| 场景 | V1 p50/p95/p99 | V2 p50/p95/p99 | 结论 |
| --- | --- | --- | --- |
| 热读 | 250 / 292 / 541–625 ns | 250 / 292 / 417–458 ns | 持平（p99 略好） |
| 热读复用 | 250 / 292 / 583–666 ns | 250 / 292 / 375–666 ns | 持平 |
| 冷读 | 281 / 345 / 569 µs | 275 / 314 / 505 µs | **p50 -2%、p95 -9%、p99 -11%** |
| 32 层链 | 2.00 / 3.63 / 5.83 µs | 1.96 / 3.50–4.58 / 5.67–6.04 µs | 持平（噪声内） |

### 3.4 落盘（100k 行实测，256K / zstd / sync）

| | V1 | V2 |
| --- | --- | --- |
| 文件 | `.rpk` 2,086,959 B + `.rpi` 4,002,984 B（2 文件） | `.rpk` 2,092,129 B（**1 文件**） |
| 合计 | 6,089,943 B（5.81 MB） | 2,092,129 B（2.00 MB，**-65.6%**） |
| 说明 | 索引独立文件占 65.7% | IndexTxn 内嵌，备份 = 拷贝单文件 |

注：V2 单文件比 V1 数据文件仅大 5,170B（+0.25%，内嵌 IndexTxn；附录 dataMB 列也可见
1.990→1.995）；对 2000 行小样本（docs/perf-report-v2.md §2）两者总字节持平，随索引占比
增大，单文件优势放大。

### 3.5 语义/能力差异（不参与耗时对比）

- RebuildIndex（V1，落盘重建）：27–30 ms / 68 MB / 2.23K allocs
- IndexTxn 损坏重开内存重建（V2，文件不改写）：17.4–18.0 ms / 31.6 MB / 1.75K allocs
- 批量读取（M4 BatchIterator）为 v2 新增能力，v1 无对应基准（PerfReport-V2 §3：~417×）。

## 4. 结论与建议

1. **V2 满足"单行 Get 回退 ≤10%"验收**：热读 -1.6% ~ -3.1%（部分 ≈），冷读 -4.3%，均为正向。
2. V2 结构性收益来自单文件事务（一次 fsync）与按区段读 IndexTxn；其余读路径无回退。
3. V1 基线已冻结为：写 FULL 94.4ms、隔离写 49.0ms、热读 388ns、冷读 267µs、Scan 13.8/24.2ms、
   Open 5.39ms、落盘 6.09MB/2 文件（2673fb4）。
4. 后续复跑对比建议固定 `-benchtime=1s -count=5`（并排除 BenchmarkEnv），与本文档口径一致。

## 附录 A. 全矩阵 sec/op（V1 vs V2，benchstat，5 计数）

> `~` = 无显著差异（p≥0.05）；"变化"为 V2 相对 V1 的百分比，负为更快；
> Latency/* 的 ns/op 为驱动伪影（见 §1），rebuild/index_rebuild 语义不同（见 §3.5），均不入表。

| 基准 | V1 (2673fb4) | V2 (5764a25) | 变化 | p 值 |
| --- | --- | --- | --- | --- |
| MainMatrix/get_hot/bs=64K/io=mmap-8 | 384.5n | 378.3n | -1.61% | p=0.032 |
| MainMatrix/get_hot_into/bs=64K/io=mmap-8 | 394.6n | 390.4n | ~ | p=0.222 |
| MainMatrix/get_cold/bs=64K/io=mmap-8 | 65.63µ | 64.06µ | -2.40% | p=0.016 |
| MainMatrix/scan/bs=64K/cache=hot/io=mmap-8 | 13.73m | 13.46m | -1.95% | p=0.008 |
| MainMatrix/scan/bs=64K/cache=cold/io=mmap-8 | 23.59m | 22.98m | -2.58% | p=0.008 |
| MainMatrix/get_hot/bs=256K/io=mmap-8 | 387.7n | 377.3n | -2.68% | p=0.008 |
| MainMatrix/get_hot_into/bs=256K/io=mmap-8 | 393.7n | 388.1n | ~ | p=0.095 |
| MainMatrix/get_cold/bs=256K/io=mmap-8 | 266.8µ | 255.4µ | -4.29% | p=0.008 |
| MainMatrix/scan/bs=256K/cache=hot/io=mmap-8 | 13.79m | 13.47m | -2.29% | p=0.008 |
| MainMatrix/scan/bs=256K/cache=cold/io=mmap-8 | 24.24m | 23.60m | -2.64% | p=0.008 |
| MainMatrix/get_hot/bs=1M/io=mmap-8 | 390.0n | 377.8n | -3.13% | p=0.008 |
| MainMatrix/get_hot_into/bs=1M/io=mmap-8 | 391.7n | 388.4n | ~ | p=0.151 |
| MainMatrix/get_cold/bs=1M/io=mmap-8 | 1.175m | 1.159m | ~ | p=0.310 |
| MainMatrix/scan/bs=1M/cache=hot/io=mmap-8 | 13.77m | 13.46m | -2.25% | p=0.016 |
| MainMatrix/scan/bs=1M/cache=cold/io=mmap-8 | 24.82m | 24.55m | -1.09% | p=0.008 |
| MainMatrix/conc_get_g8/io=mmap-8 | 482.7n | 452.7n | -6.22% | p=0.032 |
| MainMatrix/conc_get_g64/io=mmap-8 | 482.9n | 446.9n | -7.45% | p=0.008 |
| MainMatrix/get_hot/bs=64K/io=readat-8 | 389.8n | 380.0n | -2.51% | p=0.008 |
| MainMatrix/get_hot_into/bs=64K/io=readat-8 | 401.9n | 392.0n | -2.46% | p=0.008 |
| MainMatrix/get_cold/bs=64K/io=readat-8 | 70.80µ | 68.49µ | -3.27% | p=0.008 |
| MainMatrix/scan/bs=64K/cache=hot/io=readat-8 | 13.79m | 13.54m | -1.75% | p=0.008 |
| MainMatrix/scan/bs=64K/cache=cold/io=readat-8 | 24.42m | 23.90m | -2.12% | p=0.008 |
| MainMatrix/get_hot/bs=256K/io=readat-8 | 389.4n | 380.2n | -2.36% | p=0.016 |
| MainMatrix/get_hot_into/bs=256K/io=readat-8 | 392.3n | 390.5n | ~ | p=0.095 |
| MainMatrix/get_cold/bs=256K/io=readat-8 | 268.9µ | 265.4µ | -1.30% | p=0.008 |
| MainMatrix/scan/bs=256K/cache=hot/io=readat-8 | 13.77m | 13.53m | -1.79% | p=0.008 |
| MainMatrix/scan/bs=256K/cache=cold/io=readat-8 | 24.97m | 24.17m | -3.19% | p=0.008 |
| MainMatrix/get_hot/bs=1M/io=readat-8 | 386.7n | 381.0n | ~ | p=0.333 |
| MainMatrix/get_hot_into/bs=1M/io=readat-8 | 395.4n | 395.5n | ~ | p=1.000 |
| MainMatrix/get_cold/bs=1M/io=readat-8 | 1.196m | 1.199m | ~ | p=0.841 |
| MainMatrix/scan/bs=1M/cache=hot/io=readat-8 | 13.59m | 13.67m | ~ | p=0.151 |
| MainMatrix/scan/bs=1M/cache=cold/io=readat-8 | 25.20m | 25.13m | ~ | p=0.690 |
| MainMatrix/conc_get_g8/io=readat-8 | 474.5n | 446.4n | ~ | p=0.095 |
| MainMatrix/conc_get_g64/io=readat-8 | 469.1n | 470.5n | ~ | p=0.841 |
| MainMatrix/write_full/bs=64K/dur=sync-8 | 96.47m | 80.87m | -16.17% | p=0.008 |
| MainMatrix/write_iso/bs=64K/dur=sync-8 | 52.14m | 37.30m | -28.47% | p=0.008 |
| MainMatrix/write_full/bs=64K/dur=async-8 | 86.00m | 73.95m | -14.01% | p=0.008 |
| MainMatrix/write_iso/bs=64K/dur=async-8 | 41.37m | 32.05m | -22.53% | p=0.008 |
| MainMatrix/scan1m/bs=64K-8 | 219.2m | 219.4m | ~ | p=1.000 |
| MainMatrix/getrand1m/bs=64K-8 | 34.28µ | 34.59µ | ~ | p=0.056 |
| MainMatrix/get_deepchain/bs=64K-8 | 2.041µ | 2.090µ | ~ | p=0.421 |
| MainMatrix/scan_deepchain/bs=64K-8 | 27.38m | 27.58m | ~ | p=0.548 |
| MainMatrix/open_replay/bs=64K-8 | 5.301m | 1.817m | -65.73% | p=0.008 |
| MainMatrix/write_full/bs=256K/dur=sync-8 | 94.39m | 79.40m | -15.88% | p=0.008 |
| MainMatrix/write_iso/bs=256K/dur=sync-8 | 49.05m | 34.94m | -28.77% | p=0.008 |
| MainMatrix/write_full/bs=256K/dur=async-8 | 83.48m | 73.72m | -11.69% | p=0.008 |
| MainMatrix/write_iso/bs=256K/dur=async-8 | 38.39m | 30.36m | -20.92% | p=0.008 |
| MainMatrix/scan1m/bs=256K-8 | 219.4m | 218.9m | ~ | p=0.690 |
| MainMatrix/getrand1m/bs=256K-8 | 133.4µ | 132.9µ | ~ | p=0.095 |
| MainMatrix/get_deepchain/bs=256K-8 | 2.412µ | 2.062µ | -14.51% | p=0.008 |
| MainMatrix/scan_deepchain/bs=256K-8 | 28.14m | 27.29m | -3.00% | p=0.008 |
| MainMatrix/open_replay/bs=256K-8 | 5.394m | 1.647m | -69.46% | p=0.008 |
| MainMatrix/write_full/bs=1M/dur=sync-8 | 96.99m | 78.62m | -18.94% | p=0.008 |
| MainMatrix/write_iso/bs=1M/dur=sync-8 | 50.54m | 34.34m | -32.04% | p=0.008 |
| MainMatrix/write_full/bs=1M/dur=async-8 | 86.51m | 72.83m | -15.82% | p=0.008 |
| MainMatrix/write_iso/bs=1M/dur=async-8 | 38.90m | 30.13m | -22.55% | p=0.008 |
| MainMatrix/scan1m/bs=1M-8 | 232.3m | 227.2m | -2.21% | p=0.008 |
| MainMatrix/getrand1m/bs=1M-8 | 591.8µ | 571.1µ | -3.50% | p=0.008 |
| MainMatrix/get_deepchain/bs=1M-8 | 2.086µ | 2.065µ | ~ | p=1.000 |
| MainMatrix/scan_deepchain/bs=1M-8 | 28.08m | 27.34m | -2.62% | p=0.008 |
| MainMatrix/open_replay/bs=1M-8 | 5.274m | 1.777m | -66.31% | p=0.008 |

## 附录 B. 复现命令

```sh
# V1（先切到提交）
git checkout 2673fb4
go test -run '^$' -bench 'Benchmark(MainMatrix|Latency)$' -benchtime=1s -benchmem -count=5 -v . | tee v1-results.txt

# V2
git checkout 5764a25   # 或 main
go test -run '^$' -bench 'Benchmark(MainMatrix|Latency)$' -benchtime=1s -benchmem -count=5 -v . | tee v2-results.txt

# 对比
benchstat v1-results.txt v2-results.txt
```

> 注：`make bench`（3x）仍是项目默认快照工具，与本文档 1s 口径同向但数值略抖，
> 复跑基线请以附录 B 为准。