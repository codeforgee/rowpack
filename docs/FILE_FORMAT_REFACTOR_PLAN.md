# RowPack v2 文件格式性能重构计划

> 状态：草案，待评审  
> 日期：2026-09-09  
> 适用范围：尚未发布的 RowPack v2 及其 Go 实现  
> 核心目标：提高读写性能，降低 CPU、常驻内存和操作峰值内存  
> 兼容策略：v2 尚未发布，不兼容当前 golden 文件；不保留旧格式 reader，不提供迁移工具

## 1. 背景与结论

当前 v2 已完成单文件、append-only SnapshotTxn、IndexTxn、Zstd、AES-256-GCM、崩溃恢复和
逐行随机访问，但仍有两个由磁盘布局决定的主要成本：

1. 数据块是完整压缩单元。默认 256 KiB Block 中读取一行，也需要读取、解压和校验整个
   Block，冷点读存在明显的读放大、CPU 消耗和临时内存分配。
2. 打开文件后，所有 Row Index 都被展开成每行约 24 字节的常驻内存结构；IndexTxn 解析和
   Commit 期间还会产生额外的逐行临时结构。

本次不另起 v3。由于 v2 尚未发布，直接重写 v2 的 Rows Block 和 Row Index 格式，同时保留
已经验证过的外层事务模型：

- 单个 `.rpk` 文件；
- SnapshotTxn 只追加、不原地覆盖；
- SnapshotFooter 是提交权威；
- FULL / DELTA 快照和不可变历史；
- 块或页级 Zstd、CRC32C、可选 AES-256-GCM；
- 数据损坏与派生索引损坏采用不同恢复语义。

目标格式采用“逻辑 Block + 独立压缩 Page”的数据布局，以及“排序 Row Index Page + 稀疏
Fence”的索引布局。Page 是实际的读取、解压、校验、加密和缓存单位；Block 继续作为写入、
统计和快照组织单位。

## 2. 当前基线

基线环境为仓库现有 benchmark 默认配置：Apple M1 Pro、Go 1.27、Zstd、256 KiB Block、
100k 行 × 7 列。

| 指标 | 当前值或结构成本 |
| --- | --- |
| FULL 顺序写 | 约 99.8 万行/秒 |
| 热点读 | 约 0.28–0.43 µs/op，约 98 B/op，3 allocs/op |
| 冷点读 | 约 0.33 ms/op，约 328 KiB/op，7 allocs/op |
| Scan 100k | 约 14 ms；包含约 100k 次 Bytes 相关分配 |
| Open 100k | 约 1.7 ms，约 6.9 MiB/op |
| Row Index 常驻内存 | 约 24 B/row |
| 数据解压单位 | 整个 Block，默认约 256 KiB raw |

实施前必须用固定提交重新生成完整基线，保存 benchmark 原始输出、CPU profile、heap profile
和数据集几何，避免只比较文档中的历史数字。

## 3. 成功标准

以下指标是合入新格式的验收门槛，不是方向性愿望。

### 3.1 主要指标

| 场景 | 必须达到 | 期望目标 |
| --- | --- | --- |
| 256 KiB 逻辑块冷点读临时分配 | ≤ 64 KiB/op | ≤ 40 KiB/op |
| 冷点读延迟 | 至少降低 4 倍 | 降低 6–10 倍 |
| Eager Row Index 常驻内存 | ≤ 16 B/row | 约 12 B/row |
| Lazy Row Index 常驻内存 | ≤ 0.25 B/row 加固定开销 | ≤ 0.1 B/row |
| Open 峰值索引内存 | 最终索引 + 2 个 Index Page | 最终索引 + 1 个 Index Page |
| Scan 每行分配 | 0 alloc/row | 整次 Scan 仅固定/按页分配 |
| FULL 写吞吐 | 不低于当前基线 90% | 不低于当前基线 |
| 热点读延迟 | 不高于当前基线 125% | 不高于当前基线 110% |

### 3.2 约束指标

- 顺序负载 `.rpk` 文件大小不得比当前格式增加超过 10%。
- 随机 RowID 负载文件大小不得增加超过 15%。
- 加密读写相对于同格式非加密路径的增量成本必须单独报告。
- 所有长度、计数、offset 和整数运算必须在分配、切片或类型转换前检查溢出。
- 损坏数据不得 panic、越界读取或按攻击者提供的尺寸进行无界分配。

