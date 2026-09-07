# RowPack Go API 设计

> 状态：设计基线  
> 目标模块：`github.com/rowpack/rowpack`（发布前替换为最终路径）  
> 配套格式：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md)  
> 元数据格式：[METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md)

## 1. 设计原则

- API 表达 Store、Schema、Snapshot 和 Row，不暴露磁盘 offset。
- 读方法可安全并发调用；一个 Store 同时只有一个 SnapshotWriter。
- 长耗时方法接受 `context.Context`。
- 默认使用 Zstd、256 KiB Block、SyncCommit。
- 错误支持 `errors.Is/As`，提交结果未知必须显式表达。
- 使用强类型 `Value`，避免 `[]any` 的平台类型歧义。
- 读入口统一为借用/复用模式，不引用可被缓存淘汰或复用的缓冲区：`Get`/
  `Iterator.Next` 解码进调用者提供的 dst（返回值别名 dst，由调用者掌控生命
  周期）；`Iterator.Next(nil)` 使用迭代器内部缓冲，跨调用复用，返回的 Row
  到下一次 Next 前有效。

## 2. 包结构

首版只导出根包 `rowpack`，实现放入：

```text
internal/fileformat   固定结构编解码、CRC、边界验证
internal/codec        Schema 与 TypedTuple
internal/block        Block 构建、Zstd、Directory
internal/index        索引事务与不可变内存视图
internal/cache        并发 Block Cache
internal/recovery     打开、校验、恢复
```

## 3. 基础类型

```go
type SnapshotID uint64
type TableID uint32
type RowID uint64
type SchemaVersion uint32
```

`SnapshotID(0)` 仅表示无父快照；其余零 ID 对普通对象非法。

### 3.1 逻辑类型

```go
type Type uint16

const (
	TypeBool Type = iota + 1
	TypeInt8
	TypeInt16
	TypeInt32
	TypeInt64
	TypeUint8
	TypeUint16
	TypeUint32
	TypeUint64
	TypeFloat32
	TypeFloat64
	TypeString
	TypeBytes
	TypeDate
	TypeTime
	TypeDateTime
	TypeDecimal
)
```

枚举值与磁盘 Type ID 相同，发布后不得重排。

```go
type Date int32       // Unix epoch 起的日数
type TimeOfDay int64  // 午夜起纳秒数

type Decimal struct {
	Unscaled *big.Int
	Scale    int32
}

func NewDate(t time.Time) Date
func (d Date) Time(loc *time.Location) time.Time
func NewTimeOfDay(hour, min, sec, nsec int) (TimeOfDay, error)
```

DateTime 写入时转换为 UTC，舍弃时区名和 monotonic clock，保留纳秒精度。

### 3.2 Value 与 Row

```go
type Value struct { /* private immutable representation */ }

func Null() Value
func Bool(v bool) Value
func Int8(v int8) Value
func Int16(v int16) Value
func Int32(v int32) Value
func Int64(v int64) Value
func Uint8(v uint8) Value
func Uint16(v uint16) Value
func Uint32(v uint32) Value
func Uint64(v uint64) Value
func Float32(v float32) Value
func Float64(v float64) Value
func String(v string) Value
func Bytes(v []byte) Value
func DateValue(v Date) Value
func TimeValue(v TimeOfDay) Value
func DateTime(v time.Time) Value
func DecimalValue(v Decimal) Value

func (v Value) IsNull() bool
func (v Value) Type() Type
func (v Value) Bool() (bool, bool)
// 每种类型均提供对应 getter

type Row []Value
```

Bytes、Decimal 构造器复制输入，getter 返回副本。NULL 不携带固有类型，由 Schema 列决定。Writer 在写方法返回前完成编码或深拷贝；Get/Iterator 返回独立 Row。

## 4. Schema

```go
type Column struct {
	Name     string
	Type     Type
	Nullable bool
	Scale    int32 // 仅 Decimal 使用
}

type Schema struct {
	TableID TableID
	Version SchemaVersion
	Name    string
	Columns []Column
}

type Table struct {
	ID            TableID
	Name          string
	LatestVersion SchemaVersion
}
```

```go
func (w *SnapshotWriter) DefineSchema(schema Schema) error
func (s *Store) Schema(ctx context.Context, snapshot SnapshotID, table TableID, version SchemaVersion) (Schema, error)
func (s *Store) LatestSchema(ctx context.Context, snapshot SnapshotID, table TableID) (Schema, error)
func (s *Store) Tables(ctx context.Context, snapshot SnapshotID) ([]Table, error)
```

