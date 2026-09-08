# RowPack 性能测试报告

> 文档状态：统一基线（v1.2 Tier 0）
> 日期：2026-09-07（当前基线）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon M1 Pro) / klauspost/compress v1.20.0 (zstd)
> 说明：**本报告 §1 只有一组当前基线**——由统一矩阵 `make bench` 生成，完整输出
> 落在 `docs/bench-results.txt`（含每个子测试的 ns/op、B/op、allocs/op、吞吐、
> 数据文件大小、压缩率、缓存命中率、索引内存与峰值 RSS 增量）。历史批次数据
> 保留在本文末尾 `§2+` 和 docs/benchmarks-v1.md，仅用于回溯，不作为对比基线。

## 1. 当前统一基线（v1.2 Tier 0，2026-09-07）

单条命令复现全部基线（`make bench`，默认 `-benchtime=3x -count=1`；可用
`BENCHTIME`/`BENCHCOUNT` 覆盖）：

```sh
make bench   # => go test -bench 'Benchmark(Env|MainMatrix|Latency)' -benchmem -count=1
```

- `BenchmarkEnv`：运行时自描述环境 + 真实数据集几何（100k × 7 列，bs=256K：
  dataMB=2.0，bytePerRow=20.9，ratio=0.168，indexMB=2.3）。
- `BenchmarkMainMatrix`：64 个子测试覆盖 写/读/扫描/链/打开 场景 × BlockSize
  (64K/256K/1M) × 缓存(hot/cold) × 持久化(sync/async) × I/O(mmap/readat)，
  读场景带 `b.ResetTimer` 前预热，保证低 benchtime 下 per-op 数值不稀释。
- `BenchmarkLatency`：固定 4096 样本点读 p50/p95/p99（此子测试只看延迟列，
  ns/op 与 B/op 列无意义）。

以下为 256K / mmap / sync / 热缓存档位的参考值（完整矩阵见
`docs/bench-results.txt`）：

| 场景（256K/mmap） | 结果 | 说明 |
| --- | --- | --- |
| FULL 顺序写（sync） | 96.9 ms / 100k，1032 krows/s | 含一次 SyncCommit |
| 隔离写（sync） | 50.1 ms / 100k，1996 krows/s | 预构造 Row |
| 隔离写（async） | 39.2 ms / 100k，2554 krows/s | 预构造 Row |
| FULL 顺序写（async） | 86.9 ms / 100k，1150 krows/s | 无 fsync |
| Get 热读（复用 dst） | ~0.46 µs，2 allocs，104 B | 缓存命中 |
| Get 热读（无复用） | ~0.53 µs，2 allocs | 同上（dst 复用后差异极小） |
| Get 冷读 | 255 µs，6 allocs，328 KB | 缓存关闭，mmap |
| 并发 Get 8 / 64 goroutine | 6.6 / 6.7 µs（并行，缓存全热） | LRU 全局锁为主导成本 |
| Scan 100k（热） | 13.7 ms，7312 krows/s | 缓存可容纳全部块 |
| Scan 100k（冷） | 24.2 ms，4138 krows/s | 每轮全量 mmap 读+解压+CRC |
| Scan 1M | 243 ms，4116 krows/s | 22.9MiB 索引常驻 |
| 随机读 1M 冷 | 303 µs | 未预热，接近冷读 |
| Get DeepChain（32 层，热） | 3.4 µs | 父链解析 + 单行解码 |
| Scan DeepChain（32 层） | 31.9 ms，4133 krows/s | 132k 行合并 |
| Open 索引重放（100k） | 5.7 ms，28.7 MB/op | .rpi 全量载入 |
| RebuildIndex（100k） | 27.2 ms，71 MB/op | .rpk 扫描 + 索引重建 |
| 延迟 Get 热读 p50/p95/p99 | 250 / 292 / 625 ns | 4096 样本 |
| 延迟 Get 冷读 p50/p95/p99 | 276 / 317 / 506 µs | 4096 样本 |
| 延迟 DeepChain p50/p95/p99 | 1.9 / 2.5 / 4.8 µs | 4096 样本 |

> 注：热读真实吞吐 ~2M get/s（0.5 µs），远优于历史报告中的 7–12 µs——历史数值
> 受低 `-benchtime` 下一次性开销（GC/页缓存）稀释，非真实性能；统一矩阵的
> 预热逻辑已消除该偏差。并发 Get 未随 goroutine 扩展（6 µs 平台），指向
> v1.2 Tier 2 的 LRU 分片优化点。