## 4. 非目标

本次不包含：

- 兼容当前开发期 v2 文件或 golden 文件；
- 旧格式到新格式的在线或离线迁移；
- B+Tree、LSM compaction 或可写页；
- SQL 执行、谓词下推和列投影 API；
- 纯列式存储；
- 多写者或跨进程并发写；
- 改变 FULL / DELTA 的公开语义；
- 取消单文件和 append-only 提交模型。

没有列投影 API 时，纯列式格式会让整行 Get 读取多个列块，增加 I/O、解压和缓存复杂度，
因此本轮继续采用行式 Page。

## 5. 总体架构

```text
[FileHeader]

[SnapshotTxn]
  [SnapshotHeader]

  [Metadata Block/Page ...]

  [Rows Block]
    [RowsBlockHeader]
    [RowsPageDirectory]
    [Stored Rows Page 0]
    [Stored Rows Page 1]
    ...

  [IndexTxn]
    [Snapshot/Metadata/Block Index Chunk ...]
    [Row Index Page 0]
    [Row Index Page 1]
    [Row Index Fence Directory]
    [IndexTxnFooter]

  [SnapshotFooter]
```

外层 Block 用于限制一个写入批次的总规模和保持现有 Block API；内层 Page 才是物理读取、
压缩、加密、CRC 和缓存单位。

## 6. Rows Block 重构

### 6.1 Page 大小

首轮 benchmark 比较 16 KiB、32 KiB 和 64 KiB raw Page，默认值不提前冻结。初始推荐值为
32 KiB，原因是它把默认冷读解压量从约 256 KiB 降低到约 1/8，同时仍能给 Zstd 提供足够
上下文。

逻辑 BlockSize 和 PageSize 分离：

- `BlockSize`：默认 256 KiB，控制一个 Rows Block 的目标 raw 总量；
- `PageSize`：候选默认 32 KiB，控制独立压缩和缓存边界；
- 超过 PageSize 的单行使用独立 Large Row Page；
- 超过 MaxRowBytes 的行仍直接拒绝。

`FileHeader` 必须持久化 DefaultBlockSize 和 DefaultPageSize。PageSize 是建库后不可变的
格式参数；Open 时调用方不得覆盖文件内参数。

### 6.2 Rows Block 目录

Rows Block Header 后保存定长 Page Directory。每项至少包含：

```text
PageOrdinal
FirstRecordOrdinal
RecordCount
StoredOffset
StoredSize
RawSize
MinRowID
MaxRowID
PageCRC32C
Flags
```

具体字段宽度在实现原型测量后冻结。Directory 本身由 Block Header CRC 或独立 CRC 覆盖，
且保持明文，使读取器无需解密整个 Block 即可定位 Page。

`MinRowID/MaxRowID` 只用于批量读取规划和诊断，不假设物理记录按 RowID 排序。原始变更流
继续保持调用顺序，以保证 `ScanBlocks` 的现有语义。

### 6.3 Rows Page 负载

当前每行同时保存 24 字节 RowDirectoryEntry 和 24 字节 RowRecordHeader，字段存在重复。
新 Page 使用一次性目录流：

```text
[RowsPageHeader]
[RowID delta stream]
[Record end-offset stream]
[SchemaVersion RLE/delta stream]
[ChangeType 2-bit stream]
[TypedTuple bodies]
```

规则：

- Page 第一条 RowID 保存绝对值，后续保存 zigzag delta；
- Record offset 保存单调递增的 end-offset delta，避免同时保存 offset 和 length；
- 相同 SchemaVersion 使用 run-length 编码；
- ChangeType 使用 2 bit，保留一个非法值用于损坏检测；
- DELETE 没有 TypedTuple body；
- Page CRC 覆盖解压后的完整 Page，不再保存逐行 CRC；
- TypedTuple body 默认去掉重复的 ColumnCount 和 NullBitmapBytes，只保存 NullBitmap 与值；
- Schema 决定列数、位图长度和每列类型；解码结束必须恰好到达该记录边界。

