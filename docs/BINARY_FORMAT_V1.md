# RowPack 单文件格式 v1 设计

> 状态：已实现并冻结（v1 为唯一格式线）
> 日期：2026-09-08
> 设计原则：数据与索引同文件；保留逐行索引和稳定的读路径
> 配套文档：[GO_API_DESIGN_V1.md](GO_API_DESIGN_V1.md) · [METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md) · [INDEX_TXN_FORMAT_V1.md](INDEX_TXN_FORMAT_V1.md) · [ENCRYPTION_V1.md](ENCRYPTION_V1.md)

## 1. 结论

v1 将数据流和 IndexTxn 流交错写入一个 `<base>.rpk` 文件，不重新设计 Page、B+Tree 或稀疏索引，
而是复用稳定的既有结构：SnapshotHeader、Rows/Metadata Block；Snapshot/Block/Metadata/Row Index
Entry；IndexTxn Builder、重放和 `index.View`；Get、Scan、DELTA 父链和 Tombstone 解析；Zstd、
AES-256-GCM、Cache 和 TypedTuple。每个 SnapshotTxn 末尾的 SnapshotFooter 是数据与索引共同提交的
唯一权威标志。

## 2. 设计目标与取舍

v1 用单文件取代从未发布的双文件草案（`.rpk`+`.rpi`）：单个 `.rpk`（单 Appender、单 mmap）、
数据/IndexTxn/Footer 同事务一次 fsync 提交、不再需要文件配对（备份/迁移/复制即拷贝该文件），索引重建改为从同一文件重放或
整体 Rewrite，单行 Get 仍依赖 Row Index、性能不退化。Block 内分页（页容器）、排序 Row Index
Page 与紧凑 SoA/Eager shard 索引均已纳入 v1；单行冷读、索引常驻和 Scan 内存的后续优化不改变
本规范冻结的磁盘布局。

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

SnapshotFooter 必须是事务最后一个固定结构；IndexTxn 完整但 Footer 缺失时，整个事务仍未提交。

## 4. FileHeader

固定 128 字节，Magic `ROWPACK1`，Major=1、Minor=0。字段布局：

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

StoreUUID 作为文件身份、缓存键和加密 AAD 的一部分，不用于文件配对。不存在独立的索引文件头。
Header 创建后不更新，保持 append-only。加密字段预留区位于 offset 64..120，`HeaderCRC32C`@120，
`ReservedCRC`@124。`RequiredFeatures` 为四能力位（TypedTupleV1/Zstd/MetadataBlock/Delta，即
0x0F）：能力语义不随单文件化改变，`CheckVersion` 的 0x0F 掩码检查原样保留，常量
`RequiredFeaturesV1` 即该位集。

## 5. SnapshotHeader 与 Block

SnapshotHeader、BlockHeader、Metadata Payload 和 TypedTuple 使用固定字段和编码。Rows Block 为
**页容器**布局（见下），Metadata Block 为整块压缩的 Metadata Payload，Snapshot Meta Block 为整
块压缩的不透明值。Block 约束：一个 Block 只属于一个 Snapshot；Rows Block 只属于一个 Table；先
压缩后 AES-256-GCM 加密（**Metadata / Snapshot Meta Block 整容器密封**，**Rows Block 逐页密
封**）；独立 StoredSize、RawSize、CRC 和认证。

BlockKind 分配：`1 = Rows`、`2 = Metadata`（引擎解析的 TLV 记录目录）、`3 = Snapshot Meta`
（引擎**不**解析的每快照值）。

### 5.0 Snapshot Meta Block

一个快照至多一个 `BlockKind = 3` 的 Block，承载该快照的整块元信息（应用版本、采集参数、外部清
单、人类可读日志等「每快照一份」的内容）。它与 Metadata Block 完全不同，两者不可混用：

|  | Metadata Block（kind 2） | Snapshot Meta Block（kind 3） |
| --- | --- | --- |
| 内容 | Metadata Payload：目录 + TLV 记录 | 调用方给定的原始字节，无信封、无目录 |
| 每快照数量 | 可多个（按大小滚动） | 至多一个 |
| 是否解析 | 是（行解码契约） | 否（只读回字节） |

