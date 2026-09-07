# RowPack 二进制格式 v1

> 状态：设计基线  
> 格式版本：1.0  
> 配套需求：[REQUIREMENTS.md](REQUIREMENTS.md)

## 1. 格式原则

RowPack v1 使用两个文件：

```text
<base>.rpk    数据文件，提交事实的权威来源
<base>.rpi    追加式索引日志，可由 .rpk 重建
```

统一规则：

- 多字节整数使用 Little Endian；浮点数使用 IEEE 754。
- 不得把 Go struct 的内存表示直接写盘。
- 顶层结构按 8 字节对齐，Padding 写零且不计入 CRC。
- 校验统一使用 CRC-32C Castagnoli，即 Go `crc32.MakeTable(crc32.Castagnoli)`。
- 固定结构 CRC 覆盖整个结构，计算时将自身 CRC 字段视为零。
- `.rpk` 仅追加，除恢复外不覆盖已提交数据。
- 完整且校验通过的 Snapshot Footer 是快照已提交的权威标志。
- `.rpi` 是派生导航结构；损坏或缺失的尾部可从 `.rpk` 重建。
- 保留字段写入时为零；读取 v1 时忽略其值。

版本固定为 `Major=1, Minor=0`。未知 Major 必须拒绝；更高 Minor 仅在 Required Feature Bits 全部可识别时允许打开。

## 2. Store 配对与限制

创建 Store 时生成随机 16 字节 `StoreUUID`，同时写入两个文件。UUID 不同必须返回 `ErrStoreMismatch`。

建议默认安全限制：

| 项目 | 默认值 |
| --- | ---: |
| 单行编码大小 | 64 MiB |
| Block 原始/压缩大小 | 128 MiB |
| 每表列数 | 16,384 |
| 单个变长值 | 64 MiB |
| DELTA 父链深度 | 4,096 |

读取器必须在分配内存前校验长度、整数溢出、文件边界和配置限制。

## 3. `.rpk` 数据文件

### 3.1 总体布局

```text
[DataFileHeader 128]
[SnapshotHeader 96]
  [BlockHeader 64][stored payload][padding]
  ...
[SnapshotFooter 96]
[next snapshot ...]
```

### 3.2 DataFileHeader：128 字节

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `ROWPACKD` |
| 8 | 2 | VersionMajor | 1 |
| 10 | 2 | VersionMinor | 0 |
| 12 | 4 | HeaderSize | 128 |
| 16 | 16 | StoreUUID | 配对标识 |
| 32 | 8 | CreatedUnixNano | UTC Unix ns |
| 40 | 8 | RequiredFeatures | 必须支持的能力 |
| 48 | 8 | OptionalFeatures | 可忽略能力 |
| 56 | 4 | DefaultBlockSize | 目标原始块大小 |
| 60 | 1 | DefaultCompression | 0=None, 1=Zstd |
| 61 | 1 | DefaultRowEncoding | 1=TypedTuple |
| 62 | 2 | Flags | v1 为 0 |
| 64 | 56 | Reserved | 0 |
| 120 | 4 | HeaderCRC32C | Header CRC |
| 124 | 4 | ReservedCRC | 0 |

Header 创建后不更新，最近提交点仅由追加记录表达。

### 3.3 SnapshotHeader：96 字节

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPKSNAPH` |
| 8 | 4 | HeaderSize | 96 |
| 12 | 1 | SnapshotType | 1=FULL, 2=DELTA |
| 13 | 1 | Flags | bit 0=AllowEmpty |
| 14 | 2 | Reserved | 0 |
| 16 | 8 | SnapshotID | 非零、物理顺序严格递增 |
| 24 | 8 | ParentSnapshotID | FULL=0，DELTA 非零 |
| 32 | 8 | CreatedUnixNano | UTC Unix ns |
| 40 | 8 | FirstBlockID | 空快照为 0 |
| 48 | 8 | WriterNonce | 随机诊断值 |
| 56 | 32 | Reserved | 0 |
| 88 | 4 | HeaderCRC32C | Header CRC |
| 92 | 4 | ReservedCRC | 0 |

### 3.4 BlockHeader：64 字节

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPKBLOCK` |
| 8 | 4 | HeaderSize | 64 |
| 12 | 1 | BlockKind | 1=Rows, 2=Metadata |
| 13 | 1 | Compression | 0=None, 1=Zstd |
| 14 | 2 | Flags | v1 为 0 |
| 16 | 8 | BlockID | Store 内严格递增 |
| 24 | 8 | SnapshotID | 所属快照 |
| 32 | 4 | TableID | 非零 |
| 36 | 4 | ItemCount | Row 或 Metadata Record 数 |
| 40 | 4 | RawSize | 未压缩 Payload 长度 |
| 44 | 4 | StoredSize | 磁盘 Payload 长度 |
| 48 | 4 | RawCRC32C | 完整未压缩 Payload CRC |
| 52 | 4 | HeaderCRC32C | Header CRC |
| 56 | 8 | Reserved | 0 |

