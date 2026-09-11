# RowPack v1 需求规格说明书

> 文档状态：已实现并冻结（v1 为唯一格式/API 线）
> 产品名称：RowPack · 实现语言：Go · 目标版本：v1
> 更新时间：2026-09-06（2026-09-10 收敛为单文件 v1 口径）
> 配套设计：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md) · [GO_API_DESIGN_V1.md](GO_API_DESIGN_V1.md) · [METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md) · [INDEX_TXN_FORMAT_V1.md](INDEX_TXN_FORMAT_V1.md) · [ENCRYPTION_V1.md](ENCRYPTION_V1.md)

## 1. 文档目的

定义 RowPack v1 的产品范围、核心概念、功能/非功能需求、外部接口、数据一致性规则及验收标准，
作为设计、开发、测试和验收的共同依据；只描述「需要实现什么」，二进制布局与算法实现由配套设计
文档定义。

## 2. 产品概述

RowPack 是用 Go 实现的轻量级嵌入式二维表存储引擎，面向备份/快照/差异归档/本地分析等顺序写、
随机读场景。引擎只用**单个持久化文件** `<name>.rpk`（append-only，含快照、压缩块、行变更与
内嵌的每快照 IndexTxn），支持 FULL/DELTA 快照、Zstandard 压缩、按快照/表/行随机访问、多读单写、
不依赖独立 WAL 的崩溃恢复，以及创建时确定的 AES-256-GCM 块级加密（可选）。

## 3. 设计目标

### 3.1 目标

把二维表及其版本历史持久化为易于复制归档的单文件；把 UPDATE/DELETE 变为顺序追加（不做文件内随机
覆盖）；在压缩前提下提供行级随机读；允许多 goroutine 并发读已提交数据；未完成快照不可被观察为
已提交；不依赖独立 WAL 完成尾部恢复；为压缩算法、索引和数据整理保留格式演进空间。

### 3.2 非目标

v1 不包含：SQL 解析/查询优化/关系运算；B+Tree、LSM Tree 或通用二级索引；多写者并行提交；跨快照
或跨表的 ACID 事务；通用 MVCC 与未提交数据读取；分布式复制、网络协议和服务端模式；在线原地更新；
自动后台 Compaction；列式存储、谓词下推和向量化执行；访问控制与密钥托管（仅提供 `KeyProvider`
抽象接口，密钥由调用方管理）。

## 4. 核心概念

| 概念 | 定义 |
| --- | --- |
| Store | 单个 `<base>.rpk` 文件及其运行时状态 |
| Table | 由 `TableID` 唯一标识、按 `(NS, Name)` 地址寻址的二维表 |
| Schema | 表的有序列定义及其版本 |
| Row | 按 Schema 列顺序编码的一组值，不重复保存列名 |
| RowID | 表内稳定的逻辑行标识，与业务主键相互独立 |
| Snapshot | 一组不可变、原子提交的行变更 |
| FULL Snapshot | 能独立表达某一时点完整表状态的快照 |
| DELTA Snapshot | 相对于一个已提交父快照的变更集合 |
| Change | INSERT、UPDATE 或 DELETE 记录 |
| Block | 多条变更编码后形成的逻辑压缩与校验单位 |
| Rows Page | Block 内的物理压缩/校验单位（默认 32 KiB） |
| Tombstone | 表示行已删除且无行负载的 DELETE 记录 |
| Commit | 使快照及相关索引对新读取操作可见的原子状态转换 |
| IndexTxn | 每个已提交快照内嵌的派生导航结构 |

快照形成父子链 `S1 FULL <- S2 DELTA <- S3 DELTA <- ...`。已提交快照永久不可修改；同一
`(TableID, RowID)` 的后续版本追加新记录表达，不覆盖旧记录。允许任意时刻提交新 FULL
（checkpoint），SnapshotID 全局递增、深度重置。

## 5. 用户与典型场景

### 5.1 目标用户

嵌入本地表格存储的 Go 开发者；保存数据库全量备份与增量差异的工具开发者；对历史快照点查/扫描/
恢复的数据工程应用。

### 5.2 典型场景

全量导出写 FULL；比对得到的 INSERT/UPDATE/DELETE 写 DELTA；按快照 ID + 表名 + RowID 读历史行；
多 goroutine 并发随机读；顺序扫描某快照的表用于恢复/导出；写入中途异常退出后重启清理未提交尾部
并恢复可用；以单个 `.rpk` 完成备份、迁移与复制。