逐行 CRC 的移除必须经过故障模型评审：Stored Page CRC、AEAD tag 和 raw Page CRC 已共同覆盖
传输、落盘和解压后数据。只有存在“单行独立复制、但不校验 Page”的公开能力时才需要保留
逐行 CRC；当前 API 没有这种能力。

### 6.4 Page 加密

- 每个 Page 独立先压缩后加密；
- AES-GCM nonce 域必须与 Metadata、Index Page 等其他对象分离；
- nonce 至少绑定 StoreUUID、SnapshotID、BlockID、PageOrdinal 和 KeyEpoch；
- AAD 绑定 Page Directory 中影响解析和归属的字段；
- Page Directory 保持明文但被认证；
- 未加密 Page 不承担 AES-GCM tag 开销。

Nonce 位域和 AAD 字节布局必须在实现前写入格式规范并添加跨域不相交测试。

## 7. Row Index 重构

### 7.1 排序索引页

Row Index 按 `(SnapshotID, TableID, RowID)` 排序后写入不可变 Index Page。每页建议 2,048 或
4,096 条记录，候选值通过顺序、乱序和小 DELTA 三种负载测试决定。

页内编码：

```text
TableID run
First RowID + RowID deltas
BlockID run/delta
PageOrdinal run/delta
RecordOrdinal delta
ChangeType 2-bit stream
```

当前 Row Index 只保存 BlockID 和 ItemOrdinal。新索引位置必须能够直接定位 Page，因此逻辑
位置调整为：

```text
(BlockID, PageOrdinal, RecordOrdinal, ChangeType)
```

### 7.2 Fence Directory

每个 Row Index Page 在明文或独立校验的 Fence Directory 中保存：

```text
SnapshotID
TableID
MinRowID
MaxRowID
StoredOffset
StoredSize
RawSize
EntryCount
PageCRC32C
```

Fence Directory 必须足够小，可以在 Open 时常驻内存。Get 先在 Fence 中二分定位 Index Page，
再从 Eager Index 或 Lazy Index Page Cache 中解析具体 RowLoc。

### 7.3 Eager 和 Lazy 两种模式

公开 Options 增加索引加载策略，零值采用 Eager，确保默认热点读不显著退化：

```go
type IndexMode uint8

const (
    IndexEager IndexMode = iota
    IndexLazy
)
```

Eager 模式：

- Open 时顺序解码 Index Page；
- 直接写入最终紧凑 shard，不创建 `[]RowIndexEntry` 中间副本；
- 目标常驻成本不超过 16 B/row；
- 优先采用 struct-of-arrays 或按 Block/Page run 分组，避免 Go struct padding；
- Get 保持纯内存二分查找。

Lazy 模式：

- Open 时只加载 Snapshot、Block、Metadata 和 Fence Directory；
- Row Index Page 按需读取、解密、解压和缓存；
- 缓存具有独立且显式的 `IndexCacheBytes` 预算；
- 同一 Index Page 的并发 miss 通过 singleflight 合并；
- Scan 可以顺序流式读取 Index Page，不污染随机 Index Page Cache。

两种模式必须返回完全一致的 Get、Exists、ReadBatch、Scan 和 tombstone 结果。

### 7.4 深 DELTA 链

本次保留按父链解析语义，但必须测试 1、8、32、128 层。Lazy 模式下逐层加载索引页可能放大
I/O，因此需要：

- 负查询缓存或 page-level bloom filter 二选一的原型比较；
- ReadBatch 按 `(Snapshot, IndexPage)` 聚合；
- FULL checkpoint 后不读取更早的父链；
- 不为了深链优化在本轮引入 compaction。

Bloom filter 只有在深链负查询 benchmark 证明收益大于内存和 CPU 成本时才进入冻结格式。

## 8. 内存和缓存模型

### 8.1 缓存预算

当前 random block cache 和 scan cache 分别分配预算，调用方看到的 `CacheBytes` 并不等于总
解压数据预算。重构后采用一个总预算并明确拆分：

```text
CacheBytes = DataPageCache + ScanWindow + IndexPageCache
```

建议 Options 最终提供：

```go
CacheBytes      int64 // 总预算
IndexCacheBytes int64 // 0 表示从总预算按策略分配
ScanCacheBytes  int64 // 0 表示从总预算按策略分配
```