规则：

- 首次引用前必须已在当前或父快照定义 Schema。
- 重复定义仅在规范内容完全相同时幂等成功。
- 新版本必须大于表的已知最高版本。
- 返回的 Schema 和 Columns 是副本。
- `version=0` 不隐式表示 latest；使用 `LatestSchema`。

`DefineSchema` 是唯一的 Schema 契约入口，内部生成核心 Table 和 Column Metadata。

### 4.1 元数据边界

引擎**不公开通用元数据通道**（2026-09 精简决策，取代原 `PutMetadata` / `Metadata` / `ListMetadata` 设计）：

- 公开 API 不提供 `PutMetadata`、`DeleteMetadata`、`Metadata`、`ListMetadata` 及
  `MetadataRecord` / `MetadataField` / `MetadataQuery` / `MetadataWireType` 等类型。
- TLV 元数据机制保留在 `internal/metadata`，仅供 `DefineSchema` 自产自销的
  Table/Column 记录使用。
- 未来如需元数据透传，以完整的读写 API 一次性设计，不做只写半成品。

行解码所需的最小 Schema 契约由 `DefineSchema` 提供（见 §7），类型字符串为引擎自产自销的规范值；引擎不猜测任何数据库方言类型。

如上层（数据库备份/还原工具）需要 13 类强类型模型与方言映射（对应 `meta.Store` 的 Header、Tables、Columns、PrimaryKeys、Indexes、UniqueKeys、ForeignKeys、AutoInc、TabComments、ColComments、Views、Functions、VirtualColumns），应在**引擎之外的独立适配层**实现，按真实数据库 fixture 设计，不冻结进引擎。字段编号参考见 METADATA_FORMAT_V1.md §6/§7。

## 5. 创建与打开

### 5.1 选项

```go
type Compression uint8
const (
	CompressionDefault Compression = iota
	CompressionNone
	CompressionZstd
)

type Durability uint8
const (
	SyncCommit Durability = iota
	AsyncCommit
)

type ValidationMode uint8
const (
	ValidationStrict ValidationMode = iota
	ValidationNone // 只关闭父视图中行存在性检查
)

type Limits struct {
	MaxRowBytes         uint32
	MaxRawBlockBytes    uint32
	MaxStoredBlockBytes uint32
	MaxColumns          uint32
	MaxValueBytes       uint32
	MaxSnapshotDepth    uint32
}

type Options struct {
	ReadOnly          bool
	BlockSize        int
	Compression      Compression
	CompressionLevel int
	CacheBytes       int64
	Durability       Durability
	Validation       ValidationMode
	Limits           Limits
}
```

`Options` 零值即默认值（Zstd、256 KiB Block、64 MiB Cache、SyncCommit、Strict），无独立的 `DefaultOptions` 构造器。

API 的 Compression 枚举不直接等于磁盘枚举：`CompressionDefault=0` 在创建时解析为 Zstd，磁盘仍写 `0=None, 1=Zstd`。这样既保持 Options 零值安全，也允许调用者明确选择 None。

默认值为 256 KiB Block、Zstd 默认级别、64 MiB Cache、SyncCommit、Strict；安全限制与格式文档一致。字段零值由 Create/Open 补默认值，显式禁用缓存使用 `CacheBytes=-1`。

### 5.2 路径与函数

```go
func Create(basePath string, opts Options) (*Store, error)
func Open(basePath string, opts Options) (*Store, error)
```

API 接受不带扩展名的基名，自动使用 `<base>.rpk/.rpi`。以这两个扩展名结尾返回 `ErrInvalidPath`。

- Create 使用 exclusive create，任一文件已存在则不覆盖。
- Open 要求两文件存在；索引完全缺失使用显式 RebuildIndex。
- ReadOnly 不获取 writer lock、不截断文件。
- 读写 Open 获取跨进程独占 writer lock；平台不支持时返回错误，不静默降级。

## 6. Store 生命周期

```go
type Store struct { /* unexported */ }

func (s *Store) Path() string
func (s *Store) ReadOnly() bool
func (s *Store) Close() error
```

