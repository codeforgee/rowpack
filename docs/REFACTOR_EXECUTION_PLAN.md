# RowPack v2 格式重构执行计划

> 状态：执行计划（对应设计稿 `FILE_FORMAT_REFACTOR_PLAN.md`，以其为唯一需求来源）
> 日期：2026-09-09
> 用法：本文是排期与落地层，回答"做什么、改哪些文件、怎么验收、何时停"。格式细节以设计稿
> §5–§11 为准，验收门槛以 §3 为准，本文不重复设计，只引用。
> 预计总工期：约 10–17 个工作日（单人、含原型实验）。

## 1. 阶段总览与提交边界

```text
S0 测量冻结 ──┐
              ├→ S1 无格式优化 ──→ S2 Rows Page ──→ S3 Index Page/Eager ──→ S4 Lazy ──→ S5 冻结清理
S1 可与 S0 并行┘   (提交2,3)        (提交4,5,6)        (提交7,8)             (提交9)     (提交10)
```

| 阶段 | 内容 | 提交边界（设计稿 §16） | 预估 | 回退性质 |
| --- | --- | --- | --- | --- |
| S0 | 冻结测量方法、固化基线 | ① benchmark 与 profile 基线 | 0.5–1d | 必做 |
| S1 | 消除不依赖格式的浪费 | ② arena/缓存预算 ③ 流式 IndexTxn | 1–2d | 独立保留 |
| S2 | Rows Page 原型与落盘 | ④ 内存原型 ⑤ 落盘格式 ⑥ Page cache 读路径 | 3–5d | **决策门**：冷读不达标则止损 |
| S3 | Row Index Page + Eager | ⑦ 排序索引页 ⑧ Eager 紧凑索引 | 2–4d | 依赖 S2 通过 |
| S4 | Lazy 索引模式 | ⑨ Lazy Index | 2–3d | 可裁剪（Eager 已达标） |
| S5 | 格式冻结与清理 | ⑩ 恢复/golden/规范/旧代码清理 | 1–2d | 必做收尾 |

规则（来自设计稿 §16）：

- 每个提交保持 `go test ./...` 绿；格式切换提交允许集中更新 golden，但禁止静默双格式分派。
- S2 是止损点：若冷读收益 < 4x 或文件大小超门槛，停止 S3/S4，保留 S0+S1 收益并更新设计稿。
- 建议在 `refactor/v2-page-format` 分支推进，每阶段合并一次 main（S0/S1 也可直接进 main）。

## 2. 现状盘点（计划与代码的差异确认，2026-09-09）

制定计划时核对过代码，以下事实影响任务定义：

1. **Scan arena 已基本完成**：`iterator.go` 的 `strArena` 已同时覆盖 String 与 Bytes
   （`materialize` / `materializeBytes`），perf-report 显示 Scan 100k 全表仅 135 allocs。
   → S1 第 1 项改为"验证 0 alloc/row 门槛 + 补齐遗漏路径"，不是从零实现。
2. **scratch 池无分级**：`internal/block/rawbuf.go` 是单一 `sync.Pool`，按需增长、无上限。
   → S1 的"尺寸分级 + 上限"是真实工作量。
3. **缓存是双 LRU 独立预算**：`loader.go` 用 `scanBudgetFor`（cache/2，1–64 MiB 夹紧）另立
   scan 窗口，`CacheBytes` 不等于总解压预算，与设计稿 §8.1 目标不符。
   → S1 需重做预算模型与 Stats 口径。
4. **基线口径需统一**：设计稿 §2 的"冷读 328 KiB/op"是读取/解压放大口径，perf-report 的
   "160 B/op / 6 allocs"是池化分配口径；两者都对但不可混用。S0 必须在报告中显式定义双口径。
5. **索引条目现状**：`internal/fileformat/indextxn.go` 的 `RowIndexEntry` 为 40 B 定长
   （含逐条 CRC），`View.rowShard` 常驻约 24 B/row —— 与设计稿 §2/§7 的重构起点一致。

## 3. S0：冻结测量方法

**目标**：让后续每个百分比都有可复现的对照物。