- payload 即值本身：`RawSize = len(value)`、`RawCRC32C = CRC32C(value)`、`ItemCount = 1`、
  `TableID = 0`；写路径在压缩膨胀时回退为 `CompressionNone`，使「长度合规」等价于「可提交」；
- 长度上限为 `Limits.MaxRawBlockBytes`（值成为一块原始 payload），写入时在发布前校验，越界返回
  `ErrInvalidArgument`，不产生任何字节；
- 定位与 Rows/Metadata Block 完全相同：`BlockIndexEntry` 记 `BlockKind`，索引重放和
  `IndexTxn` 内存重建（`ScanBlocks` 级别的块扫描）都不需要为它分支；
- 读取沿父链解析，取**最近祖先**的值：自己未设置则其父的值继续可见，任意时刻一条链上只有一个
  值；整条链都没有时为「无」（`nil`），不是错误。

### 5.1 Rows Block 页容器

Rows Block 的逻辑块（写入/统计/快照组织单位）与物理压缩页（读取/解压/缓存单位）分离。一个 Rows
Block 的 payload 是页容器：

```text
[RowsBlockHeader] 24 B 固定：PageCount / DirectoryBytes / TotalRecords
[RowsPageDirEntry × N] 56 B 每页（明文，供读取器定位页）
[stored page 0] 每页独立压缩（+可选整套容器密封），由自身 PageCRC 校验
[stored page 1] …
```

- 外层 BlockHeader 只做聚合：`RawSize = Σ页 RawSize`、`StoredSize = 容器长`、
  `RawCRC32C = 容器 [头+目录] 明文 CRC`；各页由自身 PageCRC 校验，块级不再有整载荷 raw CRC。
- `RowsPageDirEntry` 含 PageOrdinal / FirstRecordOrdinal / RecordCount / StoredOffset /
  StoredSize / RawSize / MinRowID / MaxRowID / PageCRC32C / Flags（bit0=超大连行页）。
- 压缩页内部的 `RowsPage` 使用一次性列流（RowID zigzag delta / end-offset delta / SchemaVersion
  RLE / ChangeType 2bit / body-only TypedTuple），不含冗余的逐行 `RowRecordHeader`
  （`RowDirectoryEntry` 仍保留为块级目录，见 `FlushedBlock.Rows`）；Page CRC 覆盖解压后完整页。
- 默认 PageSize = 32 KiB，是建库后不可变的写时分页参数；读取按容器目录定位页，不需要 PageSize。
- 超过 PageSize 的单行使用独立 Large Row Page（Flags bit0）。

### 5.2 Rows Page 加密（逐页 nonce）

每个 Rows Page **先压缩后单独 AES-256-GCM 密封**：页目录保持明文（供读取器无需解密即可定位页），
每个 stored 页的字节 = `AEAD(压缩页) ‖ tag`，因此 `RowsPageDirEntry.StoredSize` = 压缩页长 +
`AESGCMTagLen`。

- **Nonce（96 位）**：HMAC-SHA256 派生自独立 page-nonce 子密钥，绑定
  `StoreUUID ‖ SnapshotID ‖ BlockID ‖ PageOrdinal ‖ KeyEpoch`，与块 nonce、IndexTxn nonce 和
  IndexChunk nonce 域分离（`internal/seal` 的 `NoncePage`）。
- **AAD**：`PageContext.AAD` 绑定 store UUID、SnapshotID/BlockID/TableID/Compression，页目录的
  PageOrdinal/FirstRecordOrdinal/RecordCount/StoredSize/RawSize/MinRowID/MaxRowID 和 KeyEpoch；
  StoredSize 取**密封后**长度，所以读取端按目录字段认证自洽。
- 未加密页不承担 tag 开销；同一页不会重放（nonce 域分离测试见
  `internal/seal/seal_page_test.go`）。
- 读取只 OPEN（认证）并解压所访问的那一页，加密块同样享受页级 I/O。

### 5.3 Row Index（排序 Row Index Page + Fence Directory）