访问模式说明：快照的主要使用方式预计是按表、范围或 RowID 集合批量读取，因此单行冷读
仅作为诊断指标。v1.2 将批量分块读取（ReadBatch）的块数、解压次数、实际读取字节和 p95
作为主要的批量读取指标（Tier 1）。

## 1.1 批量分块读取（v1.2 Tier 1）

对比由 `make bench-batch` 复现（同 ids 集合，基线=逐行 Get，目标=ReadBatch，
1M 行 × 4096 行/批，每场景 10s）：

| 场景 | 逐行 Get | ReadBatch | 提升 | 块/批 |
| --- | --- | --- | --- | --- |
| rand / hot | 7.57 krows/s | 27.2 krows/s | 3.6× | 389 |
| rand / cold | 3.45 krows/s | 33.4 krows/s | 9.7× | 389 |
| seq / cold | 3.70 krows/s | 1941 krows/s | ~525× | 3 |
| seq / hot | 4059 krows/s | 3167 krows/s | 0.78× | 3 |

`Stats.Batch.Blocks`/`RawBytes` 验证“相同块集合只发生一次读取/解压/校验”。
seq/hot 档较“复用 dst 的逐行 Get”慢 ~22% 是物化语义成本（ReadBatch 返回独立
行、不借用调用者缓冲）；与等语义无复用 Get 对比约 1.9× 快，Scan 顺序读路径
未改动。详见 docs/plan-v12.md §3.1。

## 2. 历史微基准（v1.0，100k 行 × 7 列）

| 场景 | 指标 | 说明 |
| --- | --- | --- |
| FULL 顺序写 | 122 ms/op，196 MB 分配/op | 提交边界一次 fsync |
| Get 热读（缓存命中） | 10.3 µs/op，898 B/op，8 allocs | 零磁盘 I/O |
| Get 冷读（缓存关闭） | 291 µs/op，439 KB/op | 单块读+解压+单条解码 |
| 并发 Get 1/8 goroutine | 195 µs / 195 µs | 读路径无全局锁，线性扩展 |
| Scan（100k 行） | 69 ms（1450 krows/s），109 MB 分配 | 块内游标 |
| Open 索引重放（100k 行） | 9.2 ms | 全量载入内存 |
| RebuildIndex（100k 行） | 92 ms | 从 .rpk 重建 |

## 2. 大规模（1M 行 × 7 列）

| 场景 | 指标 |
| --- | --- |
| FULL 顺序写 | 1.49 s（673 krows/s）——更大块更满、压缩更高效 |
| 随机读（2000 次，块缓存热） | 165 µs/op（6 kget/s），11 allocs |
| 全表 Scan | 770 ms（1299 krows/s） |

## 3. DELTA 链（FULL 10 万行 + 32 层 DELTA，每层 1000 行）

| 场景 | 指标 |
| --- | --- |
| 链头点查（父链解析 32 层） | 394 µs/op，23 allocs |
| 链头全表 Scan（132k 行合并） | 89 ms（1480 krows/s） |

## 4. 端到端（200k 行，正确性 + 计时回归测试）

| 阶段 | 耗时 |
| --- | --- |
| FULL 写入 + 提交 | 249 ms（804 krows/s） |
| 随机读校验（2062 次逐值比对） | 26 ms |
| 全表 Scan + 校验 | 120 ms |
| Close / Reopen | 19 ms |

## 5. 优化前后对比（v1.0 性能迭代）

| 基准 | 优化前 | 优化后 | 提升 |
| --- | --- | --- | --- |
| Scan 100k | 7.5 s / 43.9 GB 分配 | 69 ms / 109 MB | **~110x** |
| Get 热读 | 466 µs / 441 KB / 35 allocs | 20 µs / 896 B / 7 allocs | **~24x** |
| 并发 Get g1 | 498 µs | 195 µs | ~2.5x |
| FULL 写吞吐 | 300 krows/s / 996 MB 分配 | 360 krows/s / 293 MB | +20% / 分配 -70% |
| Get 冷读 | 327 µs / 985 KB / 58 allocs | 316 µs / 466 KB / 14 allocs | 分配 -53% |

## 6. 优化手段

1. zstd encoder/decoder sync.Pool 池化 + encoder 输出缓冲池——消除每次块压缩/解压
   新建实例的 ~1 MiB 直方图分配及 EncodeAll 的 256 KiB 输出预分配。