Payload 紧随 Header。`None` 时两种 Size 相等。Zstd 必须是完整独立 Frame，不依赖字典。Rows Block 只属于一个 Snapshot 和一个 Table。Metadata Block 属于一个 Snapshot；仅含单表对象时 TableID 可设为该表，否则为 0。

### 3.5 Rows Block Payload

解压后布局：

```text
[RowsPayloadHeader 32]
[RowDirectoryEntry × ItemCount, 24 each]
[Record bytes...]
```

RowsPayloadHeader：

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPKROWPL` |
| 8 | 4 | PayloadVersion | 1 |
| 12 | 4 | DirectoryEntrySize | 24 |
| 16 | 4 | ItemCount | 与 BlockHeader 一致 |
| 20 | 4 | DirectoryBytes | `ItemCount * 24` |
| 24 | 8 | RecordsBytes | Record 区总长度 |

RowDirectoryEntry：

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | RowID | 表内行 ID |
| 8 | 4 | RecordOffset | 相对 Raw Payload 起点 |
| 12 | 4 | RecordLength | 完整 Record 长度 |
| 16 | 1 | ChangeType | 1=INSERT, 2=UPDATE, 3=DELETE |
| 17 | 1 | Flags | 0 |
| 18 | 2 | Reserved | 0 |
| 20 | 4 | SchemaVersion | DELETE 为 0 |

Directory 按调用顺序排列。Record 紧凑、不对齐、不得重叠或越界。

RowRecordHeader：24 字节：

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | RowID | 与 Directory 一致 |
| 8 | 4 | SchemaVersion | 与 Directory 一致 |
| 12 | 1 | ChangeType | 与 Directory 一致 |
| 13 | 1 | RowEncoding | INSERT/UPDATE=1；DELETE=0 |
| 14 | 2 | Flags | 0 |
| 16 | 4 | RowLength | DELETE=0 |
| 20 | 4 | RowCRC32C | 行负载 CRC；DELETE=0 |

Record 长度为 `24 + RowLength`。Block CRC 是完整性权威，Row CRC 用于局部诊断。

### 3.6 Metadata Block Payload

Metadata Block 保存引擎的 Schema 记录（Table/Column）及任意其他 TLV 记录。核心 Table 与 Column 记录共同构成 TypedTuple Schema；其他记录作为普通数据保存/透传，引擎不解释其语义。其目录、通用 TLV Record 和字段规则见 [METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md)。解释 Rows Block 所需的 Metadata 必须已在当前物理位置之前或父快照中提交。

### 3.7 TypedTuple 行编码

类型由 Schema 决定，不在每个值前重复 Type Tag：

```text
u32 ColumnCount
u32 NullBitmapBytes          // ceil(ColumnCount/8)
bytes NullBitmap             // bit i=1 表示第 i 列为 NULL，低位优先
Value × non-null columns
```

末字节未使用高位必须为零，解码后不得有尾随字节。

| Type ID | 类型 | 编码 |
| ---: | --- | --- |
| 1 | Bool | u8，仅 0/1 |
| 2/3/4/5 | Int8/16/32/64 | 定宽二补码 |
| 6/7/8/9 | Uint8/16/32/64 | 定宽无符号 |
| 10/11 | Float32/64 | IEEE 754，保留 NaN bit |
| 12 | String | `u32 length + UTF-8 bytes` |
| 13 | Bytes | `u32 length + bytes` |
| 14 | Date | i32，Unix epoch 起的日数 |
| 15 | Time | i64，午夜起纳秒 `[0,86400e9)` |
| 16 | DateTime | i64，UTC Unix ns，不保存时区名 |
| 17 | Decimal | `u32 len + two's-complement big-endian unscaled integer` |

Decimal Scale 来自 Schema。零规范编码为 `len=1, 00`；正/负数不得含多余 `00`/`ff` 符号扩展。NULL 由 bitmap 表达，长度为 0 表示非 NULL 空值。