必须保证三者实际占用之和不超过总预算；LRU map/list 节点等管理开销应进入统计，至少在 Stats
中单独报告 estimated overhead。

### 8.2 Buffer 生命周期

- 普通 Get 从缓存命中时不分配 Page buffer；
- 冷 Get 使用 PageSize 级 scratch，校验和解码完成后归还 pool；
- Scan 使用一个或少量可复用 Page scratch；
- String 和 Bytes 都通过 iterator-owned arena 暴露借用视图；
- 需要跨 `Next` 保留的调用方继续显式复制；
- arena 按 chunk 增长，Iterator.Close 时整体释放引用；
- 大行 buffer 不进入普通 Page pool，避免污染池容量分布。

### 8.3 Commit 峰值

Writer 不得同时长期持有以下三份逐行结构：Rows Directory、完整 `[]RowIndexEntry` 和最终
Eager shard。目标流程为：

```text
flush Rows Page
→ 生成紧凑 pending row locator
→ Commit 时外部/内存排序
→ 边编码 Index Page 边构建 Eager shard
→ 及时释放排序 scratch
```

首版允许 Snapshot 内存排序，但 peak benchmark 必须覆盖 1M 和 10M 行。若 10M 行无法满足
内存目标，再增加有界 run sort；不预先引入临时文件复杂度。

## 9. 读写路径变化

### 9.1 Get

```text
解析 Snapshot/Table
→ Eager shard 或 Fence + Lazy Index Page 定位 RowLoc
→ Data Page Cache 查找
→ miss 时只读取/解密/解压一个 Rows Page
→ Page CRC
→ 按 RecordOrdinal 解码 TypedTuple
```

### 9.2 ReadBatch

```text
批量解析 RowLoc
→ 按 BlockID/PageOrdinal 分组
→ 按物理 offset 排序
→ 每个 Page 最多加载一次
→ 恢复输入顺序
```

重复 RowID、缺失行和 tombstone 的公开语义保持不变。

### 9.3 Scan

- Eager 模式遍历最终排序 shard；
- Lazy 模式顺序遍历相关 Index Page；
- 数据 Page 按 Block/Page 物理顺序尽量聚合；
- 深链仍执行 RowID 归并和最新层覆盖；
- String/Bytes 使用 iterator arena，目标为 0 alloc/row；
- Scan Page 不驱逐随机热点 Page。

### 9.4 Write

- TypedTuple 直接编码进 Page builder scratch；
- Page 满时压缩一次，避免先拼完整 256 KiB raw Block 再切分；
- Block flush 只组装 Header、Page Directory 和已存储 Page；
- 压缩器为 Store/Writer 生命周期对象，不在每 Page 创建；
- Index Page 与 Rows Page 可以使用不同的压缩级别，默认先保持同级别并用 benchmark 决定。

## 10. 恢复与校验

提交事实仍只由有效且 offset 自洽的 SnapshotFooter 决定。

### 10.1 数据 Page 损坏

Rows Page 的 Header、Directory、密文认证、Stored CRC、解压长度或 Raw CRC 失败都是数据损坏，
读取涉及该 Page 时返回结构化 `CorruptionError`。Verify 的 full 模式必须遍历全部 Page。

### 10.2 Index Page 损坏

Row Index 是派生结构。SnapshotFooter 和 Rows Page 有效、但 Index Page/Fence 损坏时：

- 从当前 Snapshot 的 Rows Block/Page Directory 重建该 Snapshot 的索引；
- 只读和读写 Open 都可以在内存继续使用；
- 不原地覆盖文件；
- 报告 `IndexRebuiltInMemory`；
- 重建按 Page 流式进行，峰值不得与 Snapshot 行数同阶额外增长。

### 10.3 未提交尾部

沿用当前行为：最后一个有效 SnapshotFooter 之后的内容均不可见；读写 Open 截断，只读 Open
忽略并报告。物理扫描器必须能够按 StoredSize 跳过整个 Rows Block，无需逐 Page 解压。

## 11. API 和可观测性调整

候选 Options：