- 读取方法支持 goroutine 并发。
- 同一 Store 同时最多一个活动 Writer；Begin 不等待，冲突返回 `ErrWriterBusy`。
- Close 可并发且幂等，拒绝新操作后等待已进入的操作结束。
- Close 遇活动 Writer 时 Abort；错误通过 `errors.Join` 返回。
- Close 后除 Path、ReadOnly、Close 外均返回 `ErrClosed`。

## 7. Snapshot 写入

### 7.1 类型与元数据

```go
type SnapshotType uint8
const (
	SnapshotFull SnapshotType = iota + 1
	SnapshotDelta
)

type SnapshotOptions struct {
	Parent     SnapshotID
	AllowEmpty bool
	CreatedAt  time.Time // 零值使用当前 UTC 时间
}

type SnapshotInfo struct {
	ID          SnapshotID
	Type        SnapshotType
	Parent      SnapshotID
	CreatedAt   time.Time
	BlockCount  uint32
	ChangeCount uint64
	RawBytes    uint64
	StoredBytes uint64
}
```

```go
func (s *Store) BeginSnapshot(ctx context.Context, typ SnapshotType, opts SnapshotOptions) (*SnapshotWriter, error)
```

Full 要求 Parent=0；Delta 要求 Parent 是已提交快照。SnapshotID 在 Begin 时分配，Abort 后不复用。

### 7.2 Writer API

```go
type ChangeType uint8
const (
	ChangeInsert ChangeType = iota + 1
	ChangeUpdate
	ChangeDelete
)

type Change struct {
	Type          ChangeType
	TableID       TableID
	RowID         RowID
	SchemaVersion SchemaVersion
	Row           Row
}

type SnapshotWriter struct { /* unexported */ }

func (w *SnapshotWriter) ID() SnapshotID
func (w *SnapshotWriter) Parent() SnapshotID
func (w *SnapshotWriter) DefineSchema(schema Schema) error
func (w *SnapshotWriter) Insert(ctx context.Context, table TableID, rowID RowID, schema SchemaVersion, row Row) error
func (w *SnapshotWriter) Update(ctx context.Context, table TableID, rowID RowID, schema SchemaVersion, row Row) error
func (w *SnapshotWriter) Delete(ctx context.Context, table TableID, rowID RowID) error
func (w *SnapshotWriter) Apply(ctx context.Context, changes <-chan Change) error
func (w *SnapshotWriter) Commit(ctx context.Context) (SnapshotInfo, error)
func (w *SnapshotWriter) Abort() error
```

Writer 不支持并发调用；调用方必须自行串行化对同一 Writer 的访问。状态机：

```text
Open --Commit success--> Committed
  |----Abort-----------> Aborted
  `----Commit error----> Failed
```

终态不可复用。Commit 出错后不得重试同一 Writer；重新 Open 并查询其 ID。Abort 幂等，但对 Committed 返回 `ErrSnapshotCommitted`。

Apply 消费 channel 至关闭、ctx 取消或第一项错误；发生错误后 Writer 进入 Failed。DELETE 要求 SchemaVersion=0、Row=nil。

### 7.3 校验

Strict 模式检查：ID 非零；Schema 存在；列数、类型、Nullable、Decimal Scale 正确；字符串 UTF-8；时间和值大小合法；快照内 RowKey 不重复；FULL 只含 INSERT；DELTA INSERT 在父视图不存在且 UPDATE/DELETE 存在。

ValidationNone 仅关闭最后的父视图存在性检查，不关闭格式、Schema、重复键或资源限制校验。

## 8. 随机读取

```go
func (s *Store) Get(ctx context.Context, snapshot SnapshotID, table TableID, rowID RowID, dst Row) (Row, error)
func (s *Store) Exists(ctx context.Context, snapshot SnapshotID, table TableID, rowID RowID) (bool, error)

func (s *Store) Snapshot(ctx context.Context, id SnapshotID) (SnapshotInfo, error)
func (s *Store) LatestSnapshot(ctx context.Context) (SnapshotInfo, error)
func (s *Store) ListSnapshots(ctx context.Context) ([]SnapshotInfo, error)
```

Get 捕获调用开始时的不可变索引视图，沿父链解析；并发 Commit 不改变该次读取。Block 必须验证解压长度和 CRC。Exists 仅解析索引/Tombstone，不读取 Block；不存在返回 `(false,nil)`，Get 返回 `ErrNotFound`。

Get 是唯一的读入口（借用/复用模式）：行解码进调用者提供的 dst，复用其底层
数组与已有的 Decimal `*big.Int`，消除每次调用的 Row 分配。返回的 Row 别名
dst；下一次对同一 dst 的 Get 会覆盖其内容。String/Bytes 访问器仍返回副本，
Decimal 访问器也返回副本，因此通过访问器读值始终安全；保留原始 Value 结构
体跨多次调用则可能观察到 Decimal 被覆盖。nil dst 由引擎分配。出错时 dst
内容未定义。

空 Store 的 LatestSnapshot 返回 `ErrNotFound`。ListSnapshots 按 ID 升序并返回新切片。

## 9. Scan Iterator

```go
type ScanOptions struct {
	StartRowID RowID // inclusive；0=从头
	EndRowID   RowID // exclusive；0=无上界
}

