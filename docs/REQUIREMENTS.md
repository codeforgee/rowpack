# RowPack v1.0 需求规格说明书

> 文档状态：草案  
> 产品名称：RowPack  
> 实现语言：Go  
> 目标版本：v1.0  
> 更新时间：2026-09-06

## 1. 文档目的

本文档定义 RowPack v1.0 的产品范围、核心概念、功能需求、非功能需求、外部接口、数据一致性规则及验收标准，作为架构设计、开发、测试和版本验收的共同依据。

本文档描述“需要实现什么”，具体二进制字段布局、模块划分和算法实现由后续详细设计文档定义。

## 2. 产品概述

RowPack 是一个使用 Go 实现的轻量级嵌入式二维表存储引擎。它面向备份、快照、差异归档和本地分析等以顺序写入、随机读取为主的场景。

引擎采用两个持久化文件：

- `<name>.rpk`：数据文件，使用 append-only 方式保存快照、压缩块和行变更记录。
- `<name>.rpi`：索引文件，保存快照索引、数据块索引和行索引。

RowPack v1.0 的核心能力为：

- 在一个数据文件中存储一个或多个二维表。
- 支持完整快照和差异快照。
- 使用 Zstandard 对数据块进行压缩。
- 支持按快照、表和行进行随机访问。
- 支持多个读取者并发读取，并采用单写者模型保证写入顺序。
- 通过校验、提交标记和启动恢复处理进程异常退出及尾部不完整写入。

## 3. 设计目标

### 3.1 目标

1. 将二维表数据及其版本历史持久化为易于复制和归档的双文件格式。
2. 将 UPDATE 和 DELETE 转换为顺序追加写，避免数据文件内的随机覆盖。
3. 在启用压缩的前提下，提供行级随机读取能力。
4. 允许多个 goroutine 安全并发读取已提交数据。
5. 保证未完成的快照不会被读取者观察为已提交状态。
6. 在不依赖独立 WAL 的情况下完成尾部写入恢复。
7. 为未来的压缩算法扩展、索引优化和数据整理保留格式演进空间。

### 3.2 非目标

RowPack v1.0 不包含：

- SQL 解析、查询优化和关系运算。
- B+Tree、LSM Tree 或通用二级索引。
- 多写者并行提交。
- 跨快照或跨表的 ACID 事务。
- 通用 MVCC 和未提交数据读取。
- 分布式复制、网络协议和服务端模式。
- 在线原地更新数据记录。
- 自动后台 Compaction。
- 列式存储、谓词下推和向量化执行。
- 数据加密、访问控制和密钥管理。

## 4. 核心概念

| 概念 | 定义 |
| --- | --- |
| Store | 一组配对的 `.rpk` 与 `.rpi` 文件及其运行时状态 |
| Table | 由 `TableID` 唯一标识的二维表 |
| Schema | 表的有序列定义及其版本 |
| Row | 按 Schema 列顺序编码的一组值，不重复保存列名 |
| RowID | 表内稳定的逻辑行标识，与业务主键相互独立 |
| Snapshot | 一组不可变、原子提交的行变更 |
| FULL Snapshot | 能独立表达某一时点完整表状态的快照 |
| DELTA Snapshot | 相对于一个已提交父快照的变更集合 |
| Change | INSERT、UPDATE 或 DELETE 记录 |
| Block | 多条变更编码后形成的压缩和校验单位 |
| Tombstone | 表示行已删除且无行负载的 DELETE 记录 |
| Commit | 使快照及相关索引对新读取操作可见的原子状态转换 |

快照形成一条父子链：

```text
S1 FULL <- S2 DELTA <- S3 DELTA <- ...
```

一个已提交快照永久不可修改。相同 `(TableID, RowID)` 的后续版本通过追加新记录表达，不覆盖旧记录。

## 5. 用户与典型场景

### 5.1 目标用户

- 需要在 Go 应用中嵌入本地表格存储能力的开发者。
- 需要保存数据库完整备份和增量差异的工具开发者。
- 需要对历史快照进行点查、扫描或恢复的数据工程应用。

### 5.2 典型场景

