# RowPack 单文件格式 v2 设计

> 状态：设计草案
> 日期：2026-09-08
> 设计原则：以现有实现为基础做最小改造；不兼容旧文件；保留现有逐行索引和读路径
> API：[GO_API_DESIGN_V2.md](GO_API_DESIGN_V2.md)
> 开发计划：[DEVELOPMENT_PLAN_V2.md](DEVELOPMENT_PLAN_V2.md)
> 决策：[ADR-003](adr/ADR-003.md)
> 风险清单：**[V2_DESIGN_RISKS.md](V2_DESIGN_RISKS.md)**（本稿已按 R1–R21 决议修订；实现前请先读）

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
| 备份需同时拷贝 `.rpk`+`.rpi` 且保持配对 | 备份/迁移/复制 = 单个 `.rpk`，不产生第二个数据块 |
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

**M0 决议（R15）**：v2 FileHeader 字节布局沿用 v1（含加密字段预留区 offset 64..120，
HeaderCRC32C@120，ReservedCRC@124），仅 Magic=`ROWPACK2`、Major=2 不同。
`RequiredFeatures` 位图沿用 v1 的四能力位（TypedTupleV1/Zstd/MetadataBlock/Delta，即
0x0F），不重定义：单文件化是载体变化，能力语义不变，CheckVersion 的 0x0F 掩码检查原样
保留。v1 的 `RequiredFeaturesV1` 常量语义可直接复用。

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

**M0 决议（R14）**：BlockHeader 保持 v1 的 64 字节布局（含 KeyEpoch@56），**不增加
disk RowID envelope 字段**；批量 planner 的 MinRowID/MaxRowIDExclusive 由内存索引在
`Apply` 时按 RowIndexEntry 集合派生 per-(Snapshot, Table, Block)，MaxRowIDExclusive 在
MaxRowID=MaxUint64 时用 0（无上界）表达，metadata block 无 envelope。

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

- DataSnapshotStart/End 改为同一文件内 Snapshot 数据区范围，**语义沿用 v1**：
  `DataEnd` 指 SnapshotFooter 之后（即整个 SnapshotTxn 的结束，IndexTxn 属于该范围），
  这样 `dataFooterVerifier` 的 `footer = DataEnd - SnapshotFooterSize` 无需改动（R8）；
- IndexTxnStart/End 是同一文件 offset，且从 v1 的 informational 变成**权威校验字段**：
  每个 txn 必须满足 `IndexTxnEnd == IndexTxnStart + IndexTxnHeaderSize + BodyBytes + IndexTxnFooterSize`
  且 `SnapshotEndOffset == IndexTxnEndOffset + SnapshotFooterSize`，任一不满足即该 txn 视为损坏（R8）；
- IndexTxnFooter 不再承担最终提交语义（提交权威见 §7）；
- IndexTxn 必须位于对应 Snapshot 的 Blocks 之后、SnapshotFooter 之前；
- Entry 继续引用同一文件中更早的 Block offset；
- Row Index 继续提供 `(SnapshotID, TableID, RowID) → BlockID, ItemOrdinal`。

IndexTxn 内的 SnapshotID/offset/CRC 字段若在 v2 布局精简（删冗余数据文件字段）中变化，
必须同步修改 ParseTxn/Replay/verifier 三处的读取契约，并保持入口尺寸 8 字节对齐（R16）。

IndexTxn 是派生导航结构。SnapshotFooter 有效但 IndexTxn 内容损坏时，可以扫描本事务的
Block 重建内存索引；是否允许正常 Open 自动修复写回，应留给 Rewrite，不原地覆盖。

## 7. SnapshotFooter

SnapshotFooter 是整个 SnapshotTxn 的最终提交标志，**固定 144 字节**（M0 冻结布局）：

```text
offset  size  field
0       8     MagicSnapshotFtr ("RPKSNAPF")
8       4     size = 144
12      1     SnapshotType
13      3     reserved (0)
16      8     SnapshotID
24      8     ParentSnapshotID
32      8     PreviousFooterOffset
40      8     SnapshotStartOffset
48      8     BlocksStartOffset
56      8     BlocksEndOffset
64      8     IndexTxnStartOffset
72      8     IndexTxnEndOffset
80      8     SnapshotEndOffset
88      8     FirstBlockID
96      4     BlockCount
100     4     MetadataBlockCount
104     8     RowRecordCount
112     8     RawBytes
120     8     StoredBytes
128     4     BlocksCRC32C
132     4     IndexTxnCRC32C
136     4     FooterCRC32C
140     4     reserved (0)
```