type Iterator struct { /* unexported */ }

func (s *Store) Scan(ctx context.Context, snapshot SnapshotID, table TableID, opts ScanOptions) (*Iterator, error)
func (it *Iterator) Next(dst Row) (Row, bool)
func (it *Iterator) RowID() RowID
func (it *Iterator) Err() error
func (it *Iterator) Close() error
```

语义：

- 输出 RowID 严格升序，已覆盖版本与 Tombstone 不输出。
- Iterator 捕获创建时的不可变索引视图。
- Next 是唯一的推进入口（借用/复用模式）：
  - `Next(nil)`：解码进迭代器内部缓冲，缓冲跨调用复用（只分配一次、按需增
    长），常规循环无需任何回写；返回的 Row 到下一次 Next 前有效。
  - `Next(dst)`：解码进调用者提供的 dst，复用其底层数组与 Decimal
    `*big.Int`；返回的 Row 别名 dst。适合显式管理缓冲的热点路径。
  两种模式下需要跨调用保留的值都需拷贝（通过访问器读值始终安全）。RowID
  任意模式下均可用。
- Next=false 后检查 Err；Close 幂等。
- ctx 取消后尽快停止并返回标准 context 错误。
- 内部应对父链排序索引做 k-way merge，不构建与整表行数同规模的 map。

## 10. 校验、恢复和重建

```go
type VerifyMode uint8
const (
	VerifyQuick VerifyMode = iota
	VerifyFull
)

type VerifyReport struct {
	SnapshotsChecked uint64
	BlocksChecked    uint64
	RowsChecked      uint64
	DataBytesRead    uint64
	Duration         time.Duration
}

func (s *Store) Verify(ctx context.Context, mode VerifyMode) (VerifyReport, error)
```

Quick 检查头、索引事务、Footer、边界和父链；Full 额外解压所有 Block 并验证所有 Row。

```go
type RebuildOptions struct {
	ReplaceCorrupt bool
	Durability     Durability
}

func RebuildIndex(ctx context.Context, basePath string, opts RebuildOptions) error
```

缺失索引用显式 RebuildIndex。`ReplaceCorrupt=true` 时写临时文件、完整同步后原子 rename，不原地覆盖。普通 Open 只修复有效索引的尾部或补充数据领先部分。

## 11. 统计信息

```go
type Stats struct {
	Snapshots        uint64
	Tables           uint64
	Blocks           uint64
	LogicalRows      uint64
	DataFileBytes    int64
	IndexFileBytes   int64
	RawBytes         uint64
	StoredBytes      uint64
	IndexMemoryBytes uint64
	Cache            CacheStats
	Recovery         RecoveryStats
}

type CacheStats struct {
	CapacityBytes uint64
	UsedBytes     uint64
	Hits, Misses, Evictions, Loads uint64
}

type RecoveryStats struct {
	Performed        bool
	DataTailIgnored  uint64
	IndexTailIgnored uint64
	SnapshotsRebuilt uint64
}