| # | 任务 | 落点 |
| --- | --- | --- |
| 0.1 | 固定数据集几何与随机种子：100k/1M/10M 行 × 7 列混合、同构行、大 String、大 Bytes、超大行、深链 1/8/32/128 层、随机 RowID、小 DELTA | `bench_test.go`、`bench_matrix_test.go` 抽出 `benchspec` 常量 |
| 0.2 | `BenchmarkGetCold` 增报读取字节、解压字节、临时分配字节；临时分配用关池化对照模式（如 `ROWPACK_BENCH_NOPOOL=1`）测得 | `bench_test.go`、`internal/block/rawbuf.go`（加旁路开关） |
| 0.3 | 新增 1M/10M 索引内存与 Open 峰值 benchmark（EagerIndexBytes + RSS 增量，沿用 rssdMB 口径） | 新 `bench_memory_test.go` |
| 0.4 | 同机连续 5 次 `make bench`，确认主要指标方差可解释；确认基准不计入数据构造时间 | Makefile（必要时加 `bench-mem` 目标） |
| 0.5 | 基线归档：bench 原始输出 + CPU/heap profile + 数据集几何 + 双口径说明，存 `docs/baseline/`（profile 大文件 gitignore，报告入库） | `docs/perf-report.md` 增补"重构前基线"一节 |

**退出条件**（设计稿 §12 阶段 0）：5 次方差可解释；读路径不含构造时间；双口径定义写入报告。
**产出**：提交 ①。

## 4. S1：无格式依赖优化

**目标**：不动磁盘格式，先拿走 Scan 分配、Open 峰值和缓存统计的确定收益。

| # | 任务 | 落点 | 验收 |
| --- | --- | --- | --- |
| 1.1 | Scan/Batch arena 收尾：核对所有 Bytes/String 出口走 `strArena`；跨 `Next` 保留语义文档化 | `iterator.go`、`read_batch.go` | Scan 100k/1M = 0 alloc/row |
| 1.2 | IndexTxn 流式消费：chunk 解码改为 EntrySink 回调/迭代器，Open 直接写最终 `rowShard`，删除 `[]RowIndexEntry` 中转切片 | `internal/index/txn.go`、`chunk.go`、`view.go`、`rowpack.go`(Open) | Open 峰值明显下降；索引内存不变差 |
| 1.3 | 统一缓存预算：`CacheBytes = DataPageCache + ScanWindow (+IndexPageCache 预留)`；三段显式 Options 或策略分配；LRU 管理开销计入 Stats | `options.go`、`loader.go`、`stats.go` | 三段之和 ≤ 总预算；Stats 报 overhead |
| 1.4 | scratch 池分级与上限：按 32K/128K/1M 级别分池并设总量上限，超大 buffer 不入池 | `internal/block/rawbuf.go` | heap profile 无池留存大 buffer；冷读 B/op 不回退 |

**退出条件**：测试全绿；Scan 0 alloc/row；Open peak 明显下降；本阶段提交独立保留。
**产出**：提交 ②、③。

## 5. S2：Rows Page 原型与落盘（决策门）

**目标**：把解压/校验/加密单位从 256 KiB Block 降到 16–64 KiB Page，验证冷读收益成立。

| # | 任务 | 落点 |
| --- | --- | --- |
| 2.1 | 内存版 RowsPageBuilder/Reader：RowID zigzag delta、end-offset delta、SchemaVersion RLE、ChangeType 2-bit（保留非法值）、TypedTuple body 去 ColumnCount/位图长度、解码必须恰好到达记录边界 | 新 `internal/block/page_builder.go`、`page_reader.go` |
| 2.2 | 定长 Page Directory（PageOrdinal/FirstRecordOrdinal/RecordCount/StoredOffset/StoredSize/RawSize/MinRowID/MaxRowID/PageCRC/Flags），明文 + CRC 覆盖；字段宽度原型测量后冻结 | `internal/fileformat/`（新 page 目录布局） |
| 2.3 | PageSize 16/32/64 KiB × BlockSize 64/256/1M 矩阵实验：压缩率、写 CPU、冷读、扫描四组数据；同时验证 Min/Max RowID 是否保留（检查点 #7） | bench + `docs/adr/` 新决策记录 |
| 2.4 | 落盘：FileHeader 增加 DefaultBlockSize/DefaultPageSize（建库后不可变，Open 以文件为权威）；Writer 直接编码进 Page scratch，Page 满即压缩，Block flush 只组装 Header+Directory+Stored Pages | `internal/fileformat/headers.go`、`constants.go`、`writer.go`、`internal/block/builder.go` |
| 2.5 | Page 加密：nonce 域绑定 StoreUUID/SnapshotID/BlockID/PageOrdinal/KeyEpoch，AAD 绑定 Directory 关键字段；nonce 位域与 AAD 布局先写进规范再实现，附跨域不相交测试 | `encryption.go`、`docs/BINARY_FORMAT_V2.md` |
| 2.6 | 读路径切换：blockLoader → Page 粒度 loader（Data Page Cache + ScanWindow），Get 只解压一个 Page；ReadBatch 按 Page 分组排序加载；Scan 复用少量 scratch | `loader.go`、`read.go`、`read_batch.go`、`iterator.go` |
| 2.7 | 溢出与损坏加固：所有长度/计数/offset 运算在分配前检查；伪造 size/count/offset、单 bit 翻转、Directory 重叠不得 panic/无界分配 | `internal/fileformat/errors.go`、新 fuzz 目标 |