## 6. 总体约束

### 6.1 文件约束

1. 一个 Store 必须且只能使用一个 `.rpk` 文件，不得产生第二个数据/索引文件。
2. 文件必须 append-only；除恢复时截断无效尾部外，不得原地覆盖已提交数据。
3. 索引（IndexTxn）必须与数据同文件，位于对应快照的 Blocks 之后、SnapshotFooter 之前。
4. 文件必须包含格式 Magic、格式版本、特性标志和必要校验信息。
5. 所有多字节整数必须使用固定字节序；v1 规定为 Little Endian。
6. 未识别的主版本必须拒绝打开；可兼容次版本或可忽略特性按格式规则处理。

### 6.2 标识约束

- `SnapshotID`：Store 内唯一，`uint64`，全局递增；
- `TableID`：Store 内唯一，`uint32`；
- `RowID`：表内唯一，`uint64`；
- `BlockID`：Store 内唯一，`uint64`；
- `SchemaVersion`：表内单调递增。

`RowID` 不等同于业务主键。v1 由调用者提供 RowID；同一表内不得冲突。

## 7. 功能需求

### FR-001 创建与打开 Store

支持创建新 Store（只生成单个 `<base>.rpk`），并以只读或读写模式打开已有 Store；打开时必须校验
Magic、版本和基础结构完整性；文件缺失或 Magic/版本不兼容时返回可识别错误，不得静默创建或覆盖；
同一 Store 在一个进程内最多一个写实例，只读实例和并发读不得共享可变文件游标；加密 Store 的
Open 必须能提供密钥，否则返回 `ErrKeyRequired`。

### FR-002 表与 Schema

一个 Store 支持多个表；每个表具有唯一 `TableID`、名称及至少一个 Schema 版本。Schema 保存有序
列定义，至少含列名、逻辑类型、可空性；每条含负载的行记录能关联到明确的 Schema 版本；行负载按
列顺序编码，不在每行重复列名。v1 定义并稳定支持以下基础值类型：NULL、布尔、带符号整数、无符号
整数、浮点数、字符串、字节数组、时间、日期时间、十进制定点数（共 17 种类型 ID）；不支持的类型
必须在写入前报错。

RowPack 在快照内持久化自身的 Canonical Schema，至少含表名、Schema 版本、列顺序、逻辑类型、
可空性、Decimal 精度；它是行编码/解码契约，不等同于源数据库 Schema。源数据库的原始设计元信息
（列定义、约束、索引、视图、触发器、注释、厂商扩展等）如被采集，作为独立的 Source Metadata
保存（用于恢复、审计、对比），不得驱动引擎按源库方言建模，也不由引擎解释语义。Source Metadata
的载体已定为**普通行数据**：上层用 `DefineTableIn` 在自己的 ns 建目录表（对象一张、字段列表
一张），以普通 `Insert/Update/Delete` 写入；不得做成元数据 TLV 记录类型或新增字段，因为方言
属性不可穷举而 TLV 字段编号一经发布即冻结（见
[METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md) §9）。

元数据支持 ns、稳定对象 ID、Revision、Tombstone 和可扩展 TLV 字段；未知非关键字段必须可跳过
并无损透传，未知关键字段必须拒绝。元数据与行数据必须在同一 Snapshot 原子提交，解释行所需的
元数据必须可由当前快照或父链获得。引擎不内建 CoreMetadata 或数据库对象强类型模型；行解码所需
的最小 Canonical Schema 契约由 `Tx.DefineTable` 写入（引擎自产自销的规范类型字符串）。元数据
TLV 仅是内部持久化格式，不承载源库元信息，也不提供通用读写通道。

### FR-003 快照生命周期

支持 FULL 与 DELTA 两种快照：FULL 的 `ParentID` 必须为 0，DELTA 必须引用一个已提交父快照。一个
写入会话同时最多存在一个未提交快照；快照支持 Begin / 写入变更 / Commit / Abort 生命周期。
Commit 成功后快照不可修改；Abort 或 Commit 失败的快照对读取者不可见；空 DELTA 允许提交（表达
无变化检查点）。快照提交后必须能列出其 ID、类型、父 ID、创建时间、Block 数、变更数和数据范围。
任意时刻允许提交新的 FULL checkpoint，其深度重置为 1，后续 DELTA 以它为父。