1. 将某个数据库表的全量导出写为 FULL 快照。
2. 将后续比对得到的 INSERT、UPDATE、DELETE 写为 DELTA 快照。
3. 根据快照 ID、表 ID 和行 ID 读取某个历史时点的行。
4. 多个 goroutine 同时随机读取已提交数据。
5. 顺序扫描某个快照下的表数据，用于恢复或导出。
6. 进程在写入中途异常退出，重启后忽略或清理未提交尾部并恢复可用状态。

## 6. 总体约束

### 6.1 文件约束

1. 一个 Store 必须且只能使用一个 `.rpk` 数据文件和一个 `.rpi` 索引文件。
2. 数据文件必须采用 append-only 写入；除恢复时截断无效尾部外，不得原地覆盖已提交数据。
3. 索引文件必须保存快照、块和行定位所需的信息；具体可采用分区或追加日志结构。
4. 两个文件均必须包含格式 Magic、格式版本、特性标志和必要校验信息。
5. 所有多字节整数必须使用固定字节序；v1 规定为 Little Endian。
6. 未识别的主版本必须拒绝打开；可兼容的次版本或可忽略特性应按格式规则处理。

### 6.2 标识约束

- `SnapshotID`：Store 内唯一，使用 `uint64`。
- `TableID`：Store 内唯一，使用 `uint32`。
- `RowID`：表内唯一，使用 `uint64`。
- `BlockID`：Store 内唯一，使用 `uint64`。
- `SchemaVersion`：表内单调递增。

`RowID` 不等同于业务主键。v1 可由调用者提供 RowID，也可由引擎分配，但同一表内不得发生冲突。

## 7. 功能需求

### FR-001 创建与打开 Store

1. 系统必须支持创建新的 Store，并生成配对的数据文件与索引文件。
2. 系统必须支持以只读或读写模式打开已有 Store。
3. 打开时必须校验文件 Magic、版本、文件配对标识和基础结构完整性。
4. 任一文件缺失、文件不配对或版本不兼容时必须返回可识别错误，不得静默创建或覆盖。
5. 同一 Store 在一个进程内最多允许一个写实例；只读实例和并发读操作不得共享可变文件游标。

### FR-002 表与 Schema

1. 一个 Store 必须支持多个表。
2. 每个表必须具有唯一 `TableID`、名称及至少一个 Schema 版本。
3. Schema 必须保存有序列定义，至少包含列名、逻辑类型和可空性。
4. 每条含负载的行记录必须能够关联到明确的 Schema 版本。
5. 行负载应按列顺序编码，不得在每行重复存储列名。
6. v1 必须定义并稳定支持以下基础值类型：NULL、布尔、带符号整数、无符号整数、浮点数、字符串、字节数组、时间、日期时间和十进制定点数。
7. 不支持的值类型必须在写入前返回错误。
8. RowPack 必须在快照内持久化自身的 Canonical Schema，至少包括表名、Schema 版本、列顺序、逻辑类型、可空性和 Decimal 精度。Canonical Schema 是行编码/解码契约，不等同于源数据库 Schema。
9. 源数据库的原始设计元信息（列定义、约束、索引、视图、触发器、注释和厂商扩展等）如被采集，应作为独立的 Source Metadata 保存，用于恢复、审计和对比；不应驱动 RowPack 核心按源数据库方言建模，也不由引擎解释其语义。
10. 元数据必须支持命名空间、稳定对象 ID、Revision、Tombstone 和可扩展 TLV 字段；未知非关键字段必须可跳过并无损透传，未知关键字段必须拒绝处理。
11. 元数据与行数据必须在同一 Snapshot 中原子提交，解释行所需的元数据必须可由当前快照或父链获得。
12. 引擎不内建 CoreMetadata 或数据库对象的强类型模型。行解码所需的最小 Canonical Schema 契约由 `DefineSchema` 提供，使用引擎自产自销的规范类型字符串。元数据 TLV 仅是内部持久化格式；当前公开 API 不提供通用 Source Metadata 的读写通道。

### FR-003 快照生命周期