行索引使用**排序 Row Index Page + Fence Directory**（见 §6），不使用旧双文件草案中的
`RowIndexEntry` chunk delta。`(SnapshotID, TableID, RowID) → BlockID, ItemOrdinal` 映射由该页
格式提供；Eager 模式在 Open 时把整行索引解码为紧凑 SoA shard，Lazy 模式只加载 Fence 并按需读页。
每个 `RowIndexPage` **按表切页**（一个页绝不跨表 run），使页内 `TableID` 唯一、
`MinRowID`/`MaxRowID` 属于该表，Fence 成为 `(TableID, RowID)` 的单调二叉索引——这是 Lazy 二叉
搜索正确的前提（跨表页会让全局 `MinRowID` 随页非单调）。

BlockHeader 保持 64 字节布局（含 KeyEpoch@56），**不增加 disk RowID envelope 字段**；批量
planner 的 MinRowID/MaxRowIDExclusive 由内存索引在 `Apply` 时按 RowIndexEntry 集合派生
per-(Snapshot, Table, Block)，MaxRowIDExclusive 在 MaxRowID=MaxUint64 时用 0（无上界）表达，
metadata block 无 envelope。

## 6. 内嵌 IndexTxn

行索引部分从旧双文件草案的 `RowIndexEntry` chunk delta 切换为**排序 Row Index Page + Fence
Directory**。正文布局：

```text
IndexTxnHeader (80B, RowIndexPageCount @ offset 12..16)
SnapshotChunk // 定长 SnapshotIndexEntry，chunk seq 0
MetadataChunk × A // 定长条目，chunk seq 1..A
BlockChunk × B // 定长条目，chunk seq A+1..A+B
ChunkDirectory // 明文 (A+B+1) × 32B，chunk 定位
IndexPage × N // 每页独立 zstd（复用 store 压缩级别），加密 +16B tag
RowIndexFenceEntry × N // 明文 52B，由正文 CRC 认证
IndexTxnFooter (80B)
```

- `RowIndexPageCount`（N）存于 `IndexTxnHeader` offset 12..16 的 reserved 字；页数必须 ≤
  `RowEntryCount`（每页 ≥1 条）。offset 76..80 的 reserved 字留给加密 store 的 `KeyEpoch`。
- 每个 `RowIndexPage` 是 `(TableID, RowID)` 升序的 ≤4096 条记录，且**按表切页**（末页可少、表
  边界可产生较小页）。页头为冻结 `RowIndexPageHeader`，五条流（TableID run / RowID 非负 uvarint
  delta / BlockID run / ItemOrdinal zigzag delta / ChangeType 2bit），页 CRC 覆盖流区。
- 每个 `RowIndexFenceEntry`（52B）带 `SnapshotID/TableID/Min/MaxRowID/StoredOffset(正文内)/
  StoredSize(压缩+tag)/RawSize/EntryCount/PageCRC32C`；Fence 明文，按 `StoredOffset` 递增排列，
  用于按 RowID 二分定位页后 OPEN+decompress+decode。
- `RowIndexPageCount == 0` 表示快照无行条目。

IndexTxn 与 Snapshot 的关系：

- DataSnapshotStart/End 是同一文件内 Snapshot 数据区范围：`DataEnd` 指 SnapshotFooter 之后（即
  整个 SnapshotTxn 的结束，IndexTxn 属于该范围），`dataFooterVerifier` 的
  `footer = DataEnd - SnapshotFooterSize` 无需特判；
- IndexTxnStart/End 是同一文件 offset，且是**权威校验字段**：每个 txn 必须满足
  `IndexTxnEnd == IndexTxnStart + IndexTxnHeaderSize + BodyBytes + IndexTxnFooterSize` 且
  `SnapshotEndOffset == IndexTxnEndOffset + SnapshotFooterSize`，任一不满足即该 txn 视为损坏；
- IndexTxnFooter 不承担最终提交语义（提交权威见 §7）；
- IndexTxn 必须位于对应 Snapshot 的 Blocks 之后、SnapshotFooter 之前；
- Entry 引用同一文件中更早的 Block offset；
- Row Index 提供 `(SnapshotID, TableID, RowID) → BlockID, ItemOrdinal`。**ItemOrdinal 是记录在
  自身 Rows Block 内的位置序号**（从 0 起、跨页连续，等于该块 `RowsContainer.RecordAt` 的入参），
  必须与 `BlockID` 成对使用；它不是快照内的累计记录计数。由 Block 扫描重建内存索引时同样按
  「每块重新计数」写入——跨块累加会造出一张打开不报错、但把行解析到别的位置的错位索引。

