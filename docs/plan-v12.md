# RowPack v1.2 优化计划

> 状态：规划基线
> 目标：以 v2 单文件格式优化批量分块读取、扫描内存、快照链和大规模打开；不要求兼容旧文件格式。
> 基线：[perf-report.md](perf-report.md) §1
> 单文件格式方向：[BINARY_FORMAT_V2.md](BINARY_FORMAT_V2.md)

## 1. 目标与非目标

目标是优化快照的批量分块读取，降低真实大数据场景的扫描峰值内存和长时间运行后的索引常驻内存，同时保持当前热读、单行 Get 和写入吞吐不回退。单行冷读保留为诊断指标，不作为唯一优化方向。

本版本不做列式存储、逐行加密、数据库查询优化器或 v2 磁盘格式；数据分块加密另见 [DATA_BLOCK_ENCRYPTION_FEASIBILITY.md](DATA_BLOCK_ENCRYPTION_FEASIBILITY.md)。

## 2. P0：统一基准与可观测性

- 统一 CPU、Go、压缩库、BlockSize、缓存状态和 `-benchtime`。
- 区分冷缓存、热缓存、mmap、ReadAt、SyncCommit 和 AsyncCommit。
- 增加 p50/p95/p99 延迟，而不只记录平均值。
- 增加文件大小、压缩率、解压字节数、Cache 命中率和峰值 RSS。
- 每个优化必须同时记录吞吐、分配和内存峰值。

验收：所有当前基准可以由单条命令重现，报告只保留一组当前基线。

### 2.1 状态：已完成（实现于 2026-09-07，文档同步于本次变更）

- [x] `BenchmarkMainMatrix`（bench_matrix_test.go）：场景 × BlockSize × 缓存 ×
      持久化 × I/O 路径的统一剪枝矩阵，子测试命名即维度（如
      `scan/bs=256K/cache=hot/io=mmap`），单条命令跑完全部基线：
      `go test -run '^$' -bench 'Benchmark(Env|MainMatrix|Latency)' -benchmem`。
- [x] `BenchmarkEnv`：运行时探测 Go / zstd 模块版本、OS/arch，并用真实构建的
      100k 数据集测量 `dataMB / ratio / bytePerRow / indexMB`，保证报告自描述。
- [x] `BenchmarkLatency`：固定 4096 样本的 p50/p95/p99 点读延迟（hot / into /
      cold / deepchain）。注：该子测试为固定采样设计，`ns/op`/`B/op` 列无意义，
      只看 `p50ns/p95ns/p99ns`。
- [x] 每子测试额外指标：`krows/s|kget/s` 吞吐、`dataMB` 数据文件大小、
      `ratio`=存储/原始压缩率、`idxMB` 索引常驻、`hitpct` 缓存命中率、
      `rssdMB` 进程峰值 RSS 增量（getrusage，非 unix 平台为 0）。
- [x] mmap vs ReadAt 对比：`iofile.ForceReadAt` 测试钩子强制所有新视图走
      ReadAt 兜底路径（生产路径不变，仅 one-branch OR）。
- [x] `make bench`：单条命令复现全部基线并落到 docs/bench-results.txt；
      `BENCHTIME`/`BENCHCOUNT` 可覆盖。- 测量卫生：读场景在 `b.ResetTimer` 前
      预热吸收一次性开销（GC/页缓存），避免低 `-benchtime` 稀释 per-op 数值。

遗留（留给后续版本）：解压字节数计数（`Stats` 无按路径拆分的解压统计，待 Tier 1
批量读一起加）；跨进程多实例的矩阵编排。

## 3. P0：批量分块读取

快照的主要访问模式预计是按表或 RowID 范围批量读取。当前逐行读取可能重复触发 Block 定位、读取、解压和校验，因此 v1.2 应优先把请求按 Block 聚合。

建议增加 `ReadBatch` 或等价内部批量读取路径，输入 Snapshot、Table 和 RowID 范围/集合。