1. 系统必须支持创建 FULL 和 DELTA 两种快照。
2. FULL 快照的 `ParentID` 必须为 0；DELTA 快照必须引用一个已提交父快照。
3. 一个写入会话同一时间最多存在一个未提交快照。
4. 快照必须支持 Begin、写入变更、Commit 和 Abort 生命周期。
5. Commit 成功后快照必须不可修改。
6. Abort 或 Commit 失败的快照不得对读取者可见。
7. 空 DELTA 快照是否允许由选项控制；默认允许，用于表达无变化的检查点。
8. 快照提交后必须能够列出其 ID、类型、父 ID、创建时间、记录数和数据范围。

### FR-003A Schema 与设计元信息对比

1. RowPack 的 Canonical Schema 与源数据库 Schema 必须作为两个不同层次处理；源数据库 Schema 不得直接驱动引擎核心按数据库方言建模。
2. Canonical Schema 的变化必须通过新的 SchemaVersion 表达；已提交 Schema 不得原地修改，历史快照必须继续使用原版本解码。
3. 新增、删除或修改列，以及类型、可空性或 Decimal 精度变化，属于 Canonical Schema Diff；约束、索引、视图、注释和厂商扩展等属于 Source Metadata Diff。
4. 两类 Diff 的结果可以由上层适配器用于恢复或生成迁移 SQL，但不属于 RowPack 引擎核心的数据库语义。
5. 当前版本不提供通用 Source Metadata 公开读写 API；元数据 TLV 的持久化扩展能力不等同于已有的 `PutMetadata`、`Metadata` 或 `ListMetadata` API。

### FR-004 行变更写入

1. 系统必须支持 INSERT、UPDATE 和 DELETE 三种变更类型。
2. INSERT 和 UPDATE 在 v1 中必须保存完整行，而不是仅保存变化列。
3. DELETE 必须写入 Tombstone，不保存行负载。
4. 每个变更必须包含 `TableID`、`RowID`、变更类型和必要的 Schema 版本。
5. 写入顺序必须与调用顺序一致。
6. 对同一快照中的重复 `(TableID, RowID)`，v1 必须拒绝写入并返回冲突错误，避免提交语义歧义。
7. 写入必须先形成数据块并持久化数据，再持久化相应索引，最后发布内存可见状态。
8. 未调用 Commit 的数据不得被普通读取 API 返回。

### FR-005 块构建与压缩

1. v1 必须支持 `None` 和 `Zstd` 两种压缩标识，默认使用 Zstd。
2. 压缩必须以 Block 为单位，禁止整文件压缩和逐行独立压缩。
3. 默认目标块大小为 256 KiB 未压缩数据，并允许在创建 Store 时配置。
4. 达到目标大小后应结束当前块；块大小是目标值而非严格上限。
5. 超过目标块大小的单行必须允许形成独立块。
6. 一个 Block 只能属于一个快照和一个表，避免读取时混合无关数据。
7. Block 的未压缩内容必须包含行偏移表，使解压后可直接定位单条记录。
8. 每个 Block 必须记录压缩算法、记录数、原始长度、压缩长度、快照 ID、表 ID 和校验值。
9. CRC32 必须对压缩前的规范化块内容计算，以保持内容校验不受压缩级别影响。
10. 解压后的长度或 CRC 不匹配时必须返回数据损坏错误。

### FR-006 索引

索引文件必须至少提供以下三类逻辑索引：

1. Snapshot Index：`SnapshotID -> 类型、ParentID、数据范围、Block 范围、记录数、提交状态`。
2. Block Index：`BlockID -> 数据文件 offset、压缩长度、原始长度、SnapshotID、TableID、记录数、校验信息`。
3. Row Index：`SnapshotID + TableID + RowID -> BlockID、块内 offset、长度、状态`，或提供语义等价的持久化结构。

索引必须满足：

- 能在不扫描整个数据文件的情况下定位指定 Block。
- 能解析给定快照下某行的最终可见版本。
- 能表达 Tombstone。
- 能在启动时被校验、加载或按需访问。
- 能识别其覆盖到的数据文件最后有效位置，用于尾部恢复。

### FR-007 随机读取