func (s *Store) Stats() Stats
```

Stats 是近似快照，不建立事务屏障。LogicalRows 指最新快照的逻辑行数。

## 12. 错误模型

```go
var (
	ErrNotFound            = errors.New("rowpack: not found")
	ErrAlreadyExists       = errors.New("rowpack: already exists")
	ErrInvalidPath         = errors.New("rowpack: invalid path")
	ErrInvalidArgument     = errors.New("rowpack: invalid argument")
	ErrReadOnly            = errors.New("rowpack: read only")
	ErrWriterBusy        = errors.New("rowpack: writer busy")
	ErrSnapshotCommitted = errors.New("rowpack: snapshot committed")
	ErrSnapshotAborted     = errors.New("rowpack: snapshot aborted")
	ErrSnapshotFailed      = errors.New("rowpack: snapshot failed")
	ErrInvalidParent       = errors.New("rowpack: invalid parent snapshot")
	ErrSchemaMismatch      = errors.New("rowpack: schema mismatch")
	ErrSchemaConflict     = errors.New("rowpack: schema conflict")
	ErrCorruptData        = errors.New("rowpack: corrupt data")
	ErrCorruptIndex        = errors.New("rowpack: corrupt index")
	ErrVersionUnsupported  = errors.New("rowpack: unsupported version")
	ErrStoreMismatch       = errors.New("rowpack: store files do not match")
	ErrClosed              = errors.New("rowpack: closed")
)
```

```go
type CorruptionError struct {
	File       string
	Offset     int64
	SnapshotID SnapshotID
	TableID    TableID
	BlockID    uint64
	Kind       error // ErrCorruptData 或 ErrCorruptIndex
	Reason     string
}
func (e *CorruptionError) Error() string
func (e *CorruptionError) Unwrap() error

type CommitError struct {
	SnapshotID SnapshotID
	Unknown    bool
	Err        error
}
func (e *CommitError) Error() string
func (e *CommitError) Unwrap() error
```

`.rpk` sync 已开始后的 Commit 错误设置 `Unknown=true`。错误信息不得包含完整行值。

## 13. 并发与可见性契约

Store 内部建议使用 `atomic.Pointer[indexView]` 指向不可变视图。读操作登记生命周期后只 Load 一次 view，并在无全局锁状态下完成索引解析、ReadAt 和解压。Commit 在单写锁内顺序写两个文件，基于旧 view 构建新 view，再原子发布。已发布 map/slice 永不原地修改。

Block Cache：

- key 是 StoreUUID + BlockID；Store 局部缓存可隐含 UUID。
- value 是校验后的完整 Raw Block。
- 并发 miss 用 singleflight 或等价方式合并。
- 大于容量的单 Block 可读取但不缓存。
- 淘汰不得影响正在解码的读取。
- CRC 失败的 Block 不进入缓存。

## 14. 使用示例

```go
opts := rowpack.Options{} // 零值即默认：Zstd、256 KiB、64 MiB Cache、SyncCommit、Strict
db, err := rowpack.Create("/data/users-backup", opts)
if err != nil { return err }
defer db.Close()

w, err := db.BeginSnapshot(ctx, rowpack.SnapshotFull, rowpack.SnapshotOptions{})
if err != nil { return err }
defer w.Abort()

schema := rowpack.Schema{
	TableID: 1,
	Version: 1,
	Name: "users",
	Columns: []rowpack.Column{
		{Name: "id", Type: rowpack.TypeUint64},
		{Name: "name", Type: rowpack.TypeString},
		{Name: "created_at", Type: rowpack.TypeDateTime},
	},
}
if err := w.DefineSchema(schema); err != nil { return err }

err = w.Insert(ctx, 1, 1001, 1, rowpack.Row{
	rowpack.Uint64(1001),
	rowpack.String("张三"),
	rowpack.DateTime(time.Now()),
})
if err != nil { return err }

full, err := w.Commit(ctx)
if err != nil { return err }

row, err := db.Get(ctx, full.ID, 1, 1001, nil)
```

DELTA：

```go
w, err = db.BeginSnapshot(ctx, rowpack.SnapshotDelta, rowpack.SnapshotOptions{Parent: full.ID})
if err != nil { return err }
defer w.Abort()

if err := w.Update(ctx, 1, 1001, 1, updatedRow); err != nil { return err }
if err := w.Delete(ctx, 1, 1002); err != nil { return err }
delta, err := w.Commit(ctx)
```

## 15. 兼容性与实现顺序

- 磁盘枚举数值发布后不变；公开 API 遵循 Go 语义化版本。
- 新 Options 字段必须保证零值兼容。
- 内部磁盘结构不得导出，避免文件格式和 API 实现耦合。
- API major 与磁盘格式版本独立演进；同一模块 major 保留磁盘 v1 读取能力。

建议实现顺序：

1. fileformat 固定结构、CRC、边界检查及 golden files。
2. TypedTuple/Schema codec 与 fuzz。
3. Block、None/Zstd、超大行。
4. IndexTxn 重放和 immutable view。
5. Store、Get、SnapshotWriter。
6. Scan、Cache、Stats。
7. Recovery、Verify、故障注入与 race test。