IndexTxn 内的 SnapshotID/offset/CRC 字段如需精简，必须同步修改 ParseTxn/Replay/verifier 三处的
读取契约，并保持入口尺寸 8 字节对齐。IndexTxn 是派生导航结构：SnapshotFooter 有效但 IndexTxn
内容损坏时可扫描本事务的 Block 重建内存索引，不允许正常 Open 原地覆盖修复，必要时通过 Rewrite
生成新文件。

## 7. SnapshotFooter

SnapshotFooter 是整个 SnapshotTxn 的最终提交标志，**固定 144 字节**：

```text
offset size field
0 8 MagicSnapshotFtr ("RPKSNAPF")
8 4 size = 144
12 1 SnapshotType
13 3 reserved (0)
16 8 SnapshotID
24 8 ParentSnapshotID
32 8 PreviousFooterOffset
40 8 SnapshotStartOffset
48 8 BlocksStartOffset
56 8 BlocksEndOffset
64 8 IndexTxnStartOffset
72 8 IndexTxnEndOffset
80 8 SnapshotEndOffset
88 8 FirstBlockID
96 4 BlockCount
100 4 MetadataBlockCount
104 8 RowRecordCount
112 8 RawBytes
120 8 StoredBytes
128 4 BlocksCRC32C
132 4 IndexTxnCRC32C
136 4 FooterCRC32C
140 4 reserved (0)
```

约束：`BlocksStartOffset = SnapshotStartOffset + SnapshotHeaderSize`；当快照没有任何块时（空
DELTA），`BlocksEndOffset = IndexTxnStartOffset = SnapshotHeader 之后`；
`SnapshotEndOffset = IndexTxnEndOffset + SnapshotFooterSize`。

关键约束：`SnapshotEndOffset` 等于 Footer 后对齐位置；`PreviousFooterOffset` 严格指向前一个已提交
Footer；FULL 的 ParentSnapshotID=0，**允许任意时刻提交新 FULL**（SnapshotID 一律取全局递增
计数器；其 Depth 重置为 1，可见性不再沿祖先链解析），后续 DELTA 继续以它为新父链起点；DELTA 的
ParentSnapshotID 必须存在且小于当前 SnapshotID；Blocks 和 IndexTxn 的 offset 必须位于当前
SnapshotTxn 内且不重叠，且 `BlocksStart < BlocksEnd <= IndexTxnStart < IndexTxnEnd <
SnapshotEnd` 严格递增；BlocksCRC32C 按本事务 Block 的**物理写入顺序**拼接各 Block Header 的
HeaderCRC32C 后计算（与扫描顺序一致），`StoredBytes` 等仅为诊断字段，权威值由重放的索引条目
重算；Footer 同时绑定 Block Header CRC 序列和 IndexTxn 落盘字节的 CRC。

**提交权威**：快照是否提交只看 SnapshotFooter 自身 FooterCRC32C 有效、且其记录的 offset 自洽并
在文件范围内。`BlocksCRC32C`/`IndexTxnCRC32C` 绑定失败只改变索引可用性（触发 §10.2 内存重建），
不改变提交事实；Block 数据损坏（块头/密文认证/解压长度/Raw CRC）是硬错误，绝不因索引可重建而
跳过。

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

IndexTxn Builder 和 `View.Apply` 复用既有实现，只调整写入目标和 offset。步骤 5 开始发生 I/O
错误时，提交结果可能未知；调用者按 SnapshotID 查询，不得盲目重放非幂等业务。

### AsyncCommit

顺序相同但不主动 fsync。进程内只能发布完整 Footer 对应的事务；机器掉电允许丢失最近提交。

### Abort

没有 SnapshotFooter 的尾部不可见。读写模式可以立即截断到 SnapshotHeader 之前，也可以在下次 Open
时截断。

## 9. 打开