```go
type Options struct {
    BlockSize        int
    PageSize         int
    Compression      Compression
    CompressionLevel int
    CacheBytes       int64
    IndexMode        IndexMode
    IndexCacheBytes  int64
    ScanCacheBytes   int64
    // 其余字段保持
}
```

Create 将持久格式参数写入 FileHeader；Open 以文件内参数为权威。Open Options 只能调整运行时
缓存和索引模式，不能伪装成另一种磁盘 PageSize 或 Compression。

Stats 至少增加：

- DataPageCache capacity/used/hit/miss/eviction/load；
- IndexPageCache capacity/used/hit/miss/eviction/load；
- ScanWindow capacity/used/hit/miss；
- EagerIndexBytes / FenceBytes；
- PageLoads / PageRawBytes / PageStoredBytes；
- DecompressedBytes；
- IndexRebuiltSnapshots；
- OversizedRowPages。

## 12. 实施阶段

### 阶段 0：冻结测量方法

产出：

- 固定 benchmark 数据集和随机种子；
- 保存当前完整基线；
- 增加 `BenchmarkGetCold` 的读取字节、解压字节和分配指标；
- 增加 1M/10M 索引内存与 Open peak benchmark；
- 增加顺序、随机 RowID、小 DELTA、大 Bytes、超大行和深链数据集；
- CPU/heap profile 采集命令写入性能报告。

退出条件：在同一机器上连续 5 次测试的主要指标方差可解释，且基准不会把数据构造时间计入
读路径。

### 阶段 1：先消除不依赖格式的浪费

任务：

- 为 Bytes 增加 Scan arena，消除当前逐行分配；
- IndexTxn parser 支持回调/流式消费；
- Open 直接构建最终 shard，去掉中间逐行副本；
- 统一 cache 总预算与统计；
- 为 scratch pool 增加尺寸分级和上限。

退出条件：不改变格式时测试全绿；Scan 达到 0 alloc/row；Open peak 明显下降。该阶段提交可
独立保留，即使后续 Page 原型被否决。

### 阶段 2：Rows Page 原型

任务：

- 实现内存版 Page builder/reader；
- 比较 16/32/64 KiB Page；
- 实现紧凑 Page Directory 和 row metadata streams；
- 覆盖 None/Zstd、plain/encrypted、大行；
- Get/ReadBatch/Scan 切换到 Page cache；
- 对比压缩率、写 CPU、冷读和扫描。

退出条件：满足冷读、文件大小和写吞吐门槛，确定 PageSize 及固定字段布局。

### 阶段 3：Row Index Page 与 Eager 模式

任务：

- Snapshot 内按 TableID/RowID 排序；
- 实现 Row Index Page 和 Fence Directory；
- Eager Open 流式构建紧凑 shard；
- Get/Exists/ReadBatch/Scan 接入；
- 实现从 Rows Page 重建索引。

退出条件：≤16 B/row，热点读退化不超过门槛，Open peak 满足目标，损坏恢复测试通过。

### 阶段 4：Lazy 模式

任务：

- Fence 常驻与 Index Page Cache；
- Lazy Get/Exists/ReadBatch/Scan；
- 并发 miss 合并；
- 深链负查询优化实验；
- 内存上限和缓存污染测试。

退出条件：Lazy 常驻内存达标，语义与 Eager 完全一致，深链不存在不可接受的 I/O 放大。

### 阶段 5：格式冻结与清理

任务：

- 更新 `BINARY_FORMAT_V2.md` 为唯一权威规范；
- 删除旧 Rows Payload、旧 RowIndexEntry 和兼容分支；
- 重新生成并人工审查所有 golden；
- 更新 inspect 工具、README、ADR、测试计划和性能报告；
- 全量 test/race/fuzz/vet/staticcheck/benchmark；
- 冻结 Magic、版本号、结构尺寸、nonce 和 AAD。

退出条件：仓库不存在旧格式读写路径；规范、实现、golden 和 inspect 输出一致。

## 13. 测试矩阵

### 13.1 正确性

- 所有 Value 类型、NULL、空 String/Bytes、最大长度；
- INSERT/UPDATE/DELETE；
- 空 FULL、空 DELTA、多表、多 SchemaVersion；
- 重复和乱序 RowID；
- RowID 0、MaxUint64 及跨 delta 溢出边界；
- 普通 Page、恰好 PageSize、超大行 Page；
- Eager/Lazy 结果逐行一致；
- mmap/readat、plain/encrypted、None/Zstd 组合。