- 先按 BlockID 排序和去重，再执行读取、解压和校验，最后按 RowID 或请求顺序返回；
- 支持相邻 Block 预读和合并 I/O，并设置最大合并窗口；
- 在一次批量请求中复用解压缓冲和目录解析结果；
- 增加 BatchSize、Block 数、解压次数和实际读取字节统计；
- BlockSize 矩阵以批量吞吐和 p95 为主指标，单行冷读作为辅助指标；
- 完整 CRC 校验必须保留，批量路径只减少重复工作，不降低完整性语义。

验收目标：批量读取有效吞吐较逐行 Get 提升 3 倍以上；相同 Block 集合只发生一次读取、解压和校验；顺序读取吞吐不下降超过 5%。

### 3.1 状态：已完成（2026-09-07，基准档 1M 行 × 4096 行/批）

- [x] `Store.ReadBatch(ctx, snapshot, table, ids []RowID) ([]Row, error)`：
      按块聚合的批量读；每个块 Load+CRC+解压至多一次；单行块走 O(1)
      ParseRowAt，多行块 ParseRowsDirectory 一次后逐条解码；去重后的块按
      BlockID 升序（=文件布局序）读取；错误语义与 Get 逐行一致（任一 id
      缺失/删除 → ErrNotFound，整批失败；重复 id 与逐行 Get 相同重复返回）。
- [x] `Stats.Batch`（Calls/Rows/Blocks/RawBytes）：量化聚合效果——
      Blocks 恒为批内唯一块数，RawBytes 为这些块 raw 字节总和，可验证
      “相同块集合只发生一次读取/解压/校验”。
- [x] 基准：`BenchmarkBatchBaselineGet`（基线）/ `BenchmarkReadBatch`
      （实现后），同 ids 对比；`make bench-batch` 一键复现。
- [x] 测试：与逐行 Get 值对拍（FULL/DELTA 链 × 随机/顺序）、错误语义、
      空/重复 id、Stats 计数（1000 行聚簇 → Blocks=1）、Close/Reopen、
      16 goroutine 并发（-race）。

验收结果（2026-09-07，10s×8 场景）：

| 场景 | 逐行 Get | ReadBatch | 提升 | 块/批 |
| --- | --- | --- | --- | --- |
| rand/hot | 7.57 krows/s | 27.2 krows/s | 3.6× | 389 |
| rand/cold | 3.45 krows/s | 33.4 krows/s | 9.7× | 389 |
| seq/cold | 3.70 krows/s | 1941 krows/s | 525× | 3 |
| seq/hot | 4059 krows/s | 3167 krows/s | 0.78× | 3 |

说明：rand 两档与 seq/cold 达标（≥3×）；seq/hot（缓存全热、4096 行挤在
3 个块）慢于复用 dst 的逐行 Get ~22%，根因是物化语义（ReadBatch 返回独立
行，allocs/批 16429 vs 8195，无法借用调用者缓冲）；与等语义无复用 Get
（~1660 krows/s）对比约 1.9× 快，Scan 路径未改动。若需消除该档差距，
后续提供 ReadBatchInto（行缓冲复用）形态，本版本不引入。

## 4. P1：Scan 内存与缓存策略

原状：1M 行 Scan 约 238 ms、155.6 MB/op，根因是 Scan 与随机 Get 共用 decoded
LRU：每个 miss 块解压后无条件晋升缓存，大 Scan 把一次性块全部挤进热点集并
持续颠簸（1M 行 ~390 块 × 256 KiB 解压缓冲逐块新分配），同时挤掉随机读热点。

### 4.1 状态：已完成（2026-09-08）

- [x] `Reader.ReadAtBlockTransient` + 解压 scratch 池（internal/block/rawbuf.go）：
      解压目标缓冲从 sync.Pool 复用，DecodeAll 在容量够时零分配；CRC 失败/截断错误
      语义与普通路径一致（TestTransientRejectsCorruption），池复用不串数据并带
      fuzz（FuzzReadAtBlockTransient）。