1. 系统必须提供 `Get(snapshotID, tableID, rowID)` 语义的读取能力。
2. 返回值必须是指定快照视图中的最终可见行。
3. 若目标行在当前 DELTA 中没有变更，系统必须沿 Parent 链向前解析，直至找到行记录、Tombstone 或到达 FULL 快照。
4. 遇到 Tombstone、链首仍无该行或表不存在时必须返回可区分的未找到错误。
5. 读取必须通过 `ReadAt` 或等价的不共享游标机制访问文件。
6. 读取不得长时间持有全局锁；磁盘读取和解压过程不得阻塞其他读取者。
7. 已开始的读取操作必须观察到一致的已提交快照视图。

### FR-008 扫描读取

1. 系统必须支持扫描某个快照中的指定表。
2. 扫描结果必须是应用完整父链后的逻辑状态，不得返回已被后续版本覆盖的旧行或 Tombstone。
3. 扫描必须支持回调或迭代器方式，避免强制把整表装入内存。
4. 扫描顺序必须稳定；v1 默认按 `RowID` 升序输出。
5. 调用者中止、上下文取消或回调返回错误时，扫描必须及时停止并传播原因。

### FR-009 并发控制

1. v1 采用“多读、单写”并发模型。
2. 多个 goroutine 必须能够安全地同时调用读取 API。
3. 所有变更写入和 Commit 必须由单写锁串行化。
4. 写入未提交快照期间，读取者必须继续访问提交前的稳定视图。
5. Commit 发布新快照时，读取者只能观察到提交前或提交后的完整状态，不得观察部分索引更新。
6. `Close` 与正在进行的操作必须有明确定义；默认等待已开始操作结束，并拒绝新操作。

### FR-010 Block Cache

1. v1 必须提供进程内已解压 Block 缓存。
2. 缓存键至少包含 Store 身份和 `BlockID`。
3. 默认替换策略为线程安全 LRU 或语义等价策略。
4. 缓存容量必须可按字节配置，也必须允许禁用。
5. 同一 Block 的并发首次读取应合并为一次加载或保证不会产生不受控的重复解压。
6. 从缓存返回的字节不得被调用者修改。
7. Store 关闭时必须释放相关缓存引用。

### FR-011 持久化级别

系统必须提供以下持久化模式：

- `SyncCommit`：Commit 返回成功前，数据文件和索引文件均已完成 `fsync`/`fdatasync` 等价操作。
- `AsyncCommit`：允许操作系统延迟落盘，但必须保持进程内提交可见性的原子性。

默认模式必须为 `SyncCommit`。无论选择何种模式，写入顺序必须为：数据块、数据提交标记、索引内容、索引提交状态、内存状态发布；详细设计可合并步骤，但必须保持等价的恢复语义。

### FR-012 崩溃恢复

1. 打开 Store 时必须检测数据文件和索引文件尾部的不完整结构。
2. 只有具备完整提交标记且校验通过的快照才被视为有效。
3. 对于数据已完整写入但索引缺失的已提交块，系统应能够扫描最后已知索引位置之后的数据并重建索引。
4. 对于索引指向不完整、越界或校验失败数据的情况，系统必须忽略无效索引尾部并恢复至最近一致提交点。
5. 读写模式打开时允许将无效尾部截断；只读模式不得修改文件，只能忽略尾部并报告恢复状态。
6. 中间损坏与尾部不完整必须区分：中间损坏默认返回错误，不得自动跳过。
7. 恢复过程必须幂等，多次打开不得继续改变已恢复的逻辑结果。

### FR-013 完整性校验

1. 系统必须提供快速打开校验，至少验证头部、索引边界和最近提交点。
2. 系统必须提供显式全量校验 API，逐 Block 解压并验证长度和 CRC。
3. 校验错误必须报告文件类型、逻辑对象 ID 和可用的 offset 信息。
4. 错误类型至少应区分：格式不兼容、文件不配对、数据损坏、索引损坏、快照链损坏和截断写入。

### FR-014 资源管理

1. 所有公开的长耗时 API 必须接受 `context.Context` 或提供可取消的等价形式。
2. `Close` 必须刷新待提交内容、关闭文件句柄并释放缓存；存在未提交快照时默认 Abort 并返回可识别状态。
3. API 返回的行数据所有权必须明确；默认由调用者持有独立数据，不得引用会被缓存淘汰后复用的内部缓冲区。
4. 系统不得因格式错误或损坏文件发生 panic。