### FR-003A Schema 与设计元信息对比

Canonical Schema 与源数据库 Schema 必须作为两个不同层次处理，源库 Schema 不得直接驱动引擎核心
按方言建模。Canonical Schema 的变化必须通过新的 SchemaVersion 表达；已提交 Schema 不得原地
修改，历史快照继续用原版本解码。新增/删除/修改列以及类型、可空性、Decimal 精度变化属于
Canonical Schema Diff；约束、索引、视图、注释和厂商扩展等属于 Source Metadata Diff。两类 Diff
的结果可由上层适配器用于恢复或生成迁移 SQL，但不属于引擎核心的数据库语义。

源库元信息的载体是调用方自选 ns 下的普通目录表（FR-002），不使用元数据 TLV；因此不需要
`PutMetadata`、`Metadata`、`ListMetadata` 这类 API，也不需要实现任何保留 WireType。源库元信息
的差异计算是普通的行差异，可直接复用块级比对（`Blocks`/`ScanBlocks`）。

### FR-004 行变更写入

支持 INSERT / UPDATE / DELETE：INSERT 与 UPDATE 在 v1 保存完整行（不是仅变化列），DELETE 写入
Tombstone、不保存行负载。每个变更含表名、`RowID`、变更类型和必要的 Schema 版本；写入顺序与调用
顺序一致。对同一快照中的重复 `(Table, RowID)`，v1 必须拒绝并返回冲突错误，避免提交语义歧义。
写入必须先形成数据页并持久化数据，再持久化 IndexTxn，最后发布内存可见状态；未 Commit 的数据
不得被普通读取 API 返回。

### FR-005 块构建与压缩

v1 必须支持 `None` 和 `Zstd` 两种压缩标识，默认 Zstd。压缩以 Rows Page 为单位，禁止整文件压缩
和逐行独立压缩；Metadata Block 为整容器压缩。默认目标块大小 256 KiB 未压缩数据、默认页大小
32 KiB，均可在创建 Store 时配置；页大小建库后不可变。达到页目标大小应结束当前页；页大小是目标
值而非严格上限；超过页目标大小的单行必须允许形成独立 Large Row Page。一个 Block 只能属于一个
快照和一个表。Rows Page 必须用列流（RowID / end-offset / SchemaVersion / ChangeType / body）
编码，使解压后可直接定位单条记录。每个 Block 必须记录压缩算法、记录数、原始长度、存储长度、
快照 ID、表 ID 和校验值；每个页由自身 PageCRC 校验。CRC32C 必须对压缩前的规范化内容计算，使
内容校验不受压缩级别影响；解压后长度或 CRC 不匹配必须返回数据损坏错误。

### FR-006 索引

IndexTxn 必须至少提供三类逻辑索引：

1. Snapshot Index：`SnapshotID -> 类型、ParentID、数据范围、Block 范围、记录数、提交状态`。
2. Block Index：`BlockID -> 文件 offset、存储长度、原始长度、SnapshotID、TableID、记录数、
   校验信息`。
3. Row Index：`SnapshotID + TableID + RowID -> BlockID、ItemOrdinal、状态`，以排序 Row Index
   Page + Fence Directory 持久化。

索引必须满足：不扫描整个文件即可定位指定 Block；能解析给定快照下某行的最终可见版本；能表达
Tombstone；能在启动时被校验、加载或按需访问；能识别其覆盖到的文件最后有效位置（用于尾部恢复）；
单条 IndexTxn 损坏时可由该快照自身的 Block 在内存重建。

### FR-007 随机读取

必须提供 `Get(snapshotID, tableName, rowID)` 语义的读取能力，返回指定快照视图中的最终可见行。
若目标行在当前 DELTA 中没有变更，必须沿 Parent 链向前解析，直至找到行记录、Tombstone 或到达
FULL 快照；遇到 Tombstone、链首仍无该行或表不存在时必须返回可区分的未找到错误。读取必须通过
`ReadAt` 或等价的不共享游标机制访问文件，不得长时间持有全局锁，磁盘读取和解压不得阻塞其他读取
者；已开始的读取操作必须观察到一致的已提交快照视图。

### FR-008 扫描读取