**验收门槛**（§3.1/3.2）：256 KiB 块冷点读临时分配 ≤ 64 KiB/op（期望 ≤ 40 KiB）；冷读 ≥ 4x
（期望 6–10x）；文件大小顺序负载 ≤ +10%、随机 RowID ≤ +15%；写吞吐 ≥ 基线 90%；热点读 ≤ 125%。
**退出动作**：确定 PageSize 与字段布局（检查点 #1、#5），写入 ADR 后才允许冻结常量。
**产出**：提交 ④、⑤、⑥。**未过门槛 → 止损**，保留 S0/S1，更新设计稿。

## 6. S3：Row Index Page + Eager 模式

| # | 任务 | 落点 |
| --- | --- | --- |
| 3.1 | 排序索引页：按 (SnapshotID, TableID, RowID) 排序；TableID run、RowID delta、BlockID run/delta、PageOrdinal/RecordOrdinal delta、ChangeType 2-bit；逻辑位置改为 (BlockID, PageOrdinal, RecordOrdinal, ChangeType)；页 2048 vs 4096 实验（检查点 #2） | `internal/fileformat/indextxn.go`（替换 RowIndexEntry）、新 `internal/index/page.go` |
| 3.2 | Fence Directory：每页 (SnapshotID, TableID, Min/MaxRowID, StoredOffset/Size, RawSize, EntryCount, CRC)，Open 常驻 | `internal/index/` |
| 3.3 | Eager Open 流式构建：边编码 Index Page 边写最终紧凑 shard，对比 packed struct / SoA / block-run（检查点 #3）；压缩级别是否与数据页共用（检查点 #6） | `view.go`、`rowpack.go` |
| 3.4 | Get/Exists/ReadBatch/Scan 接入新索引；Commit 峰值按 §8.3 流水线（排序 → 边编码边建 shard → 及时释放） | `read.go`、`read_batch.go`、`writer.go` |
| 3.5 | 索引损坏重建：从 Rows Page Directory 流式重建（§10.2），报告 `IndexRebuiltInMemory`，峰值不与行数同阶 | `recovery.go`、`verify.go`（full 模式遍历全部 Page） |

**验收门槛**：Eager ≤ 16 B/row；Open 峰值 ≤ 最终索引 + 2 个 Index Page；热点读 ≤ 125%
（期望 110%）；§13.2 损坏矩阵通过。
**产出**：提交 ⑦、⑧。

## 7. S4：Lazy 模式

| # | 任务 | 落点 |
| --- | --- | --- |
| 4.1 | `IndexMode`（Eager 零值）+ `IndexCacheBytes`；Open 只加载 Fence，Index Page 按需读取/解密/解压/LRU 缓存 | `options.go`、`loader.go`、`internal/cache/` |
| 4.2 | Lazy Get/Exists/ReadBatch/Scan；并发 miss 用现有 singleflight 合并；Scan 顺序流式读 Index Page 不污染随机缓存 | `read.go`、`read_batch.go`、`iterator.go` |
| 4.3 | 深链 1/8/32/128 层实验：负查询缓存 vs page-level bloom filter 原型对比（检查点 #4），ReadBatch 按 (Snapshot, IndexPage) 聚合；FULL checkpoint 后不读更早父链 | bench + ADR |
| 4.4 | 内存上限与缓存污染测试；Eager/Lazy 全 API 结果逐行一致测试 | 新一致性测试文件 |

**验收门槛**：Lazy ≤ 0.25 B/row + 固定开销（期望 ≤ 0.1）；与 Eager 语义完全一致；深链无不可
接受的 I/O 放大。
**产出**：提交 ⑨。

## 8. S5：格式冻结与清理