### 3.8 SnapshotFooter：96 字节

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPKSNAPF` |
| 8 | 4 | FooterSize | 96 |
| 12 | 1 | SnapshotType | 与 Header 一致 |
| 13 | 3 | Reserved | 0 |
| 16 | 8 | SnapshotID | 与 Header 一致 |
| 24 | 8 | ParentSnapshotID | 与 Header 一致 |
| 32 | 8 | SnapshotStartOffset | Header offset |
| 40 | 8 | SnapshotEndOffset | Footer 后的对齐位置 |
| 48 | 8 | FirstBlockID | 空快照为 0 |
| 56 | 4 | BlockCount | Block 总数 |
| 60 | 4 | MetadataBlockCount | Metadata Block 数 |
| 64 | 8 | RowRecordCount | 变更数 |
| 72 | 8 | RawBytes | 所有 RawSize 之和 |
| 80 | 4 | BlocksCRC32C | 对物理顺序的 Block Header CRC 值串计算 |
| 84 | 4 | FooterCRC32C | Footer CRC |
| 88 | 8 | Reserved | 0 |

`SnapshotEndOffset = align8(footerOffset + 96)`。Footer 完整且 CRC 正确即表示 `.rpk` 中已提交。

## 4. `.rpi` 索引文件

### 4.1 索引模型

v1 索引按快照追加事务。每个事务保存该快照的 Metadata、Block 和 Row 增量索引。打开时顺序重放有效事务到内存不可变视图，不引入磁盘 B+Tree。

```text
[IndexFileHeader 128]
[IndexTxnHeader 80]
[SnapshotIndexEntry 72]
[MetadataIndexEntry × N, 48 each]
[BlockIndexEntry × B, 56 each]
[RowIndexEntry × R, 40 each]
[IndexTxnFooter 80]
[padding]
```

### 4.2 IndexFileHeader：128 字节

与 Data Header 同一版本和 UUID。布局为：Magic `ROWPACKI` 8 字节；Major/Minor/HeaderSize；StoreUUID；CreatedUnixNano；RequiredFeatures；OptionalFeatures；64 字节 Reserved；offset 120 的 HeaderCRC32C；4 字节 Reserved。

### 4.3 IndexTxnHeader：80 字节

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPITXNBH` |
| 8 | 4 | HeaderSize | 80 |
| 12 | 4 | Flags | 0 |
| 16 | 8 | TxnSequence | 从 1 严格递增 |
| 24 | 8 | SnapshotID | 对应快照 |
| 32 | 8 | DataSnapshotStart | `.rpk` Header offset |
| 40 | 8 | DataSnapshotEnd | `.rpk` Footer 后 offset |
| 48 | 4 | MetadataEntryCount | 数量 |
| 52 | 4 | BlockEntryCount | 数量 |
| 56 | 8 | RowEntryCount | 数量 |
| 64 | 8 | BodyBytes | Header 与 Footer 间长度 |
| 72 | 4 | HeaderCRC32C | CRC |
| 76 | 4 | Reserved | 0 |

### 4.4 Index Entries

SnapshotIndexEntry：72 字节：

```text
u64 SnapshotID
u64 ParentSnapshotID
u8  SnapshotType
bytes[3] Reserved
u32 BlockCount
u64 RowRecordCount
u64 DataStart
u64 DataEnd
i64 CreatedUnixNano
u32 DataFooterCRC32C
u32 EntryCRC32C
u64 Reserved
```

MetadataIndexEntry：48 字节：

```text
u64 SnapshotID
u64 ObjectID
u32 Revision
u32 RecordType
u64 BlockID
u32 ItemOrdinal
u8  Operation
u8  Flags
u16 Reserved
u32 EntryCRC32C
u32 Reserved2
```

BlockIndexEntry：56 字节：

```text
u64 BlockID
u64 SnapshotID
u32 TableID
u8  BlockKind
u8  Compression
u16 Flags
u64 DataOffset
u32 RawSize
u32 StoredSize
u32 ItemCount
u32 RawCRC32C
u32 EntryCRC32C
u32 Reserved
```

RowIndexEntry：40 字节：

```text
u64 SnapshotID
u32 TableID
u8  ChangeType
u8  Flags
u16 Reserved
u64 RowID
u64 BlockID
u32 ItemOrdinal
u32 EntryCRC32C
```

Metadata 正文仍从 `.rpk` 读取。Row 与 Metadata Index 都是每快照增量索引，不是完整快照物化结果。同一快照内 `(TableID, RowID)` 不得重复。

