# RowPack Go API 设计（v1）

> 状态：已实现并冻结（v1 为唯一公开 API 线，格式未发布、无历史兼容包袱）
> 格式基线：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md)
> 元数据：[METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md)
> 加密：[ENCRYPTION_V1.md](ENCRYPTION_V1.md)

## 1. API 原则

- `Create` 和 `Open` 只处理 v1 单文件 Store；
- 非 v1 文件返回 `ErrVersionUnsupported`，不做多格式探测或迁移；
- 不用 API 名称暴露物理 IndexTxn、offset 或 mmap 实现；
- 批量 API 以 RowID 为存储层边界，不接受源数据库方言 Key；
- 返回顺序、内存所有权、取消和错误语义必须明确；
- Source Metadata 不进入本版本的 API：源库元信息由调用方以自选 ns 下的
  普通目录表承载（`DefineTableIn` 到自选 ns + `Insert/Update/Delete`），
  复用既有行 API，不新增元数据读写通道。

## 2. 创建与打开

```go
func Create(basePath string, opts Options) (*Store, error)
func Open(basePath string, opts Options) (*Store, error)
```

不公开格式选择选项；`Create` 固定写出 v1 Magic/Major（`ROWPACK1`/1）。`basePath`
不带扩展名，实际文件为 `<basePath>.rpk`。加密只在 `Create` 时确定，`Open` 必须能
提供同一 `KeyProvider`（见下）。

### Options

```go
type Options struct {
	ReadOnly         bool
	BlockSize        int
	PageSize         int
	Compression      Compression
	CompressionLevel int

	CacheBytes     int64 // 解码数据总预算：DataCache + ScanWindow 不超过它；负值禁用缓存
	ScanCacheBytes int64 // 扫描窗口显式上限；0 取默认拆分，负值禁用扫描窗口

	Durability Durability
	Validation ValidationMode
	Limits     Limits
	Encryption *EncryptionConfig // nil = 明文 store，仅在 Create 时设置
}
```

零值全部由 `Create`/`Open` 替换为默认值，新增字段必须保持零值安全。约束：

- `PageSize <= BlockSize`（小测试块自动把页下调到块大小）；两者最小 64 B；
- `ScanCacheBytes < CacheBytes`（否则扫描窗口吃光随机读预算）；
- `BlockSize <= Limits.MaxRawBlockBytes`，`Limits.MaxRowBytes <= Limits.MaxRawBlockBytes`。

### 枚举与限制

```go
type Compression uint8    // CompressionDefault(0) / CompressionNone / CompressionZstd
type Durability uint8     // SyncCommit / AsyncCommit
type ValidationMode uint8 // ValidationStrict / ValidationNone

type Limits struct {
	MaxRowBytes         uint32
	MaxRawBlockBytes    uint32
	MaxStoredBlockBytes uint32
	MaxColumns          uint32
	MaxValueBytes       uint32
	MaxSnapshotDepth    uint32
}
```

- `SyncCommit`：返回前 fsync；`AsyncCommit`：不主动 fsync，进程崩溃可能丢失最近提交；
- `ValidationNone` 只跳过父视图的行存在性检查，不跳过任何 CRC/AEAD/结构校验。

### 加密配置

```go
type EncryptionConfig struct {
	KeyProvider KeyProvider
	KeyID       string
}

type KeyProvider interface {
	Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error)
}
```

`Key` 返回 32 字节 AES-256 密钥；`epoch` 0 为初始代次，轮换后按各 Block 的
`KeyEpoch` 请求对应密钥。RowPack 不保存、生成或托管主密钥。明文 store 的
`Open`/`Verify` 不需要 `KeyProvider`；加密 store 缺少密钥时返回 `ErrKeyRequired`。

## 3. 写路径

```go
const NoParent SnapshotID = 0
const Latest SnapshotID = ^SnapshotID(0)

func (s *Store) Begin(ctx context.Context, parent SnapshotID) (*Tx, error)

func (tx *Tx) ID() SnapshotID
func (tx *Tx) Parent() SnapshotID
func (tx *Tx) DefineTable(name string, columns []Column) error
func (tx *Tx) Insert(table string, id RowID, row Row) error
func (tx *Tx) Update(table string, id RowID, row Row) error
func (tx *Tx) Delete(table string, id RowID) error
func (tx *Tx) Apply(change Change) error
func (tx *Tx) ApplyBatch(changes []Change) error
func (tx *Tx) Commit(ctx context.Context) (SnapshotID, error)
func (tx *Tx) Rollback() error
```

- `Begin(ctx, NoParent)` 创建 FULL baseline；任何已提交的 SnapshotID 创建基于它的
  DELTA；`Begin(ctx, Latest)` 以最新已提交快照为父（空 store 返回 `ErrInvalidParent`）。
- FULL 可在任意时刻提交（checkpoint），SnapshotID 全局递增、深度重置；FULL 只接受
  INSERT。
