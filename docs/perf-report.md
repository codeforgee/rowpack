# RowPack 性能测试报告

> 文档状态：**统一基线（v2 单文件格式 Tier 0）**
> 日期：2026-09-08（当前基线）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon M1 Pro) / klauspost/compress v1.20.0 (zstd)
> 说明：**本报告 §1 只有一组当前基线**——由统一矩阵 `make bench` 生成，完整输出
> 落在 `docs/bench-results.txt`（gitignore，机器相关，可随时复现）。历史 v1 对照
> 批次已随 v1 格式废弃移除。

## 1. 当前统一基线（v2 Tier 0，2026-09-08）

单条命令复现全部基线（`make bench`，默认 `-benchtime=3x -count=1`；可用
`BENCHTIME`/`BENCHCOUNT` 覆盖）：

```sh
make bench   # => go test -bench 'Benchmark(Env|MainMatrix|Latency)' -benchmem -count=1
```

- `BenchmarkEnv`：运行时自描述环境 + 真实数据集几何（100k × 7 列，bs=256K：
  ratio=0.168，单文件 5.8 MB——**v2 口径含内嵌 IndexTxn**；v1 同口径为
  data 2.0 MB + .rpi ≈4.1 MB ≈ 6.1 MB，v2 总落盘更小）。
- `BenchmarkMainMatrix`：64 个子测试覆盖 写/读/扫描/链/打开/索引重建 场景 ×
  BlockSize (64K/256K/1M) × 缓存(hot/cold) × 持久化(sync/async) × I/O
  (mmap/readat)，读场景带预热。`index_rebuild` 场景为 v2 语义：IndexTxn 位腐
  后重开的内存重建（文件永不改写）。
- `BenchmarkLatency`：固定 4096 样本点读 p50/p95/p99（此子测试只看延迟列）。

以下为 256K / mmap / sync / 热缓存档位的参考值（完整矩阵见
`docs/bench-results.txt`）：

| 基准（256K/mmap/sync 档） | 结果 |
| --- | --- |
| FULL 顺序写 | 1033 krows/s · 96.9 ms/op |
| 隔离写（单行事务） | 1939 krows/s · ratio 0.041 |
| Get 热读 | 819 ns/op · 1221 kget/s · 99.03% 命中 · 2 allocs |
| Get 热读（复用 dst） | 2.93 µs/op（含一次首块解压的摊销） |
| Get 冷读 | 250 µs/op · 328 KB/op（整块解压） |
| Scan 100k（热/冷） | 13.7 ms / 24.0 ms · 7312 / 4173 krows/s |
| Get / Scan DeepChain（32 层） | 4.9 µs / 32.3 ms |
| Open 索引重放 | 5.46 ms |
| IndexTxn 损坏重开（内存重建） | 21.97 ms（文件不改写） |
| 点读延迟 p50/p95/p99（热） | 250 / 292 / 584 ns |
| 点读延迟 p50/p95/p99（冷） | 281 / 360 / 593 µs |

## 2. 与 v1（双文件）基线的对照（历史记录，v1 已废弃）

同机双分支顺序执行（v1 = main，v2 = 本分支，`-benchtime=10x`，小样本看趋势）：

| 维度 | v1 | v2 | 变化 |
| --- | --- | --- | --- |
| FULL 顺序写 | 84.4 krows/s（两次 fsync） | 96.4 krows/s（一次 fsync） | **+14%** |
| Open 索引重放 | 7.34 ms（.rpi 整读） | 5.72 ms（按区段读） | **+28%** |
| Scan 100k | 16.0 ms | 14.8 ms | +8% |
| Get 热/冷读 | 持平 | 持平 | ±5% 噪声内 |
| 落盘总量（2000 行样本） | 108,673 B / 2 文件 | 108,593 B / **1 文件** | 备份 = 拷贝单文件 |
| 批量冷读（1000 连续行） | 260 ms（Get 循环） | 0.62 ms（迭代器） | **~417×** |

## 3. 阅读注意

- `-benchtime=3x` 样本量小：ns/op 看量级与相对变化，不看绝对值；写场景首迭代
  含建库成本，矩阵已用 `b.ResetTimer` 前置预热读路径。
- `dataMB`/`bytePerRow` 在 v2 是**单文件口径**（含内嵌 IndexTxn），与 v1 的
  data/index 分列不可直接相加比较。
- 峰值 RSS（rssdMB）为本子测试归属的进程增量；mmap 页不计入 RSS 属预期。
- `index_rebuild` 与 v1 的 RebuildIndex 不同：v2 无独立索引文件，重建只发生在
  打开时且仅在 IndexTxn 校验失败的那一个快照上，结果只进内存。
