# RowPack 单文件格式 v2 设计

> 状态：设计草案
> 日期：2026-09-08
> 设计原则：以现有实现为基础做最小改造；不兼容旧文件；保留现有逐行索引和读路径
> API：[GO_API_DESIGN_V2.md](GO_API_DESIGN_V2.md)
> 开发计划：[DEVELOPMENT_PLAN_V2.md](DEVELOPMENT_PLAN_V2.md)
> 决策：[ADR-003](adr/ADR-003.md)

## 1. 结论

v2 将原 `.rpk` 数据流和 `.rpi` IndexTxn 流交错写入一个 `<base>.rpk` 文件。不是重新
设计 Page、B+Tree 或稀疏索引，而是复用现有：

- SnapshotHeader、Rows/Metadata Block；
- Snapshot、Block、Metadata、Row Index Entry；
- IndexTxn Builder、重放和 `index.View`；
- Get、Scan、DELTA 父链和 Tombstone 解析；
- Zstd、AES-256-GCM、Cache 和 TypedTuple。

每个 SnapshotTxn 末尾的 SnapshotFooter 是数据与索引共同提交的唯一权威标志。

## 2. v1 问题和 v2 对应改进

| v1 问题 | v2 改进 |
| --- | --- |
| `.rpk`、`.rpi` 两个文件 | 单个 `.rpk` |
| 两次 fsync | 数据、IndexTxn、Footer 后一次 fsync |
| 数据领先索引的跨文件窗口 | IndexTxn 位于 Footer 前，同一事务提交 |
| UUID 配对和文件缺失 | 不再需要文件配对 |
| 双文件 mmap/句柄生命周期 | 单 Appender、单 mmap |
| 重建 `.rpi` 需要临时文件和 rename | 从同一文件重放或整体 Rewrite |
| 单行 Get 依赖 Row Index | 保留现有 Row Index，性能不退化 |
| 1M 行索引约 22 MiB 常驻 | v2 首版保持；后续单独优化内存表示 |
| 单行冷读需解压整个 256 KiB Block | v2 首版保持，不扩大本次格式重构范围 |
| 大 Scan 解压内存较高 | 通过缓存/流式缓冲优化，不改变磁盘布局 |

v2 的首要目标是消除双文件成本，同时尽量不改动已经稳定的性能路径。Block 内分页等更大
变更不纳入本版。

## 3. 单文件布局

```text
[FileHeader]

[SnapshotTxn 1]
  [SnapshotHeader]
  [Metadata/Rows Blocks ...]
  [IndexTxnHeader]
  [SnapshotIndexEntry]
  [MetadataIndexEntry ...]
  [BlockIndexEntry ...]
  [RowIndexEntry ...]
  [IndexTxnFooter]
  [SnapshotFooter]

[SnapshotTxn 2]
  ...

[optional uncommitted tail]
```

SnapshotFooter 必须是事务最后一个固定结构。IndexTxn 完整但 Footer 缺失时，整个事务仍未
提交。

## 4. FileHeader

建议仍为固定 128 字节，Magic 使用 `ROWPACK2`，Major=2。保留现有 DataFileHeader 中：

```text
StoreUUID
CreatedUnixNano
RequiredFeatures
OptionalFeatures
DefaultBlockSize
DefaultCompression
DefaultRowEncoding
EncryptionAlgorithm
NonceScheme
KeyID
HeaderCRC32C
```

删除独立 IndexFileHeader。StoreUUID 仍可作为文件身份、缓存键和加密 AAD 的一部分，但
不再用于文件配对。

Header 创建后不更新，保持 append-only。

## 5. SnapshotHeader 与 Block

SnapshotHeader、BlockHeader、Rows Payload、Metadata Payload 和 TypedTuple 优先保持现有
字段和编码不变，仅通过 Major Version 区分外层事务语义。

Block 仍满足：

- 一个 Block 只属于一个 Snapshot；
- Rows Block 只属于一个 Table；
- 先压缩后 AES-256-GCM 加密；
- 独立 StoredSize、RawSize、CRC 和认证；
- Row Directory 保存 RowID、ChangeType、SchemaVersion 和记录位置。

保持 Block 格式可以最大程度复用 writer、reader、cache、golden 构造逻辑和性能优化。

## 6. 内嵌 IndexTxn

v2 直接复用现有 IndexTxn 的逻辑内容：

```text
IndexTxnHeader
SnapshotIndexEntry × 1
MetadataIndexEntry × N
BlockIndexEntry × B
RowIndexEntry × R
IndexTxnFooter
```