- `Change` 是行变更的通用载体；`Row` 对 DELETE 必须为 nil，对 INSERT/UPDATE 必须是
  完整行：

```go
type Change struct {
	Type  ChangeType // Insert / Update / Delete
	Table string
	RowID RowID
	Row   Row
}
```

- `ApplyBatch` 按顺序消费，不持有也不复制输入 slice，返回后调用方可立即复用缓冲；
  批次中某条失败时，之前的变更仍属于本事务，`Rollback` 丢弃整个事务。
- `Commit` 发布新快照；`Rollback` 可安全地紧跟在 `Begin` 后 defer，成功 Commit 后
  返回 `ErrSnapshotCommitted`。
- 表必须先于行写入定义。`DefineTable*` 在同一快照内重复定义同列是幂等 no-op，
  同地址不同列返回 `ErrSchemaConflict`；DELTA 里链上已有的表可以直接写，而 FULL
  的元数据不沿父链可见，所以它必须自己定义写的每一张表：否则 `Commit` 返回
  `ErrInvalidArgument`，且不落任何字节。
- 写路径为单写者串行；并发 `Begin` 返回 `ErrWriterBusy`。

## 4. 读路径

```go
func (s *Store) Get(ctx context.Context, snapshot SnapshotID, table string, rowID RowID, dst Row) (Row, error)
func (s *Store) Exists(ctx context.Context, snapshot SnapshotID, table string, rowID RowID) (bool, error)
func (s *Store) ReadBatch(ctx context.Context, snapshot SnapshotID, table string, ids []RowID) ([]Row, error)

func (s *Store) Scan(ctx context.Context, snapshot SnapshotID, table string, opts ScanOptions) (*Iterator, error)
func (s *Store) ScanBlocks(ctx context.Context, snapshot SnapshotID, table string, lo, hi uint64) (*Iterator, error)
func (s *Store) Blocks(ctx context.Context, snapshot SnapshotID, table string) ([]Block, error)
```

### Get / Exists

`Get` 用持久化 Row Index 直接定位目标 Block：`ResolveRow` → `(BlockID, ItemOrdinal)`
→ 读取/解密/解压/校验目标页 → 按 `ItemOrdinal` 解码；DELTA 当前层不存在时沿父快照
重复定位。`dst` 为 nil 时每次分配新行，非 nil 时复用其底层数组（见 §6）。

### ReadBatch

按块聚合：同一 `(SnapshotID, BlockID)` 在一次调用内至多加载、解密、解压和校验一次，
与请求行数无关。语义与逐行 `Get` 一致：

- 任一 id 在快照不可见（缺失或已删除）→ 整批返回 `ErrNotFound`；
- 返回顺序与输入 `ids` 一一对应，**重复输入重复返回，不静默去重**；
- 返回的行归调用方所有、互不别名。

`ReadBatch` 每次调用分配输出与工作缓冲（7 列时约 700 B/行，主要是 `Value` slab）；
聚合、顺序和错误语义与 `Get` 逐行一致，没有额外的复用接口。

### Scan / Iterator

```go
type ScanOptions struct {
    Start RowID // inclusive；0 = 从头开始
    End RowID // exclusive；0 = 无上界
}

func (it *Iterator) Next() (Row, bool)
func (it *Iterator) RowID() RowID
func (it *Iterator) ChangeType() ChangeType
func (it *Iterator) Err() error
func (it *Iterator) Close() error
```

`Scan` 按 RowID 升序流式产出逻辑可见行，过滤被后代覆盖的行和 Tombstone；创建时捕获
不可变索引视图，并发提交不影响它。`ScanBlocks` 改为输出块内原始变更流（`ChangeType`
报告每个记录的原始类型，包含 DELETE），用于块扫描批量比对。

## 5. Schema、快照与元数据

```go
func (s *Store) Schema(ctx context.Context, snapshot SnapshotID, table string, version SchemaVersion) (Schema, error)
func (s *Store) Tables(ctx context.Context, snapshot SnapshotID) ([]Table, error)
func (s *Store) TablesIn(ctx context.Context, snapshot SnapshotID, ns string) ([]Table, error)
func (s *Store) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
func (tx *Tx) DefineTable(name string, columns []Column) error
func (tx *Tx) DefineTableIn(ns, name string, columns []Column) error
```

- 表按**地址**寻址（默认 ns 用裸名）；`Tables` 返回表身份与最新 SchemaVersion；
- 每张表属于一个**ns**（`Table.NS`）。默认是 `NSUser`（`user`），
  此时 Table 记录省略 `NS` 字段，所以既有 store 的字节不变；
  `DefineTableIn` 可以指定别的名字， ns 随该表的 Table 记录一起持久化（见
  [METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md) §6.1），`TablesIn` 按它过滤；
- ns 是**调用方自选的标签**，引擎不赋予语义（没有“系统表”之类概念）。典型用法
  是把上层自己的目录表放进一个自有 ns，用 `TablesIn(ctx, snap,
  rowpack.NSUser)` 一次拿到源库表（见
  [SOURCE_CATALOG_GUIDE_V1.md](SOURCE_CATALOG_GUIDE_V1.md)）；