### 13.2 损坏与恢复

对每个固定结构和可变区执行：截断、单 bit 翻转、伪造 size/count/offset、CRC 错误、AEAD tag
错误、nonce 域错误、Page Directory 重叠、Index Fence 越界、重复 Index Page、错误 Snapshot
归属。断言错误分类、offset、BlockID/PageOrdinal，并确保无 panic 和无超限分配。

### 13.3 性能

至少覆盖：

- PageSize 16/32/64 KiB；
- BlockSize 64/256/1024 KiB；
- 100k、1M、10M 行；
- 7 列混合行、同构行、大 String、大 Bytes；
- 热/冷 Get、随机/连续 ReadBatch、热/冷 Scan；
- Eager/Lazy Open、Get、Scan 和深链；
- sync/async、mmap/readat、plain/encrypted；
- CacheBytes 从 0 到可容纳全部工作集；
- 顺序 RowID、随机 RowID、小 DELTA。

性能报告必须同时展示延迟、吞吐、B/op、allocs/op、读取字节、解压字节、索引内存、峰值 RSS
和文件大小。不得只报告 ns/op。

## 14. 风险与控制

| 风险 | 控制措施 |
| --- | --- |
| Page 变小导致压缩率下降 | 16/32/64 KiB 实测，文件大小设硬门槛 |
| 多一层 Page Directory 增加复杂度 | 定长目录、集中边界校验、golden 和 fuzz |
| Eager 紧凑索引增加查找 CPU | 热点读回归门槛，必要时保留每 shard block run 辅助表 |
| Lazy 深链产生多次 I/O | Index Page cache、批量聚合、负查询实验 |
| 去掉逐行 CRC 降低诊断粒度 | Page CRC + AEAD；Verify 报告精确 Page 和记录边界 |
| 排序索引增加 Commit 峰值 | 精确预分配、就地排序、10M peak benchmark |
| Page 加密 nonce 重用 | 独立域设计、格式级测试、AAD golden |
| Go pool 留存大 buffer | 分级 pool，超大行不入池，heap profile 门槛 |

## 15. 决策检查点

以下内容不能只凭设计偏好冻结，必须由原型数据决定：

1. 默认 PageSize 是 16、32 还是 64 KiB；
2. Row Index Page 是 2,048 还是 4,096 条；
3. Eager shard 使用 packed struct、struct-of-arrays 还是 block-run 编码；
4. Lazy 深链是否需要 Bloom filter；
5. Rows Page 的 metadata stream 是否在 Zstd 外再做 delta/RLE；
6. Index Page 是否复用数据压缩级别；
7. Page Directory 是否需要同时保存 Min/Max RowID。

每个检查点必须记录候选实现、测试数据、CPU/内存/文件大小结果和最终选择，随后再更新格式
规范。未经测量的候选布局不得进入 frozen 常量。

## 16. 推荐执行顺序

立即开始阶段 0 和阶段 1；两者提供可靠基线和无格式风险的收益。之后先完成 Rows Page，验证
冷读收益成立，再推进索引格式。原因是数据 Page 能解决当前最大且最确定的成本，而 Lazy
Index 的收益依赖实际数据规模和工作集，设计空间更大。

建议提交按以下边界拆分：

1. benchmark 与 profile 基线；
2. Bytes arena 与缓存预算修正；
3. 流式 IndexTxn consumer；
4. Rows Page 内存原型；
5. Rows Page 落盘格式；
6. Page cache 读路径；
7. 排序 Row Index Page；
8. Eager 紧凑索引；
9. Lazy Index；
10. 恢复、golden、规范和旧代码清理。

任何提交都必须保持 `go test ./...` 通过；格式切换提交允许集中更新 golden，但不得暂时保留
静默双格式分派。
# Implementation note (2026-09-09)

The proposed Lazy Row Index mode was removed before release. The implementation and public API are Eager-only; sorted Row Index Pages remain on disk, while Open/recovery always builds compact in-memory shards. The historical Lazy sections below are retained only as design history.