1. 校验 FileHeader；
2. 从文件尾定位最后一个有效 SnapshotFooter；
3. 沿 `PreviousFooterOffset` 建立 Snapshot 目录；
4. 按物理顺序（或 Footer 链顺序）对**每个 committed Snapshot 独立**读取其 IndexTxn 区段：校验
   SnapshotID、区间自洽（§6 边界强制）、Entry CRC 和 Footer 绑定 CRC；成功则 Apply；失败则判定
   该 snapshot 的 IndexTxn 损坏，按 §10.2 从 Blocks 内存重建并 Apply，报告
   `IndexRebuiltInMemory`，**继续处理下一个 snapshot**（不能沿用「首个坏 txn 即停、其后全部
   丢弃」的重放语义）；
5. 派生 SchemaIndex 并发布 Store 状态。

打开仍会重放全部 Row Index，因此 Open 时间和内存接近既有实现。**注意：索引重放必须按 IndexTxn
区段 ReadAt（或依赖 mmap 按需分页），禁止对整文件 ReadAll——单文件包含数据 Block，整文件读入会
把 Open 峰值内存放大到与数据同量级**。后续若要降低 Open 内存，应优化 `index.View` 的紧凑结构或
增加只影响运行时的分页加载，不改变提交权威。

## 10. 恢复

### 10.1 未提交尾部

最后一个有效 Footer 之后的任何 Header、Block 或 IndexTxn 都是未提交尾部：读写 Open 截断（先退
映射再截断，Windows 上活动映射会阻止截断）；只读 Open 忽略并报告；不发布其中任何数据或索引。

**物理扫描协议**：所有恢复/打开扫描必须识别四类固定结构：

```text
SnapshotHeader (RPKSNAPH) → 开始新 snapshot
BlockHeader (RPKBLOCK) → 按 StoredSize 跳过 payload
IndexTxnHeader (RPITXNBH) → 按 BodyBytes 跳过，随后必须出现 IndexTxnFooter (RPITXNEF)
SnapshotFooter (RPKSNAPF) → 结束当前 snapshot
```

未知结构出现在最后一个有效 Footer 之前 → 中间损坏硬错；之后 → 未提交尾部。空 DELTA（0 个
Block）时 IndexTxnHeader 直接跟在 SnapshotHeader 之后，同样适用。

### 10.2 IndexTxn 损坏

若 Footer 和 Blocks 有效但 IndexTxn 校验失败：数据提交事实仍由 Footer 决定；扫描当前
SnapshotTxn 的 Block（范围由 Footer 的 BlocksStart/BlocksEnd 给出）和目录重建内存 IndexTxn；
只读模式允许继续读取并报告 `IndexRebuiltInMemory`；读写模式不原地覆盖，必要时通过 Rewrite 生成
新文件；重建路径走只读解密（decrypter），不依赖写路径 encCipher，加密 store 打开时已强制
KeyProvider；重建后同一文件仍会携带损坏的 IndexTxn 字节，每次 Open 都会重复重建，属预期成本，
由 Rewrite/checkpoint 收敛。

### 10.3 数据损坏

已提交范围内 Block Header、密文认证、解压长度或 Raw CRC 失败属于中间数据损坏，必须返回错误，
不得因索引可重建而跳过。

### 10.4 Footer 搜索

从文件尾按固定对齐向前搜索 Footer Magic，并同时验证固定长度和 CRC；SnapshotEndOffset 与候选
位置一致；PreviousFooterOffset 递减且对齐；SnapshotID 单调；当前事务所有 offset 落在合法范围。
有限窗口内找不到时应扩大搜索或退化为顺序扫描，不能把较大的未提交尾部误判为无有效 Snapshot。

## 11. Get、Scan 与批量读取

### Get

```text
SnapshotID + TableID + RowID
→ index.View.ResolveRow
→ BlockID + ItemOrdinal
→ Block Cache / ReadAt
→ 解密、解压、ParseRowAt
```

单行热读保持既有水平；冷读按 Rows Page 解压（§5.1）。

### Scan

继续使用每层有序 Row Index 分片和多路合并，过滤覆盖记录与 Tombstone；单层 FULL 快路径保留。

### 批量读取

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