支持扫描某个快照中的指定表，结果是应用完整父链后的逻辑状态（不得返回被后续版本覆盖的旧行或
Tombstone）。扫描必须支持迭代器方式，避免强制把整表装入内存；顺序必须稳定，v1 默认按 `RowID`
升序。调用者中止、上下文取消或迭代器出错时，扫描必须及时停止并传播原因。

### FR-009 并发控制

v1 采用「多读、单写」模型：多个 goroutine 必须能安全并发调用读取 API；所有变更写入和 Commit
必须由单写锁串行化；写入未提交快照期间读取者继续访问提交前的稳定视图；Commit 发布新快照时，
读取者只能观察到提交前或提交后的完整状态，不得观察部分索引更新。`Close` 与正在进行的操作必须有
明确定义：默认等待已开始操作结束，并拒绝新操作。

### FR-010 缓存

必须提供进程内已解码页/块的缓存；缓存键至少含 Store 身份和 `BlockID`（或页身份）；默认替换策略
为线程安全 LRU 或语义等价策略。缓存容量必须可按字节配置，也必须允许禁用；扫描窗口与随机读缓存
共享总预算。同一 Block/页的并发首次读取应合并为一次加载，或保证不会产生不受控的重复解压。从
缓存返回的字节不得被调用者修改；Store 关闭时必须释放相关缓存引用。

### FR-011 持久化级别

- `SyncCommit`：Commit 返回成功前，单文件已完成 `fsync`/`fdatasync` 等价操作；
- `AsyncCommit`：允许操作系统延迟落盘，但必须保持进程内提交可见性的原子性。

默认必须为 `SyncCommit`。无论何种模式，写入顺序必须为：快照头、数据页、IndexTxn、SnapshotFooter、
fsync、内存状态发布；详细设计可合并步骤，但必须保持等价的恢复语义。

### FR-012 崩溃恢复

打开 Store 时必须检测文件尾部的不完整结构；只有具备完整 SnapshotFooter 且校验通过的快照才有效。
IndexTxn 完整但 SnapshotFooter 缺失时整个事务仍未提交。对于 SnapshotFooter 有效但 IndexTxn 损坏
的事务，必须扫描该事务的 Block 在内存重建索引，并继续处理后续快照。读写模式打开时允许截断无效
尾部；只读模式不得修改文件，只能忽略尾部并报告恢复状态。中间损坏与尾部不完整必须区分：已提交
范围内的损坏默认返回错误，不得自动跳过。恢复过程必须幂等，多次打开不得继续改变已恢复的逻辑
结果。

### FR-013 完整性校验

必须提供快速打开校验（至少验证头部、IndexTxn 边界和最近提交点）与显式全量校验 API（逐 Block/页
解压并验证长度和 CRC）。校验错误必须报告文件类型、逻辑对象 ID 和可用的 offset 信息；错误类型
至少区分：格式不兼容、数据损坏、索引损坏、快照链损坏、认证失败、截断写入。

### FR-014 资源管理

所有公开的长耗时 API 必须接受 `context.Context` 或提供可取消的等价形式。`Close` 必须刷新待提交
内容、关闭文件句柄并释放缓存；存在未提交快照时默认 Abort 并返回可识别状态。API 返回的行数据
所有权必须明确：读入口统一为借用/复用模式，访问器返回独立副本。系统不得因格式错误或损坏文件
发生 panic。

### FR-015 可观测性

必须暴露只读统计信息，至少包括：已提交快照数、表数、Block 数和逻辑行数；文件大小；原始字节数、
存储字节数和压缩比；随机读缓存与扫描窗口的命中、未命中、淘汰和当前占用字节数；累计物理读放大
（读取字节 / 解压字节 / 页加载）；批量读取聚合统计；最近一次恢复是否发生及被忽略或截断的尾部
字节数。

### FR-016 加密（可选能力）

加密必须在 Create 时通过 `EncryptionConfig` 确定，之后不可更改；只使用 AES-256-GCM，先压缩后
加密。Rows Block 逐页密封，Metadata Block 整容器密封，IndexTxn body 按 chunk/页密封；
Header/Footer 保持明文可扫描。nonce 必须在 Block / Index / Page / Chunk 各域内两两不同，同密钥
下永不复用；AAD 必须绑定解密时已可得的身份字段，防止密文跨 Block/表/store 挪用。加密 Store 的
Open/Verify/索引重建必须提供密钥，否则返回 `ErrKeyRequired`；认证失败必须返回可定位的
`CorruptionError`。

## 8. Go API 轮廓

