# RowPack v2 Go API 设计

> 状态：设计草案
> 日期：2026-09-08
> 格式基线：[BINARY_FORMAT_V2.md](BINARY_FORMAT_V2.md)

## 1. API 原则

- `Create` 和 `Open` 只处理 v2 单文件 Store；
- 非 v2 文件返回 `ErrUnsupportedFormat`，不做双格式探测或迁移；
- 不用 API 名称暴露物理 IndexTxn、offset 或 mmap 实现；
- 批量 API 以 RowID 为存储层边界，不接受源数据库方言 Key；
- 返回顺序、内存所有权、取消和错误语义必须明确；
- Source Metadata API 不随单文件重构顺带加入，另行设计。

## 2. 创建与打开

不公开格式选择选项：

```go
type Options struct {
    ReadOnly         bool
    BlockSize        int
    Compression      Compression
    CompressionLevel int
    CacheBytes       int64
    RuntimeIndexBytes int64
    DecodedCacheBytes int64
    ScanBufferBytes   int64
    Durability       Durability
    Validation       ValidationMode
    Limits           Limits
}
```

行为：

- `Create(base, Options{})` 创建 `<base>.rpk`；
- `Open(base, opts)` 只读取该单文件；
- 路径以 `.rpk` 结尾是否允许，应在 API 定型时统一。

## 3. 批量读取输入

```go
type RowIDRange struct {
    Start RowID // inclusive
    End   RowID // exclusive；0 可表示无上界仅限 Scan，不建议用于通用 Batch
}

type BatchOrder uint8

const (
    BatchOrderRowID BatchOrder = iota
    BatchOrderInput
)

type BatchReadOptions struct {
    Order          BatchOrder
    MaxRows        uint64
    MaxBytes       uint64
    PrefetchBlocks int
    Parallelism    int
}
```

范围必须满足 `Start < End`。多个范围允许重叠，进入 planner 后先规范化、合并和去重。

## 4. 批量读取 API

批量结果不应强制一次性装入内存，推荐迭代器：

```go
func (s *Store) ReadRowsByIDs(
    ctx context.Context,
    snapshot SnapshotID,
    table TableID,
    rowIDs []RowID,
    opts BatchReadOptions,
) (*BatchIterator, error)

func (s *Store) ReadRowRanges(
    ctx context.Context,
    snapshot SnapshotID,
    table TableID,
    ranges []RowIDRange,
    opts BatchReadOptions,
) (*BatchIterator, error)

type BatchIterator struct { /* internal */ }

func (it *BatchIterator) Next(dst Row) (rowID RowID, row Row, ok bool)
func (it *BatchIterator) Err() error
func (it *BatchIterator) Close() error
func (it *BatchIterator) Stats() BatchReadStats
```

`BatchOrderRowID` 是默认值，允许 planner 按物理 Block offset 读取后用有界缓冲恢复 RowID
顺序。`BatchOrderInput` 只适用于 `ReadRowsByIDs`，重复输入 RowID 是否重复返回必须在实现前
固定；建议输入先去重，每个可见 Row 只返回一次。

## 5. 读取统计

```go
type BatchReadStats struct {
    RequestedIDs     uint64
    RequestedRanges  uint64
    CandidateBlocks  uint64
    BlocksRead       uint64
    BlocksDecoded    uint64
    CacheHits        uint64
    StoredBytesRead  uint64
    RawBytesDecoded  uint64
    RowsReturned     uint64
}
```

统计用于性能诊断，不作为事务一致性信息。迭代器未结束时返回当前累计值。

## 6. Planner 语义

批量读取内部流程：

```text
validate/normalize request
→ capture immutable Snapshot view
→ resolve FULL/DELTA layers
→ filter Blocks by TableID and RowID envelope
→ sort candidate Blocks by physical offset
→ merge adjacent reads under byte budget
→ decode each Block once
→ resolve newest row/tombstone
→ emit in requested order
```

同一请求内 `(SnapshotID, BlockID)` 只允许读取、解密、解压和完整校验一次。请求取消后
必须停止预读和解压，并释放尚未返回的缓冲。

## 7. Get 与稀疏导航

`Get` 继续使用持久化 Row Index 直接定位目标 Block：

```go
func (s *Store) Get(ctx context.Context, snapshot SnapshotID, table TableID, rowID RowID, dst Row) (Row, error)
```

建议策略：

1. 查询 `index.View.ResolveRow`；
2. 通过 BlockID 和 ItemOrdinal 定位 Block 内记录；
3. 读取、解密、解压和校验目标 Block；
4. 使用 ItemOrdinal 解码目标 Row；
5. DELTA 当前层不存在时，沿父 Snapshot 重复定位。

后续可以增加批量访问的运行时 Block 规划，但不得改变 Row Index 的正确性语义。

## 8. Scan

现有 `Scan` 继续按 RowID 升序，并使用有界流式解压窗口。新增选项可以后续扩展，但 v2
默认不得把一次全表 Scan 的全部解压 Block 晋升为长期缓存。

`ReadRowRanges` 与 `Scan(StartRowID, EndRowID)` 的差异：

- 单连续范围、RowID 升序输出优先使用 Scan；
- 多范围或离散 RowID 使用 BatchIterator；
- 两者共享 Block planner 和解码内核。

## 9. 缓冲所有权

- `Next(dst)` 复用调用者 Row；nil dst 使用迭代器内部缓冲；
- 返回 Row 至下一次使用同一 dst 的调用前有效；
- String/Bytes/Decimal 访问器继续返回独立副本；
- 预读 Block 缓冲由迭代器持有，`Close` 或结束时释放；
- `MaxBytes` 限制结果和处理中间缓冲，超过返回可识别的资源限制错误。

## 10. FULL checkpoint 与文件重写

FULL checkpoint 不需要新增特殊格式：上层读取目标 Snapshot 的完整可见状态，再写入一个
新的 FULL Snapshot。

未来如需压实或整理 v2 文件，可以提供只处理 v2 的重写 API：

```go
type RewriteOptions struct {
    Compression      Compression
    CompressionLevel int
    BlockSize        int
    Encryption       *EncryptionOptions
    VerifySource     bool
}

func Rewrite(ctx context.Context, sourceBase, targetBase string, opts RewriteOptions) error
```

重写只生成新的 v2 文件，成功后不自动删除或替换源文件。

## 11. 错误

建议新增或明确：

```text
ErrUnsupportedFormat
ErrInvalidRange
ErrBatchLimit
ErrMissingKey
ErrAuthentication
```

点查找不到、范围中存在空洞和已删除行不是整个 Batch 的错误；默认跳过不可见 RowID，
并在统计中体现请求数和返回数差异。

## 12. 待验证 API 决策

1. `BatchOrderInput` 是否值得支持；
2. 重复 RowID 的返回语义；
3. BatchIterator 是否需要暴露 Block 边界事件；
4. `MaxBytes` 是限制返回值、工作集还是二者；
5. 批量读取是否提供回调便利 API；