设计层面的硬预期：SyncCommit 一次 fsync；FULL 写入不低于双文件草案基线 90%；Get 热读与 Scan
基本持平，冷读按页解压消除整块读放大；索引用紧凑 SoA/Eager shard 表示；批量读取同 Block 只解压
一次。Open 重放按 IndexTxn 区段 ReadAt/按需分页，**不得整文件 ReadAll**。实测数值与回归门槛见
[PERFORMANCE_BASELINE_V1.md](PERFORMANCE_BASELINE_V1.md)；本设计不把「单文件」包装成所有性能
问题的解决方案。

## 13. 加密

只使用 AES-256-GCM，加密单位、AAD 与密钥细节见 [ENCRYPTION_V1.md](ENCRYPTION_V1.md)。
Header/Footer 永不需密钥（§10.1 扫描与 Footer 校验才成立）。两条格式级不变式：

**nonce 域分离**：nonce 唯一性不依赖 AAD，必须在 nonce 字段内部显式分区：

```text
Block nonce : KeyEpoch(4B, LE) ‖ BlockID(8B, LE)
Index nonce : (KeyEpoch | 0x80000000)(4B, LE) ‖ TxnSequence(8B, LE) // bit31 = 域标志
```

Block nonce 的 epoch 恒低于 2^31，index nonce 置 epoch 字最高位，因此二者永不碰撞；Rows Page 与
Index chunk 的 nonce 由 HMAC-SHA256 派生（ENCRYPTION §5）。

**索引 CRC 覆盖落盘字节**：`SnapshotFooter.IndexTxnCRC32C` 一律对落盘字节计算（加密 store 即
密文），保证撕裂/位腐的密文在**无密钥**路径即可检出（走 §10.2 重建），不需要等到解密时才发现。

## 14. 格式替代

v1 直接替代早期双文件草案：`Create` 只创建单个 `.rpk`；`Open` 只接受 v1 Magic/Major
（`ROWPACK1`/1）；不读取双文件格式；不提供旧格式迁移 API；`RebuildIndex` 删除，替换为内存恢复
和可选 `Rewrite`；golden、恢复测试和性能基线全部重新建立。

## 15. 格式不变式与冻结清单

以下不变式是 v1 磁盘格式的硬约束，由结构测试、golden 哈希和恢复测试锁定：

**布局与提交**

- Store 只有一个持久化文件；数据、IndexTxn 和 Footer 在一次 fsync 中原子提交；
- 四类固定结构的物理扫描协议（§10.1）覆盖全部恢复/打开路径；逐 snapshot 独立校验/重建/继续
  （§9），不因单个坏 IndexTxn 丢弃后续 snapshot；
- 「提交事实」与「索引有效性」两维分离（§7）：Footer 有效即提交，IndexTxn 损坏只触发重建；
- 允许任意时刻提交新 FULL，取消「FULL 固定 id=1」的约束（§7）。

**结构尺寸（8 字节对齐）**

| 结构 | 尺寸 |
| --- | --- |
| FileHeader | 128 |
| SnapshotHeader | 96 |
| SnapshotFooter | 144 |
| BlockHeader | 64 |
| RowDirectoryEntry | 24 |
| MetaPayloadHeader | 32 |
| MetaDirectoryEntry | 32 |
| RowsBlockHeader | 32 |
| RowsPageDirEntry | 56 |
| IndexTxnHeader | 80 |
| IndexTxnFooter | 80 |
| IndexChunkHeader | 64 |
| RowIndexFenceEntry | 52 |

**加密**：nonce 位内域分离（§13）使 Block/Index 域标志互斥、同密钥下 nonce 永不复用；索引 CRC
覆盖落盘字节（§13），无密钥路径即可检出密文损坏；加密 store 的 Open/Verify/Rebuild 必须提供
KeyProvider，否则返回 `ErrKeyRequired`。

**读取**：Open 禁止整文件 ReadAll（§9），索引重放按区段 ReadAt 或 mmap 按需分页；Rows Block 按
页读取/解压/缓存，页目录明文供读取器定位；批量读取同一 Block 只解压和校验一次（§11）。

**元数据**：引擎不内建数据库对象模型或方言语义；Canonical Schema 决定行解码，Source Metadata
以普通行数据存放在调用方自选 ns 的目录表里（见
[METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md) §9），不进入 TLV。