完整签名、字段与语义见 [GO_API_DESIGN_V1.md](GO_API_DESIGN_V1.md) §2–§5：创建/打开
`Create`/`Open`；写路径 `Begin` → `DefineTable`/`DefineTableIn`/`Insert`/`Update`/`Delete`/
`Apply`/`ApplyBatch` → `Commit`/`Rollback`；读路径 `Get`/`Exists`/`ReadBatch`/`Scan`/
`ScanBlocks`/`Blocks`；元数据与快照 `Schema`/`Tables`/`TablesIn`/`ListSnapshots`；以及
`Stats`/`Verify`/`Close`。批量差异写入必须能直接接收流式 Diff 结果，避免调用方预先把整个差异集
加载到内存。

## 9. 文件逻辑布局

单文件 `<base>.rpk` 为 `[FileHeader]` + 顺序的 SnapshotTxn（每个 txn：SnapshotHeader →
Metadata/Rows Blocks → 内嵌 IndexTxn → SnapshotFooter）+ 可选未提交尾部；SnapshotFooter 是快照
提交的权威标志。完整布局、页容器与字段见 [BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md) §3/§5/§7。

## 10. IndexTxn 逻辑布局

每个 SnapshotTxn 内嵌 `[IndexTxnHeader][Snapshot/Metadata/Block Chunks + ChunkDirectory]`
`[Row Index Pages + Row Index Fence][IndexTxnFooter]`。实现可用内存索引、mmap 或文件内二分查找，
但不得改变第 7 节规定的读取语义；IndexTxn 是历史快照的派生导航结构，启动时可全部载入内存，必须
提供索引内存占用统计。字段布局见 [INDEX_TXN_FORMAT_V1.md](INDEX_TXN_FORMAT_V1.md)。

## 11. 快照解析规则

读取 `(S3, T1, R10)`：当前快照有该行记录时优先于父快照（INSERT 与 UPDATE 都表示当前完整行，仅
写入校验语义不同）；找到 DELETE 即终止父链查找并返回未找到；当前快照未找到则查 Parent，直至
FULL 或链首；到达 FULL 后不得继续查其前驱；父快照不存在、形成环或类型规则不合法时，Store 被视
为快照链损坏。

## 12. 错误模型

公开 API 必须支持 `errors.Is` 或等价机制识别错误，不得依赖字符串匹配判断类别，错误信息必须包含
操作上下文。完整性失败使用结构化 `CorruptionError`（文件、offset、Snapshot/Table/Block、Kind、
Cause）。哨兵错误清单见 [GO_API_DESIGN_V1.md](GO_API_DESIGN_V1.md) §8。

## 13. 非功能需求

### NFR-001 正确性

- 所有公开 API 必须通过 Go race detector；
- 已提交快照在正常关闭和异常重启后应得到相同逻辑结果；
- 任何损坏输入不得导致越界读取、无限内存分配或 panic。

### NFR-002 性能

在基准环境、数据集和磁盘条件被记录的前提下，v1 应达到：缓存命中的单行读取不发生磁盘 I/O 和
重复解压；缓存未命中的单行读取最多读取其索引路径及一个目标页/块，不扫描完整文件；顺序写入吞吐
不得因每行单独 `fsync`（同步发生在 Commit 边界）；扫描额外工作内存受页/块大小、索引策略和缓存
配置约束，不与表总数据量线性增长。具体数值由基准套件固化（见
[PERFORMANCE_BASELINE_V1.md](PERFORMANCE_BASELINE_V1.md)）。

### NFR-003 可扩展性

- 文件格式必须携带版本号和特性标志；
- 压缩算法必须通过稳定枚举或接口抽象，后续可增加 LZ4 等算法；
- Row 编码器与压缩器必须解耦；
- 未压缩长度、压缩长度和文件 offset 的字段宽度必须支持大文件；文件 offset 使用 `uint64`。

### NFR-004 可移植性

- 支持 Go 当前稳定版本及其前一个主要版本，具体以 `../go.mod` 为准；
- 优先支持 Linux、macOS 和 Windows 的 amd64/arm64；
- 文件格式不得依赖机器字长、结构体内存布局或本机字节序。

### NFR-005 可测试性

- 编码、压缩、索引、恢复和缓存模块必须可独立测试；
- 必须提供确定性故障注入点，用于模拟每个持久化阶段前后的崩溃；
- 测试必须可生成损坏的 Header、Block、IndexTxn、Footer 和 Index Entry。