### 4.5 IndexTxnFooter：80 字节

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPITXNEF` |
| 8 | 4 | FooterSize | 80 |
| 12 | 4 | Flags | 0 |
| 16 | 8 | TxnSequence | 与 Header 一致 |
| 24 | 8 | SnapshotID | 与 Header 一致 |
| 32 | 8 | TxnStartOffset | Header offset |
| 40 | 8 | TxnEndOffset | Footer 后对齐位置 |
| 48 | 8 | DataSnapshotEnd | 与 Header 一致 |
| 56 | 4 | BodyCRC32C | 所有 Entry 的原始字节 |
| 60 | 4 | DataFooterCRC32C | 对应数据 Footer CRC |
| 64 | 4 | FooterCRC32C | Footer CRC |
| 68 | 12 | Reserved | 0 |

只有 Footer、所有 Entry 和关联数据 Footer 均匹配，索引事务才有效。

## 5. 内存索引语义

重放 `.rpi` 后构建：

```text
snapshots[SnapshotID]              -> SnapshotMeta
blocks[BlockID]                    -> BlockLocation
metadata[(SnapshotID,ObjectID)]    -> MetadataLocation/Tombstone
schemas[(TableID, SchemaVersion)]  -> derived Table+Column metadata
rows[(SnapshotID,TableID,RowID)]   -> RowLocation/Tombstone
```

`Get` 从目标 Snapshot 沿父链查询增量 Row Index。扫描通过每层按 RowID 排序的增量索引进行多路合并；磁盘不保存每个 Snapshot 的完整行索引。

## 6. 提交协议

### SyncCommit

```text
1. append SnapshotHeader、Blocks、SnapshotFooter 到 .rpk
2. sync .rpk
3. append 完整 IndexTxn 到 .rpi
4. sync .rpi
5. 原子发布新的内存索引视图
6. 返回成功
```

步骤 2 后崩溃时，调用者可能未收到成功，但该 Snapshot 会在重启后恢复为已提交。这属于“提交结果未知”；调用者应查询 SnapshotID，不得盲目重放非幂等业务。

AsyncCommit 顺序相同但不主动 sync；机器崩溃可以丢失最近提交，进程内仍不得部分可见。

Abort 不写 SnapshotFooter。实现可立即截断到 SnapshotHeader 前，或在下次恢复时截断。

## 7. 打开与恢复

1. 校验两个 File Header 和 UUID。
2. 顺序重放 `.rpi` 至最后一个有效 IndexTxn。
3. 核对该事务对应的 `.rpk` SnapshotFooter。
4. 若 `.rpk` 领先，扫描后续完整 Snapshot 并重建索引：读写模式追加到 `.rpi`，只读模式仅构建内存索引。
5. 若索引领先或尾部损坏，读写模式截断 `.rpi` 至共同提交点；只读模式忽略尾部。
6. 最后有效 Data Footer 后的无 Footer 字节是未提交尾部：读写模式截断，只读模式忽略并报告。
7. 两个有效 Snapshot 之间的损坏属于中间损坏，必须失败，不得跳过。

仅含两个 128 字节 Header 的 Store 合法。

## 8. Snapshot 和 Schema 规则

- FULL：Parent=0，只允许 INSERT，是 Store 级检查点，空 FULL 非法。
- DELTA：Parent 必须是较小的已提交 Snapshot，可含 INSERT/UPDATE/DELETE，可显式允许为空。
- Strict Writer 校验 INSERT 在父视图中不存在，UPDATE/DELETE 在父视图中存在。
- 同一 `(TableID, SchemaVersion)` 的重复 Schema 必须规范字节完全一致。
- 行按自身 SchemaVersion 解码；v1 不自动将历史行升级到最新 Schema。
- v1 允许末尾增加列以及只改表名/列名；改变已有列位置或类型应使用新表或上层迁移。

## 9. Feature Bits

| Bit | 名称 | 说明 |
| ---: | --- | --- |
| 0 | TypedTupleV1 | v1 行编码 |
| 1 | Zstd | 允许 Zstd Block |
| 2 | MetadataBlocks | 通用 Metadata Block |
| 3 | DeltaSnapshots | 允许 DELTA |

Header 创建后不更新，所以 RequiredFeatures 表示该 Store 允许写入的能力集合。未知 Required Bit 必须拒绝打开。

## 10. 强制验证项

实现必须验证 Magic、版本、固定长度、CRC、UUID、offset 溢出和边界；ID/事务序号递增；父链存在且无环；Block 上下文一致；解压长度和 CRC；Directory 大小和 Record 不重叠；Directory 与 Record 字段一致；TypedTuple 与 Schema 一致且无尾随字节；Index Entry 与数据内容一致。

仓库必须固定保存空 Store、None FULL、覆盖全类型的 Zstd FULL、FULL+DELTA、空 DELTA、超大单行、数据领先索引及各类损坏文件的 golden files。v1.0 发布后不得重写这些样本。