2. `block.ParseRowAt` 单条读取——随机读 O(1) 于块大小。
3. Scan 块内游标 + `ParseRowsDirectory` 轻量目录解析——同块连续行复用目录，
   记录字节按需切片，不解析逐条记录头。
4. `FlushedBlock`——builder 直接传递已构建目录条目，提交不再重复解析 payload。
5. index.Builder body 预分配 + Build 直接返回结构化 Txn；`Reserve` 预分配
   entries 切片与去重 map。
6. codec.EncodeInto——写路径复用编码缓冲区。
7. buildRawPayload 复用 uncompressed payload scratch 缓冲。

## 7. 正确性保障

- 优化全程 `go test ./...` 与 `go test -race ./...` 通过（含 32 goroutine 并发测试）。
- zstd 池：确定性输出测试 + 8 goroutine 并发 round-trip 测试。
- `TestPerfEndToEnd`：200k 行写→随机读逐值校验→全表 Scan 校验→Close/Reopen
  再校验，作为性能回归闸门。

## 8. 复测记录：2026-09-07 晚（harness 缺陷定位与修复）

> 本轮复测（18:29–19:00）经历了「疑似退化 → 定位 harness 测量缺陷 → 修复确认」
> 三步，最终结论：**无生产代码回归，写基准曾因 harness 缺陷虚高 15–40%**。
> 完整输出在 `docs/bench-results.txt`（`make bench` 规范产物，最终一轮已覆盖）。

### 8.1 经过

1. 首轮复测（机器负载/内存压力大：swap 3.5 GB、内存剩余 ~240 MB）发现写基准
   慢 +15%~+47%，一时归因于环境；清理进程后写基准并无好转。
2. 用 `git worktree` 在 cc34694/59cb5a5/2dc5d40 旧提交与 HEAD 上同参对拍，发现
   旧提交矩阵与旧式 harness 都稳定跑出基线值（write_full 94–97 ms），唯独 HEAD
   矩阵 harness 慢（111–113 ms）。CPU profile 显示差异集中在 `runtime.madvise`
   （Go scavenger），应用热区（zstd/codec/crc32）完全一致。
3. 逐步 overlay 排除 tmpdb/applyIO/opts 后，把矩阵写函数还原为 2dc5d40 风格
   （普通 `if err != nil { b.Fatal }`）即恢复 95.9 ms → 根因是 `a82d857`「调整测试
   框架」把计时热循环改成**无条件 `require.NoError(b, w.Insert(...))`**——每行 1 次、
   100k 次/轮迭代，testify require 成功路径也要走 `t.Helper()`（runtime.Callers），
   每调用 ~150 ns，累计 ~15 ms，恰好是虚高幅度。旧式 harness 与加密 harness
   用的是「出错才 require」正确模式，所以历史基线与加密对拍数字不受影响。

### 8.2 修复

计时热循环统一改为 `if err := ...; err != nil { require.NoError(b, err) }`
（bench_matrix_test.go 写/读/扫描、bench_batch_test.go 基线 Get、
bench_encryption_test.go Get/Scan、bench_test.go 旧式基准）。错误语义不变。

### 8.3 修复后验证（19:00 全矩阵）

措施：先用 `git worktree` 同机对拍排除代码退化；修复后 `make bench` 全矩阵对照
14:37 基线：

- 写场景（6 cell 全档）：**全部回到基线 ±5% 内**（write_full 256K sync +0.9%，
  iso +4.5%；64K sync +3.8%；其余 <±3%）。
- 读/扫描/重建：scan/scan1m ✓ ±3%、get_cold ✓ ±5%、open_replay/rebuild ✓ ±12%
  （多为改善）、scan_deepchain +1~4%。
- 热读（3s benchtime 复核）：get_hot 375 ns（基线 458–527 ns，更快）、
  get_deepchain 2.04 µs（基线 3.4 µs，更快）、conc_get g8 5.3 µs（基线 6.6–9 µs）。
- 同步验收门槛：加密 vs 未加密 get_hot 241.8/241.8 ns（0%）、get_cold +5.2%
  （≤10% ✓）、scan +0.2%、write +0.1%；批量读 vs 逐行 Get rand 4.9×/10.1×、
  seq/cold ~530×、seq/hot 1.35×。

结论：`make bench` 现在测量的是引擎真实行为；14:37 基线（§1）继续有效，作为
默认对照基线。