主要变化：

- DataSnapshotStart/End 改为同一文件内 Snapshot 数据区范围；
- IndexTxnStart/End 是同一文件 offset；
- IndexTxnFooter 不再承担最终提交语义；
- IndexTxn 必须位于对应 Snapshot 的 Blocks 之后、SnapshotFooter 之前；
- Entry 继续引用同一文件中更早的 Block offset；
- Row Index 继续提供 `(SnapshotID, TableID, RowID) → BlockID, ItemOrdinal`。

IndexTxn 是派生导航结构。SnapshotFooter 有效但 IndexTxn 内容损坏时，可以扫描本事务的
Block 重建内存索引；是否允许正常 Open 自动修复写回，应留给 Rewrite，不原地覆盖。

## 7. SnapshotFooter

SnapshotFooter 是整个 SnapshotTxn 的最终提交标志，建议扩展为至少包含：

```text
SnapshotID
ParentSnapshotID
PreviousFooterOffset
SnapshotStartOffset
BlocksStartOffset
BlocksEndOffset
IndexTxnStartOffset
IndexTxnEndOffset
SnapshotEndOffset
FirstBlockID
BlockCount
MetadataBlockCount
RowRecordCount
RawBytes
StoredBytes
BlocksCRC32C
IndexTxnCRC32C
FooterCRC32C
```

关键约束：

- `SnapshotEndOffset` 等于 Footer 后对齐位置；
- `PreviousFooterOffset` 严格指向前一个已提交 Footer；
- FULL 的 ParentSnapshotID=0；
- DELTA 的 ParentSnapshotID 必须存在且小于当前 SnapshotID；
- Blocks 和 IndexTxn 的 offset 必须位于当前 SnapshotTxn 内且不重叠；
- Footer 同时绑定 Block Header CRC 序列和 IndexTxn 原始字节 CRC。

Footer 完整且 CRC 正确，表示数据与索引已作为一个事务提交。

## 8. 提交协议

### SyncCommit

```text
1. append SnapshotHeader
2. append Metadata/Rows Blocks
3. build and append IndexTxn
4. append SnapshotFooter
5. fsync 单文件
6. 原子发布 index.View
7. 返回成功
```

相比现有双文件协议，IndexTxn Builder 和 `View.Apply` 可以复用，只调整写入目标和 offset。

步骤 5 开始发生 I/O 错误时，提交结果可能未知；调用者按 SnapshotID 查询，不得盲目重放
非幂等业务。

### AsyncCommit

顺序相同但不主动 fsync。进程内只能发布完整 Footer 对应的事务；机器掉电允许丢失最近
提交。

### Abort

没有 SnapshotFooter 的尾部不可见。读写模式可以立即截断到 SnapshotHeader 之前，也可以
在下次 Open 时截断。

## 9. 打开

正常打开流程：

1. 校验 FileHeader；
2. 从文件尾定位最后一个有效 SnapshotFooter；
3. 沿 `PreviousFooterOffset` 建立 Snapshot 目录；
4. 按物理顺序或 Footer 链顺序读取每个 IndexTxn；
5. 校验 IndexTxn 的 SnapshotID、offset、Entry CRC 和 Footer 绑定 CRC；
6. 重放到现有 `index.View`；
7. 派生 SchemaIndex 并发布 Store 状态。

打开仍会重放全部 Row Index，因此 v2 首版的 Open 时间和内存接近现有实现。单文件重构不
同时引入惰性索引，以避免扩大风险。

后续若要降低 Open 内存，应优化 `index.View` 的紧凑结构或增加只影响运行时的分页加载，
不改变 v2 提交权威。

## 10. 恢复

### 10.1 未提交尾部

最后一个有效 Footer 之后的任何 Header、Block 或 IndexTxn 都是未提交尾部：

- 读写 Open：截断；
- 只读 Open：忽略并报告；
- 不发布其中任何数据或索引。

### 10.2 IndexTxn 损坏

若 Footer 和 Blocks 有效但 IndexTxn 校验失败：

- 数据提交事实仍由 Footer 决定；
- 扫描当前 SnapshotTxn 的 Block 和目录重建内存 IndexTxn；
- 只读模式允许继续读取并报告 `IndexRebuiltInMemory`；
- 读写模式不原地覆盖，必要时通过 Rewrite 生成新文件。

### 10.3 数据损坏

已提交范围内 Block Header、密文认证、解压长度或 Raw CRC 失败属于中间数据损坏，必须
返回错误，不得因索引可重建而跳过。

### 10.4 Footer 搜索