### NFR-006 安全边界

- 打开不可信文件时，所有长度、数量和 offset 必须先校验再分配或读取；
- 必须提供最大行大小、最大块大小、最大列数和最大父链深度限制，并允许在合理范围内配置；
- 解压必须限制目标大小，防止压缩炸弹导致无界内存分配。

## 14. 验收标准

- **AC-001 基本存取**：创建 Store，写入含至少 3 张表、17 种基础值类型和 10,000 行的 FULL 快照，
  关闭并重开后所有行值、类型和 NULL 状态完全一致；文件系统中只有一个 `.rpk` 文件。
- **AC-002 差异快照**：在 FULL 上连续提交至少 3 个 DELTA（含 INSERT、UPDATE、DELETE），读取任意
  历史快照均得到该时点正确结果，且旧快照结果不受新提交影响。
- **AC-003 随机访问**：对不少于 100,000 行的数据随机读取 10,000 个 RowID，过程不得扫描完整
  文件，结果与写入源数据一致。
- **AC-004 并发读取**：至少 32 个 goroutine 并发 Get 和 Scan，同时另一个 goroutine 构建并提交
  快照；无数据竞争、无死锁，读取者只观察到提交前或提交后的完整状态。
- **AC-005 压缩与缓存**：默认创建的页使用 Zstd；连续读取同一页内多行时统计表明首次读取后出现
  缓存命中；禁用缓存后结果一致。
- **AC-006 超大单行**：写入大于配置目标页大小的合法行，以独立 Large Row Page 保存，重启后可
  正确读取并通过 CRC 校验。
- **AC-007 未提交快照**：Begin 后写入但不 Commit，模拟进程终止；重启后该快照不可见，最近一个
  已提交快照仍可完整读取。
- **AC-008 故障恢复**：分别在快照头写入后、数据块写入中、数据同步后、IndexTxn 写入中、Footer
  写入中和同步完成后注入故障；每种情况重启均恢复到最近一致提交点，或重建可证明完整的索引，
  不返回部分快照。
- **AC-009 损坏检测**：分别破坏 Rows Page 压缩数据、原始 CRC、IndexTxn、Footer 和父快照引用；
  快速或全量校验必须返回对应的结构化损坏错误，不得 panic。
- **AC-010 资源限制**：用伪造的超大长度、超大解压尺寸和过深父链文件测试，系统必须在配置限制内
  拒绝处理，不发生失控内存分配。
- **AC-011 文件兼容性**：同一 v1 文件在所有受支持平台读取结果一致；未知主版本文件打开时返回
  `ErrVersionUnsupported`，且文件内容不被修改。
- **AC-012 生命周期**：Close 后调用任何读写 API 均返回 `ErrClosed`；并发 Close 不发生 panic；
  未提交 writer 按规定 Abort。
- **AC-013 加密**：加密 Store 的写入/重开/Get/Scan/批量读与明文 Store 语义等价；无密钥 Open
  返回 `ErrKeyRequired`；篡改任一密文字节触发 `ErrAuthFailed`；nonce 域不重叠。

## 15. 测试要求

v1 发布前必须覆盖：单元测试（编解码、类型边界、CRC、压缩、索引查找、父链解析、LRU）；结构测试
（固定结构尺寸、魔数、枚举与 Marshal/Unmarshal round-trip）；Fuzz（文件与记录解析、任意/截断
输入、压缩数据）；集成测试（创建、写入、提交、重开、历史读取、扫描、只读模式）；并发测试并过
`go test -race`；恢复测试（覆盖全部故障注入点）；兼容性测试（golden files）；加密测试
（round-trip、无 key、错 key、篡改、加密 golden 重开）；Benchmark（顺序写、随机冷/热读、顺序
扫描、批量读、压缩率、索引内存占用）。

## 16. 后续版本候选能力

不影响 v1 验收：离线或在线 Compaction / Rewrite；快照删除、保留策略与空间回收；业务主键到 RowID
的独立索引；LZ4 等额外压缩算法；列裁剪、块级统计信息和谓词过滤；更细粒度的索引分页加载。

原列在此的「通用 Source Metadata 公开读写 API」是已决议的**非目标**：源库元信息以自选 ns 下的
普通目录表承载（FR-002），无需新增 API，也无需实现任何保留的元数据 WireType。