### FR-015 可观测性

系统必须暴露只读统计信息，至少包括：

- 已提交快照数、表数、Block 数和逻辑行数。
- 数据文件及索引文件大小。
- 原始字节数、压缩字节数和压缩比。
- Block Cache 命中、未命中、淘汰和当前占用字节数。
- 最近一次恢复是否发生及被忽略或截断的尾部字节数。

## 8. 建议的 Go API 轮廓

以下 API 用于限定产品语义，不作为最终代码签名：

```go
type Options struct {
	ReadOnly          bool
	BlockSize        int
	Compression      Compression
	CompressionLevel int
	CacheBytes       int64
	Durability       Durability
}

type SnapshotType uint8

const (
	FullSnapshot  SnapshotType = 1
	DeltaSnapshot SnapshotType = 2
)

type ChangeType uint8

const (
	Insert ChangeType = 1
	Update ChangeType = 2
	Delete ChangeType = 3
)

type RowKey struct {
	TableID uint32
	RowID   uint64
}

type Change struct {
	Type          ChangeType
	TableID       uint32
	RowID         uint64
	SchemaVersion uint32
	Row           []any
}

func Create(path string, opts Options) (*Store, error)
func Open(path string, opts Options) (*Store, error)

func (s *Store) BeginSnapshot(ctx context.Context, typ SnapshotType, parentID uint64) (*SnapshotWriter, error)
func (w *SnapshotWriter) Put(ctx context.Context, change Change) error
func (w *SnapshotWriter) Delete(ctx context.Context, tableID uint32, rowID uint64) error
func (w *SnapshotWriter) Commit(ctx context.Context) (uint64, error)
func (w *SnapshotWriter) Abort() error

func (s *Store) Get(ctx context.Context, snapshotID uint64, tableID uint32, rowID uint64) (Row, error)
func (s *Store) Scan(ctx context.Context, snapshotID uint64, tableID uint32, fn func(RowID, Row) error) error
func (s *Store) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
func (s *Store) Verify(ctx context.Context, mode VerifyMode) error
func (s *Store) Stats() Stats
func (s *Store) Close() error
```

批量差异写入必须能够直接接收流式 Diff 结果，避免调用者预先把整个差异集加载到内存。

## 9. 数据文件逻辑布局

`.rpk` 文件的逻辑布局如下：

```text
[File Header]

[Snapshot Header S1 / FULL]
  [Block Header]
  [Compressed Block Payload]
  ...
[Snapshot Footer S1]

[Snapshot Header S2 / DELTA / Parent=S1]
  [Block Header]
  [Compressed Block Payload]
  ...
[Snapshot Footer S2]
```

压缩前的 Block 逻辑布局如下：

```text
[Block Payload Header]
[Row Offset Entry 0]
[Row Offset Entry 1]
...
[Record 0: Change Header + Row Payload]
[Record 1: Change Header + Row Payload]
...
```

Snapshot Footer 是快照提交完成的权威标志之一，至少包含 SnapshotID、ParentID、起止位置、Block 数、记录数和内容校验信息。

## 10. 索引文件逻辑布局

`.rpi` 文件必须在逻辑上包含：

```text
[Index File Header]
[Snapshot Index Section or Log]
[Block Index Section or Log]
[Row Index Section or Log]
[Index Commit Marker / Checkpoint]
```

实现可以选择内存索引、mmap 或文件内二分查找，但不得改变第 7 节规定的读取语义。

v1 可在启动时将适量索引载入内存；实现必须提供索引内存占用统计。针对超大数据集的 mmap、分页索引或持久化树结构属于后续版本优化，不是 v1 验收前置条件。

## 11. 快照解析规则

读取 `(S3, T1, R10)` 时，逻辑解析过程为：

```text
在 S3 查询 R10
  ├─ 找到 INSERT/UPDATE：返回该行
  ├─ 找到 DELETE：返回 NotFound
  └─ 未找到：查询 Parent S2
                 └─ 继续，直至 FULL 或链首
```

规则如下：