| # | 任务 | 落点 |
| --- | --- | --- |
| 5.1 | `BINARY_FORMAT_V2.md` 重写为唯一权威规范：Magic、版本号、全部结构尺寸、nonce 位域、AAD 布局冻结 | docs |
| 5.2 | 删除旧 Rows Payload（`internal/fileformat/rows_payload.go`）、旧 `RowIndexEntry` 及全部兼容分支 | internal/… |
| 5.3 | `make golden` 重新生成并人工 diff 审查（Makefile 已有字节变化告警流程） | `testdata/golden/` |
| 5.4 | 更新 `cmd/rowpack-inspect`、README、ADR、TEST_PLAN、perf-report；Stats 补齐 §11 全部计数器（双 Cache、ScanWindow、EagerIndexBytes/FenceBytes、PageLoads、IndexRebuiltSnapshots、OversizedRowPages 等） | cmd/、docs/、`stats.go` |
| 5.5 | 全量门禁：`go test ./...`、`-race`、fuzz、`go vet`、staticcheck、最终 `make bench`（加密增量成本单独成表） | Makefile/CI |

**退出条件**：仓库不存在旧格式读写路径；规范、实现、golden、inspect 输出四方一致。
**产出**：提交 ⑩。

## 9. 决策检查点登记表（设计稿 §15）

| # | 决策 | 阶段 | 验证方法 | 记录位置 |
| --- | --- | --- | --- | --- |
| 1 | 默认 PageSize 16/32/64 KiB | S2 | 压缩率×冷读×写 CPU 矩阵 | ADR + S2 报告 |
| 2 | Index Page 2048 vs 4096 条 | S3 | 顺序/乱序/小 DELTA 三负载 | ADR |
| 3 | Eager shard：packed / SoA / block-run | S3 | 热点读 CPU + 内存对比 | ADR |
| 4 | 深链是否要 Bloom filter | S4 | 深链负查询 benchmark，收益 > 内存+CPU 才引入 | ADR |
| 5 | metadata stream 是否在 Zstd 外再 delta/RLE | S2 | 压缩率与解码 CPU 对比 | ADR |
| 6 | Index Page 是否复用数据压缩级别 | S3 | 编码 CPU/大小对比 | ADR |
| 7 | Page Directory 是否保存 Min/Max RowID | S2 | 批量读取规划收益 vs 目录膨胀 | ADR |

规则：未经测量不得冻结常量；每个检查点记录候选实现、数据、结果与选择。

## 10. 测试与质量门（贯穿所有阶段）

- **每提交**：`go test ./...` + `go vet`；每阶段收尾加 `-race`。
- **S2 起**：fuzz 目标覆盖 Page/Directory/Index Page 解码（§13.2 损坏矩阵：截断、bit 翻转、
  伪造 size/count/offset、CRC/AEAD 错误、nonce 域错误、Directory 重叠、Fence 越界、错误归属）。
- **S3 起一致性**：Eager/Lazy × mmap/readat × plain/encrypted × None/Zstd 全组合逐行一致。
- **性能报告口径**：延迟、吞吐、B/op、allocs/op、读取字节、解压字节、索引内存、峰值 RSS、
  文件大小九项并列，禁止只报 ns/op。
- **溢出审计**：每阶段对新增长度/计数/offset 运算做一次专项 review（§3.2 约束）。

## 11. 风险与止损

| 风险 | 触发信号 | 动作 |
| --- | --- | --- |
| Page 变小压缩率崩 | 文件大小 > +10%/+15% | 回检查点 #1 扩大 PageSize 或加强 stream 编码 |
| 冷读收益不足 | < 4x | **S2 止损**：停 S3/S4，保留 S0/S1，更新设计稿 |
| Eager shard 查找 CPU 超标 | 热点读 > 125% | 退回 block-run 辅助表方案（设计稿 §14 预案） |
| Lazy 深链 I/O 放大不可接受 | 深链 Get 延迟劣化超门槛 | 负查询缓存/bloom 二选一；仍不行则文档声明 Lazy 适用边界 |
| Commit 峰值超标（10M 行） | peak benchmark 失败 | 按 §8.3 引入有界 run sort，不引入临时文件 |
| 池留存大 buffer | heap profile 大对象滞留 | 分级池 + 超大行不入池（S1 已含，超标则收紧上限） |

## 12. 立即行动清单（本周可开始）

1. S0：跑 `make bench` × 5 存档，补 `BenchmarkGetCold` 双口径指标与 1M/10M 内存基准（提交 ①）。
2. S1：`rawbuf` 分级池 + `loader.go` 统一预算 + `stats.go` overhead 口径（提交 ②）。
3. S1：`internal/index` 流式 consumer，Open 直建 shard（提交 ③）。
4. S2 启动前评审：nonce 位域与 AAD 布局草案先落 `BINARY_FORMAT_V2.md` 草稿节。