约束：`BlocksStartOffset = SnapshotStartOffset + SnapshotHeaderSize`；当快照没有任何块时
（空 DELTA），`BlocksEndOffset = IndexTxnStartOffset = SnapshotHeader 之后`；
`SnapshotEndOffset = IndexTxnEndOffset + SnapshotFooterSize`。

关键约束：

- `SnapshotEndOffset` 等于 Footer 后对齐位置；
- `PreviousFooterOffset` 严格指向前一个已提交 Footer；
- FULL 的 ParentSnapshotID=0；**允许任意时刻提交新 FULL**（取消 v1“FULL 固定 id=1”的
  约束，SnapshotID 一律取全局递增计数器；其 Depth 重置为 1，可见性不再沿祖先链解析），
  后续 DELTA 继续以它为新父链起点（R7）；
- DELTA 的 ParentSnapshotID 必须存在且小于当前 SnapshotID；
- Blocks 和 IndexTxn 的 offset 必须位于当前 SnapshotTxn 内且不重叠，且
  `BlocksStart < BlocksEnd <= IndexTxnStart < IndexTxnEnd < SnapshotEnd` 严格递增；
- BlocksCRC32C 按本事务 Block 的**物理写入顺序**拼接各 Block Header 的 HeaderCRC32C
  后计算的 CRC（与扫描顺序一致）；`StoredBytes` 等仅为诊断字段，权威值由重放的索引条目重算；
- Footer 同时绑定 Block Header CRC 序列和 IndexTxn 落盘字节的 CRC。

**提交权威（R3）**：快照是否提交只看 SnapshotFooter 自身 FooterCRC32C 有效、且其记录的
offset 自洽并在文件范围内。`BlocksCRC32C`/`IndexTxnCRC32C` 绑定失败只改变索引可用性
（触发 §10.2 内存重建），不改变提交事实；Block 数据损坏（块头/密文认证/解压长度/Raw
CRC）是硬错误，绝不因索引可重建而跳过。

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
4. 按物理顺序（或 Footer 链顺序）对**每个 committed Snapshot 独立**读取其 IndexTxn 区段：
   - 校验 SnapshotID、区间自洽（§6 边界强制）、Entry CRC 和 Footer 绑定 CRC；
   - 成功 → Apply；
   - 失败 → 该 snapshot 判定 IndexTxn 损坏：按 §10.2 从 Blocks 内存重建并 Apply，
     报告 `IndexRebuiltInMemory`，**继续处理下一个 snapshot**（R2：v2 不能沿用 v1
     “首个坏 txn 即停、其后全部丢弃”的重放语义）；
5. 派生 SchemaIndex 并发布 Store 状态。

打开仍会重放全部 Row Index，因此 v2 首版的 Open 时间和内存接近现有实现。**注意：索引
重放必须按 IndexTxn 区段 ReadAt（或依赖 mmap 按需分页），禁止对整文件 ReadAll——单文件
包含数据 Block，整文件读入会把 Open 峰值内存放大到与数据同量级（R5）**。单文件重构不同
时引入惰性索引，以避免扩大风险。

后续若要降低 Open 内存，应优化 `index.View` 的紧凑结构或增加只影响运行时的分页加载，
不改变 v2 提交权威。

## 10. 恢复

### 10.1 未提交尾部

最后一个有效 Footer 之后的任何 Header、Block 或 IndexTxn 都是未提交尾部：

- 读写 Open：截断（先退映射再截断，Windows 上活动映射会阻止截断，R6）；
- 只读 Open：忽略并报告；
- 不发布其中任何数据或索引。

**物理扫描协议（R1）**：所有恢复/打开扫描必须识别四类固定结构：