- [x] `blockLoader` 双缓存预算：随机读 decoded LRU（份额不变）与有界 Scan window
      （`scanBudgetFor` = CacheBytes/2，下限 1 MiB、上限 64 MiB）。Scan 查询顺序
      decoded cache → scan window → 池化瞬态读；瞬态块仅在 window 有余量时晋升，
      否则流式走池，不分配也不碰随机读热点（loader.go LoadScan/scanRef）。
- [x] Scan 已改走 `LoadScan`（iterator.go locateBlock），换块/结束/Close 时把 scratch
      归还池；热块（cache 或 window 命中）零解压复用，跨迭代小工作集全命中。
- [x] `Stats.ScanCache`（独立 CacheStats）与基准新列 `scanhitpct`：Scan 命中率不再
      混入随机读 hitpct（后者保持为 Get 专有口径）。

验收结果（2026-09-08，3×3 迭代，bs=256K，环境同 §2.1）：

| 场景 | 基线 | 现在 | 变化 |
| --- | --- | --- | --- |
| Scan 100k hot | 7397 krows/s | 7290 krows/s（scanhitpct 80 = 旧 hitpct 80） | -1.4% ✓ |
| Scan 100k cold | 4104 krows/s / 15.4 MB | 4132 krows/s / 3.2 MB | +0.7% / **-79%** ✓ |
| Scan 1M | 4077 krows/s / 155.6 MB | 4421 krows/s / 40.0 MB | +8.4% / **-74%** ✓ |
| Scan DeepChain（32 层） | 4085 krows/s / 8.45 MB | 3905 krows/s / 8.8 MB | -4.4%（≤5% 门槛）✓ |
| getrand1m / get_deepchain / write_full | — | get 3.24 kget/s、deepchain 99.96 hitpct，与基线一致 | 无回归 ✓ |

说明：peak RSS 的 `rssdMB` 是 getrusage 进程历史峰值增量，随分配器高水位波动、受跑序
干扰，不作为本项验收口径；`B/op`（155.6 → 40.0 MB，-74%）是最稳定的内存代理指标，
1M Scan 总分配从每次 155.6 MB 降至 40.0 MB，超额完成“降低 30%”门槛。热 Scan
（100k warm 与 DeepChain）均在 5% 内。

遗留（后续可选）：compressed-only / adaptive 缓存策略枚举、Scan window 预算对外
暴露（当前内部固定 CacheBytes/2，上限 64 MiB）。

## 5. P1：Delta checkpoint 与物化视图

- 增加快照链深度统计和阈值告警。
- 提供显式 `CreateCheckpoint` 或等价的 FULL 快照生成流程。
- 对热点快照缓存 materialized row view，缓存必须可淘汰。
- 保持旧快照不可变，checkpoint 只是新快照，不原地合并历史文件。

建议默认阈值：深度 16 提示，深度 64 建议 checkpoint；不自动执行可能影响磁盘和 IO 的重写操作。

验收目标：32/64/128 层链的 Get 与 Scan 均有独立基准；checkpoint 后读取成本接近 FULL 快照。

## 6. P2：惰性索引与大规模打开

- 按 Snapshot/Table 分片持久化或映射 Row Index。
- Open 时只加载最近快照和必要导航信息。
- 冷分片按需加载并使用 LRU 淘汰。
- RebuildIndex 仍支持完整重建，不能依赖所有分片常驻内存。

这是大规模 Store 优化，只有在千万级行数或大量快照下才进入实现；小数据集若收益不明显则不做复杂化。

验收目标：大 Store Open 的常驻索引内存随“已访问分片”增长，而不是随全量行数线性增长；随机 Get 性能无明显回退。

## 7. 版本与验收门槛

- v2 不保留旧文件格式兼容路径；运行时缓存必须可删除并重建。
- `go test ./...`、`go test -race ./...`、fuzz、golden 和故障恢复全部通过。
- 所有性能结论必须有基准命令、样本次数、环境和缓存状态。
- 未达到验收目标的优化不进入默认路径，只保留实验开关。

## 8. 实施顺序

```text
统一基准 → 批量分块读取 → 解压缓存预算 → checkpoint
       → BlockSize 矩阵 → 惰性索引评估 → 仅对有明确收益的方案实现
```