- 表的身份是 `(NS, Name)`，因此**同名表可以在不同 ns 共存**。所有收表的
  API 收的是**地址字符串**：默认 ns 用裸名（`"users"`），其他 ns 加
  `"ns."` 前缀（`"public.users"`）。`Qualify`/`SplitAddress` 是公开的地址
  工具函数；`Table.Address()` 给出结果。`.` 是地址分隔符，但**引擎不禁止任何
  字符**：地址按**第一个** `.` 切分，所以 ns 与表名都可以含 `.`（`SplitAddress` 在 ns
  自身含点时不再是 `Qualify` 的逆，以 `Table.NS`/`Table.Name` 为准）；两个不同的
  `(ns, name)` 算出同一地址是真实冲突，第二个 `DefineTable*` 报 `ErrSchemaConflict`；
- `Schema` 解析指定版本的 Canonical Schema；未知类型字符串的记录按普通数据跳过，
  不导致 Open 失败；
- `SnapshotInfo` 是已提交快照的不可变摘要（ID/Type/Parent/CreatedAt/BlockCount/
  ChangeCount/RawBytes/StoredBytes）；
- 元数据 TLV 与 `DefineTable` 写入的 Table/Column 记录见
  [METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md)。

## 6. 行复用与缓冲所有权

读入口统一为借用/复用模式：

- `Iterator.Next()` 无参数，解码进迭代器内部缓冲并跨调用复用；返回的 Row 到下一次
  `Next` 前有效，整表 Scan 无逐行分配；
- `Get(dst)` 解码进调用者提供的 Row；`Get` 是并发入口，nil dst 每次分配新行；
- `ReadBatch` 一次性返回整批行，行归调用方所有；
- `String()`/`Bytes()`/`Decimal()` 访问器始终返回独立副本，跨调用安全；
- dst 槽自身的缓冲（`Decimal` 的 `big.Int`、容量足够的 `Bytes` 底层数组）会被复用，
  保留的 Value 结构体在下一次解码进同一 dst 后可能看到被覆盖的值。

## 7. 统计与校验

```go
func (s *Store) Stats() Stats
func (s *Store) Verify(ctx context.Context, mode VerifyMode) (VerifyReport, error)
```

- `Stats` 是近似只读快照，不建立事务屏障：包含快照/表/块/逻辑行计数、缓存
  （随机读缓存 + 有界扫描窗口）、累计物理读放大（`Read`）和批量聚合（`Batch`）；
- `VerifyQuick` 校验头部、IndexTxn、Footer、边界和父链；`VerifyFull` 额外解压所有块、
  校验每行 CRC 与可解码性。失败返回结构化 `CorruptionError`。

## 8. 错误

所有公开 API 返回可用 `errors.Is` 匹配的哨兵错误，不得按字符串分类：

```text
ErrNotFound ErrAlreadyExists ErrInvalidPath
ErrInvalidArgument ErrReadOnly ErrWriterBusy
ErrSnapshotCommitted ErrSnapshotAborted ErrSnapshotFailed
ErrInvalidParent ErrSchemaMismatch ErrSchemaConflict
ErrCorruptData ErrCorruptIndex ErrVersionUnsupported
ErrStoreMismatch ErrClosed ErrMustReopen
ErrKeyRequired ErrKeyUnavailable ErrKeyIDNotFound ErrAuthFailed
```

- `CorruptionError`（Kind + 文件/offset/Snapshot/Table/Block/Cause）描述完整性失败，
  `Unwrap` 同时挂 `ErrCorruptData`/`ErrCorruptIndex` 与底层 `Cause`；
- `CommitError`：`Unknown=true` 表示同步开始后失败、提交结果未知，必须按 SnapshotID
  查询而不是盲目重放；store 同时进入 must-reopen 状态，新写者返回 `ErrMustReopen`，
  读仍自洽；
- 批量读取中不可见行不是整个 Batch 的错误（`ReadBatch` 整批 `ErrNotFound`，
  见 §4）。

## 9. 并发与生命周期

- 多读单写：读可并发，写单写者串行，提交原子可见；
- `Close` 等待在途读取，之后所有调用返回 `ErrClosed`；
- 读操作捕获创建时的不可变快照视图，提交不改变已发起的读；
- `Verify`、`Stats` 不阻塞写者（近似快照语义）。

## 10. 格式替代与兼容

- 只接受 v1 格式；双文件草案（`.rpk` + `.rpi`，未发布）不读取、不迁移；
- `RebuildIndex` 不作为公开 API：IndexTxn 损坏由 Open 内存重建，重建次数由
  `Stats().Recovery.SnapshotsRebuilt` 暴露；
- 如需压实或整理文件，另行提供只处理 v1 的 `Rewrite`，成功后不自动删除/替换源文件。