```text
SnapshotHeader (RPKSNAPH)  → 开始新 snapshot
BlockHeader    (RPKBLOCK)  → 按 StoredSize 跳过 payload
IndexTxnHeader (RPITXNBH)  → 按 BodyBytes 跳过，随后必须出现 IndexTxnFooter (RPITXNEF)
SnapshotFooter (RPKSNAPF)  → 结束当前 snapshot
```

未知结构出现在最后一个有效 Footer 之前 → 中间损坏硬错；之后 → 未提交尾部。空 DELTA
（0 个 Block）时 IndexTxnHeader 直接跟在 SnapshotHeader 之后，同样适用。

### 10.2 IndexTxn 损坏

若 Footer 和 Blocks 有效但 IndexTxn 校验失败（提交判定见 §7 的 R3 条款）：

- 数据提交事实仍由 Footer 决定；
- 扫描当前 SnapshotTxn 的 Block（范围由 Footer 的 BlocksStart/BlocksEnd 给出）和目录
  重建内存 IndexTxn；
- 只读模式允许继续读取并报告 `IndexRebuiltInMemory`；
- 读写模式不原地覆盖，必要时通过 Rewrite 生成新文件；
- 重建路径走只读解密（decrypter），不依赖写路径 encCipher；加密 store 打开时已强制
  KeyProvider（R13）；
- 重建后同一文件仍会携带损坏的 IndexTxn 字节：每次 Open 都会重复重建，属预期成本，
  由 Rewrite/checkpoint 收敛。

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
| Open Replay | 基本持平，可能因单 mmap 略有改善；前提是重放按区段 ReadAt/按需分页，**不得整文件 ReadAll**（R5） |
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

加密 IndexTxn 时：Body 加密，Header/Footer 保留最小明文导航字段（Header/Footer 永不需
密钥，R1 扫描与 Footer 校验才成立），AAD 绑定 StoreUUID、SnapshotID、IndexTxn offset 和
长度。

**nonce 域分离（R11）**：nonce 唯一性不依赖 AAD，必须在 nonce 字段内部显式分区：

```text
Block nonce : KeyEpoch(31bit) ‖ 0 ‖ BlockID(8B)
Index nonce : KeyEpoch(31bit) ‖ 1 ‖ TxnSequence(8B)   // bit31 = 域标志
```

否则 TxnSequence 与某个 BlockID 数字相等即 nonce 复用，AES-GCM 灾难性失败。

**索引 CRC 覆盖落盘字节（R12）**：Footer 的 IndexTxnCRC32C 一律对落盘字节计算（加密
store 即密文），保证撕裂/位腐的密文在**无密钥**路径即可检出（走 §10.2 重建），不需要等到
解密时才发现。

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

1. FileHeader、SnapshotFooter 的精确字节尺寸和 offset（注意保持 8 字节对齐，R16）；
2. IndexTxn 是否保持现有二进制布局或删除冗余数据文件字段（牵动 ParseTxn/Replay/verifier 三处契约，R8）；
3. IndexTxn Body 在加密 Store 中是否强制加密（建议强制，R11/R12 已给出配套 spec）；
4. Footer 尾部搜索的初始窗口和退化策略（建议正向结构扫描为主路径，R4）；
5. `Rewrite` 是否进入首版公共 API（成功/失败边界见 R20）；
6. 单文件路径是否允许调用方直接传入 `.rpk` 后缀（须定死，防 `foo.rpk.rpk`，R21）。

## 18. 评审已确认的边界约定

本节汇总 V2_DESIGN_RISKS.md 评审后直接写入本文档的决议（编号对应风险清单）：

- **R1**：§10.1 四类结构物理扫描协议；
- **R2**：§9 逐 snapshot 独立校验/重建/继续的重放算法 + §6 区间边界强制；
- **R3**：§7 “提交事实”与“索引有效性”两维分离的判定条款；
- **R5**：§9/§12 Open 禁止整文件 ReadAll；
- **R7**：§7 允许后续 FULL checkpoint（取消 v1 id=1 强制）；
- **R8**：§6 DataEnd 沿用 v1 语义 + IndexTxn offset 权威化；
- **R11/R12**：§13 nonce 位内域分离 + 索引 CRC 覆盖落盘字节；
- **R14**：Block RowID envelope 由内存索引派生，不进磁盘块头（待 M4 前由 M0 正式确认）。