从文件尾按固定对齐向前搜索 Footer Magic，并同时验证：

- 固定长度和 CRC；
- SnapshotEndOffset 与候选位置一致；
- PreviousFooterOffset 递减且对齐；
- SnapshotID 单调；
- 当前事务所有 offset 落在合法范围。

有限窗口内找不到时应扩大搜索或退化为顺序扫描，不能把较大的未提交尾部误判为无有效
Snapshot。

## 11. Get、Scan 与批量读取

### Get

继续复用当前 Row Index 和父链解析：

```text
SnapshotID + TableID + RowID
→ index.View.ResolveRow
→ BlockID + ItemOrdinal
→ Block Cache / ReadAt
→ 解密、解压、ParseRowAt
```

因此单行热读预计保持当前水平；冷读仍受整 Block 解压成本影响。

### Scan

继续使用每层有序 Row Index 分片和多路合并，过滤覆盖记录与 Tombstone。单层 FULL 快路径
继续保留。

### 批量读取

新增批量路径，但复用 Row Index：

```text
RowIDs/Ranges
→ ResolveRow
→ 按 BlockID 分组去重
→ 按物理 offset 排序
→ 每个 Block 读取、解压、校验一次
→ 按 ItemOrdinal 解码多行
→ 恢复 RowID 或请求顺序
```

不需要改变磁盘 Block 格式即可获得批量读取收益。

## 12. 性能预期

| 指标 | 预期 |
| --- | --- |
| SyncCommit | 由两次 fsync 降为一次 |
| FULL 写入 | 不低于现有基线 90% |
| Get 热读 | 基本持平 |
| Get 冷读 | 基本持平，后续单独优化 |
| Scan | 基本持平 |
| Open Replay | 基本持平，可能因单 mmap 略有改善 |
| 索引内存 | 基本持平 |
| 批量读取 | 同 Block 多行只解压一次 |
| 文件管理 | 两文件降为一文件 |

本次设计不把“单文件”包装成所有性能问题的解决方案。单文件明确改善提交和资源管理；
单行冷读、索引常驻和 Scan 内存需要后续独立优化并用基准证明。

## 13. 加密

只使用 AES-256-GCM。Rows/Metadata Block 的加密格式保持现有设计；IndexTxn 是否加密需
单独决定：

- 明文 IndexTxn：Open 和点查不需要先解密索引，但泄露 RowID、表和数据分布；
- 加密 IndexTxn：保护导航信息，但 Open 必须提供密钥。

建议加密 Store 同时加密 IndexTxn Body，Header/Footer 保留最小导航字段，AAD 绑定
StoreUUID、SnapshotID、IndexTxn offset 和长度。索引 nonce 必须与 Block nonce 使用不同
域分隔。

## 14. 格式替代

v2 直接替代旧格式：

- `Create` 只创建单个 `.rpk`；
- `Open` 只接受 v2 Magic/Major；
- 不读取双文件格式；
- 不提供旧格式迁移 API；
- `RebuildIndex` 删除，替换为内存恢复和可选 `Rewrite`；
- golden、恢复测试和性能基线全部重新建立。

## 15. 实施顺序

```text
冻结 Footer/IndexTxn offset
→ 单 Appender 与 FileHeader
→ FULL 单文件写入/重开
→ DELTA 与恢复
→ 批量读取
→ IndexTxn 加密
→ golden/fuzz/race/性能冻结
```

## 16. 验收标准

- Store 只有一个持久化文件；
- 数据、IndexTxn 和 Footer 在一次 fsync 中原子提交；
- 现有 Get、Scan、Schema、FULL/DELTA 语义保持；
- 单行 Get 性能较当前基线回退不超过 10%；
- IndexTxn 损坏时可由当前事务 Block 重建内存索引；
- 任一写入边界崩溃后只看到提交前或提交后的完整 Snapshot；
- 批量读取同一 Block 只解压和校验一次；
- 全部 17 种类型完成 Store 写入、关闭、重开、Get 和 Scan 往返；
- 新格式通过 golden、fuzz、race、故障注入和完整性能测试。

## 17. 待冻结决策

1. FileHeader、SnapshotFooter 的精确字节尺寸和 offset；
2. IndexTxn 是否保持现有二进制布局或删除冗余数据文件字段；
3. IndexTxn Body 在加密 Store 中是否强制加密；
4. Footer 尾部搜索的初始窗口和退化策略；
5. `Rewrite` 是否进入首版公共 API；
6. 单文件路径是否允许调用方直接传入 `.rpk` 后缀。