1. 当前快照中的记录优先于父快照。
2. UPDATE 与 INSERT 对读取结果均表示当前完整行，但写入校验语义不同。
3. DELETE 终止父链查找并返回未找到。
4. 到达 FULL 快照后不得继续查找其前驱。
5. 父快照不存在、形成环或类型规则不合法时，Store 被视为快照链损坏。

## 12. 错误模型

公开 API 至少需要支持 `errors.Is` 或等价机制识别以下错误：

- `ErrNotFound`
- `ErrAlreadyExists`
- `ErrReadOnly`
- `ErrWriterBusy`
- `ErrSnapshotCommitted`
- `ErrSnapshotAborted`
- `ErrInvalidParent`
- `ErrSchemaMismatch`
- `ErrUnsupportedType`
- `ErrCorruptData`
- `ErrCorruptIndex`
- `ErrVersionUnsupported`
- `ErrStoreMismatch`
- `ErrClosed`

错误信息必须包含操作上下文，但不得依赖字符串匹配判断错误类别。

## 13. 非功能需求

### NFR-001 正确性

- 所有公开 API 必须通过 Go race detector。
- 已提交快照在正常关闭和异常重启后应得到相同逻辑结果。
- 任何损坏输入不得导致越界读取、无限内存分配或 panic。

### NFR-002 性能

在基准环境、数据集和磁盘条件被记录的前提下，v1 应达到：

- 缓存命中的单行读取不发生磁盘 I/O 和重复解压。
- 缓存未命中的单行读取最多读取其索引路径及一个目标 Block，不扫描完整数据文件。
- 顺序写入吞吐不得因每行单独 `fsync`；同步发生在 Commit 边界。
- 扫描过程中额外工作内存应受块大小、索引策略和缓存配置约束，不与表总数据量线性增长。

具体延迟和吞吐数值在首个 benchmark 建立后固化，不在需求阶段假设特定硬件结果。

### NFR-003 可扩展性

- 文件格式必须携带版本号和特性标志。
- 压缩算法必须通过稳定枚举或接口抽象，后续可增加 LZ4 等算法。
- Row 编码器与压缩器必须解耦。
- 未压缩长度、压缩长度和文件 offset 的字段宽度必须支持大文件；文件 offset 使用 `uint64`。

### NFR-004 可移植性

- 支持 Go 当前稳定版本及其前一个主要版本，具体以 `../go.mod` 为准。
- 优先支持 Linux、macOS 和 Windows 的 amd64/arm64。
- 文件格式不得依赖机器字长、结构体内存布局或本机字节序。

### NFR-005 可测试性

- 编码、压缩、索引、恢复和缓存模块必须可独立测试。
- 必须提供确定性故障注入点，用于模拟每个持久化阶段前后的崩溃。
- 测试必须可生成损坏的 Header、Block、Footer 和 Index Entry。

### NFR-006 安全边界

- 打开不可信文件时，所有长度、数量和 offset 必须先校验再分配或读取。
- 必须提供最大行大小、最大块大小、最大列数和最大父链深度限制，并允许在合理范围内配置。
- 解压必须限制目标大小，防止压缩炸弹导致无界内存分配。

## 14. 验收标准

### AC-001 基本存取

创建 Store，写入包含至少 3 张表、10 种基础值类型和 10,000 行的 FULL 快照，关闭并重新打开后，所有行值、类型和 NULL 状态完全一致。

### AC-002 差异快照

在 FULL 快照上连续提交至少 3 个 DELTA 快照，分别包含 INSERT、UPDATE 和 DELETE；读取任意历史快照均得到该时点的正确结果，且旧快照结果不受新提交影响。

### AC-003 随机访问

对不少于 100,000 行的数据随机读取 10,000 个 RowID，读取过程不得扫描完整数据文件；读取结果与写入源数据一致。

### AC-004 并发读取

至少 32 个 goroutine 并发执行 Get 和 Scan，同时另一个 goroutine 构建并提交快照；测试无数据竞争、无死锁，读取者只观察到提交前或提交后的完整状态。

### AC-005 压缩与缓存

默认创建的 Block 使用 Zstd。连续读取同一 Block 内多行时，统计信息表明首次读取后出现缓存命中；禁用缓存后结果保持一致。

### AC-006 超大单行

写入一条大于配置目标块大小的合法行，系统以独立 Block 保存，重启后可正确读取并通过 CRC 校验。

### AC-007 未提交快照

Begin 后写入数据但不 Commit，模拟进程终止。重启后该快照不可见，最近一个已提交快照仍可完整读取。

### AC-008 故障恢复

分别在数据块写入中、数据同步后、索引写入中和索引同步后注入故障。每种情况下重启均恢复到最近一致提交点，或重建可证明完整的数据索引，不返回部分快照。

### AC-009 损坏检测

分别破坏 Block 压缩数据、原始 CRC、索引 offset 和父快照引用。快速校验或全量校验必须返回对应的结构化损坏错误，不得 panic。

### AC-010 资源限制

使用伪造的超大长度、超大解压尺寸和过深父链文件进行测试，系统必须在配置限制内拒绝处理，不发生失控内存分配。

### AC-011 文件兼容性

同一 v1 文件在所有受支持平台读取结果一致。使用未知主版本文件打开时返回 `ErrVersionUnsupported`，且文件内容不被修改。

### AC-012 生命周期

Close 后调用任何读写 API 均返回 `ErrClosed`；并发 Close 不发生 panic；未提交 writer 按规定 Abort。

## 15. 测试要求

v1 发布前必须包含：

- 单元测试：编码/解码、类型边界、CRC、压缩、索引查找、父链解析、LRU。
- 属性测试或 Fuzz：文件解析、记录解析、任意字节输入、截断输入和压缩数据。
- 集成测试：创建、写入、提交、重开、历史读取、扫描和只读模式。
- 并发测试：多读单写、Commit 可见性、Close 竞态，并通过 `go test -race`。
- 恢复测试：覆盖所有规定的故障注入点。
- 兼容性测试：使用固定 golden files 验证格式稳定性。
- Benchmark：顺序写、随机冷读、随机热读、顺序扫描、压缩率和索引内存占用。

## 16. 版本里程碑

### M1：格式与基础 I/O

- 确定二进制格式 v1。
- 完成文件头、Block、Record、Footer 编解码。
- 完成 Zstd/None 压缩及 CRC。

### M2：快照与索引

- 完成 FULL/DELTA 生命周期。
- 完成 Snapshot、Block、Row 三类索引。
- 完成 Get 和历史快照解析。

### M3：并发、扫描与缓存

- 完成多读单写模型。
- 完成逻辑表扫描。
- 完成可配置 Block Cache 和统计信息。

### M4：可靠性与发布

- 完成恢复、全量校验和故障注入。
- 完成 Fuzz、race、兼容性测试和 Benchmark。
- 发布格式说明、Go API 文档及 v1.0。

## 17. 后续版本候选能力

以下能力不影响 v1 验收：

- 离线或在线 Compaction。
- 快照 Checkpoint，缩短过长 DELTA 链。
- mmap、分页索引或磁盘有序索引。
- 业务主键到 RowID 的独立索引。
- LZ4 等额外压缩算法。
- 列裁剪、块级统计信息和谓词过滤。
- 快照删除、保留策略和空间回收。
- 文件级加密和签名。

## 18. 待详细设计确认事项

以下事项不改变总体需求，但必须在编码前由详细设计文档定稿：

1. Row 的规范化二进制编码及十进制、时间类型的精确定义。
2. `.rpk` 与 `.rpi` 各结构的固定字段、字节数、对齐和校验范围。
3. 数据文件与索引文件跨文件提交的精确协议及恢复状态机。
4. Row Index 是每快照增量索引、持久化合并索引还是二者结合。
5. FULL 快照覆盖全部 Store 还是允许按表形成一致快照；v1 建议为 Store 级快照。
6. RowID 由调用者提供和引擎自动分配同时存在时的冲突规则。
7. Schema 演进允许的变更集合及历史行解码规则。
8. 默认最大行大小、最大块大小、最大父链深度和默认缓存容量。
9. 文件路径 API 是接收基名、目录还是显式双文件路径；v1 建议接收基名并自动生成扩展名。

这些事项确认后，应产出《RowPack 二进制格式 v1》和《RowPack Go API 设计》两份配套文档。
