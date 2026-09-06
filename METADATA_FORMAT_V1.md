# RowPack 元数据格式 v1

> 状态：设计基线  
> 配套格式：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md)

## 1. 目标

RowPack 除了行数据，还要保存能够解释、校验和还原这些行的数据库元信息。v1 将一切元数据建模为通用 Metadata Record：引擎把元数据当作**普通存储数据**，通过通用 TLV 记录通道（`PutMetadata`）保存，**不内建任何数据库强类型语义**。

引擎行解码所需的最小 Schema 契约由 `DefineSchema` 提供（写引擎自有的规范类型字符串）；表结构、约束、注释等数据库元信息由上层适配器作为普通记录写入与读取。本文档 §6/§7 的 13 类 RecordType 编号与字段映射是**上层适配器的参考约定**，不是引擎内建能力——引擎按通用 TLV 规则保存/透传这些记录，不识别其语义。Sequence、Trigger、分区、统计信息等使用标准扩展记录。扩展新对象或新字段时，不修改外层 Block 格式。

元数据和行属于同一个 Snapshot 并原子提交：

```text
Snapshot
├── Metadata Block
│   ├── Header
│   ├── Table / Column / VirtualColumn
│   ├── PK / UK / FK / Index / AutoInc
│   └── vendor extensions
└── Rows Block...
```

FULL 保存完整有效元数据；DELTA 只保存 UPSERT/DELETE。修改同一对象时保留 ObjectID、增加 Revision。解释 Rows Block 所需的元数据必须先于该 Block 出现，或已存在于父快照。

## 2. 标识

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| ObjectID | u64 | Store 内稳定对象 ID，非零 |
| ParentObjectID | u64 | 所属对象；根对象为 0 |
| Revision | u32 | 同一对象严格递增，从 1 开始 |
| RecordType | u32 | 元数据种类 |
| Namespace | string | 核心或厂商扩展命名空间 |
| ExternalKey | string | 源数据库 OID/GUID/限定名，可为空 |

ObjectID 是 RowPack 身份，不等同于源数据库 OID。Header 保留 ObjectID=1，其他对象从 2 分配。Table ObjectID 必须可无损转换为 uint32 TableID，便于 Rows Block 引用；其他对象使用完整 u64。

核心命名空间是 `rowpack.meta.v1`。厂商扩展使用反向域名，例如 `com.mysql`、`com.oracle`、`com.microsoft.sqlserver`、`org.postgresql`、`cn.dameng`。

## 3. Metadata Block Payload

`BlockKind=2` 表示 Metadata。解压后：

```text
[MetadataPayloadHeader 32]
[MetadataDirectoryEntry × ItemCount, 32 each]
[MetadataRecord bytes...]
```

MetadataPayloadHeader：

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | Magic | ASCII `RPKMETAP` |
| 8 | 4 | PayloadVersion | 1 |
| 12 | 4 | DirectoryEntrySize | 32 |
| 16 | 4 | ItemCount | 与 BlockHeader 一致 |
| 20 | 4 | DirectoryBytes | `ItemCount * 32` |
| 24 | 8 | RecordsBytes | Record 区长度 |

MetadataDirectoryEntry：

| Offset | Size | 字段 | 说明 |
| ---: | ---: | --- | --- |
| 0 | 8 | ObjectID | 对象 ID |
| 8 | 4 | Revision | 对象版本 |
| 12 | 4 | RecordType | 类型 |
| 16 | 4 | RecordOffset | 相对 Raw Payload 起点 |
| 20 | 4 | RecordLength | 完整记录长度 |
| 24 | 1 | Operation | 1=UPSERT, 2=DELETE |
| 25 | 1 | Flags | bit 0=Critical |
| 26 | 2 | Reserved | 0 |
| 28 | 4 | RecordCRC32C | DELETE 为 0 |

DELETE 不带 Record，offset/length/CRC 为 0。

## 4. MetadataRecord

```text
u32 RecordLength             // 包含整个 Record
u32 RecordType
u64 ObjectID
u64 ParentObjectID
u32 Revision
u32 Flags                    // bit 0=Critical
u32 NamespaceLength
u32 ExternalKeyLength
u32 FieldCount
u32 FieldsLength
bytes NamespaceUTF8
bytes ExternalKeyUTF8
Field × FieldCount
u32 RecordCRC32C
```

未知 Namespace 或 RecordType：Critical=0 时跳过但保留原始字节；Critical=1 时返回 `ErrMetadataUnsupported`。再次写出未知记录时必须无损保留。

## 5. Field TLV

```text
u16 FieldID
u8  WireType
u8  Flags        // bit 0=Critical, bit 1=Repeated
u32 ValueLength
bytes Value
```

| WireType | ID | Value |
| --- | ---: | --- |
| Bool | 1 | 1 byte，0/1 |
| Uint | 2 | 1/2/4/8 byte Little Endian |
| Sint | 3 | 1/2/4/8 byte二补码 |
| String | 4 | UTF-8 |
| Bytes | 5 | 原始字节 |
| ObjectRef | 6 | u64 ObjectID |
| StringList | 7 | u32 count + 重复 u32 length/bytes |
| ObjectRefList | 8 | u32 count + u64 列表 |
| Expression | 9 | Expression FieldSet |
| FieldSet | 10 | u32 count + 嵌套 Fields |

未知 Field：Critical=0 时跳过并保留；Critical=1 时拒绝。Field 按 ID 升序规范编码；只有 Repeated=1 可重复，并保持原顺序。嵌套默认最多 32 层。

## 6. 核心 RecordType（上层适配器参考约定）

> 以下 1–13 的编号与语义是**上层数据库适配器的参考约定**，用于组织从
> `meta.Store` 采集的元数据。引擎本身不内建解释：这些记录通过通用 TLV 保存
> 与透传，未知 RecordType 一律按第 4 节规则处理（Critical=0 跳过并保留原始
> 字节，Critical=1 拒绝）。引擎不因不认识的元数据类型而失败。

这些类型供上层适配器组织现有 `meta.Store` 的列表结构：

| ID | 名称 | 对应 Go 类型 |
| ---: | --- | --- |
| 1 | Header | `meta.Header`，每个 Store 逻辑上一个 |
| 2 | Table | `meta.Table` |
| 3 | Column | `meta.Column` |
| 4 | PrimaryKey | `meta.PrimaryKey`，复合主键每列一条 |
| 5 | Index | `meta.Index` |
| 6 | UniqueKey | `meta.UniqueKey` |
| 7 | ForeignKey | `meta.ForeignKey`，复合外键每列一条 |
| 8 | AutoInc | `meta.AutoInc` |
| 9 | TableComment | `meta.TableComment` |
| 10 | ColComment | `meta.ColComment` |
| 11 | View | `meta.View` |
| 12 | Function | `meta.Function` |
| 13 | VirtualColumn | `meta.Column`，与普通 Column 分流保存 |

这些核心类型使用 Namespace=`rowpack.meta.v1`。14–1023 保留；新增通用对象放入标准扩展类型 1024–65535；数据库厂商类型从 65536 开始。

## 7. 核心字段映射（上层适配器参考约定）

> 承接 §6，本节的 FieldID 与 WireType 供上层适配器按 `meta.Store` 字段组织
> TLV。引擎不校验这些映射。字符串字段原样保存（引擎的 TLV 不做任何
> 规范化），这是纯数据存储的固有行为。
> Go `int` 写盘统一转换为 i64，并在读回目标平台 int 前检查溢出。

### 7.1 Header (RecordType=1)

| ID | 字段 | WireType |
| ---: | --- | --- |
| 1 | DBType | String |
| 2 | UserId | String |
| 3 | DBName | String |
| 4 | IpAddr | String |
| 5 | Port | String |
| 6 | DBVersion | String |
| 7 | Charset | String |
| 8 | Collation | String |
| 9 | Version | String |
| 10 | Properties | FieldSet |
| 11 | Excludes | StringList |
| 12 | Includes | StringList |
| 13 | SnapshotType | Sint(8) |
| 14 | SnapshotToken | String |
| 15 | SnapshotAt | Sint(8) |

DBType 和 Version 使用稳定文本，不能序列化 Go 内存；Properties 编成有序 FieldSet。IpAddr/UserId 属敏感元数据，写入开关见安全章节。

### 7.2 Table (RecordType=2)

1 TableName(String)，2 TotalRows(Sint64)，3 BatchSize(Sint64)，4 Bytes(Sint64)，5 Schema(String)，6 Properties(FieldSet)。

Table 的 ObjectID 由 `(Schema, TableName)` 按当前大小写 Policy 查找或分配。另在 Record Envelope 的 ExternalKey 保存规范限定名。Rows Block 使用分配后的 uint32 TableID；Metadata Record 额外保留原始名称字段。

### 7.3 Column / VirtualColumn (RecordType=3/13)

1 TableName(String)，2 ColumnName(String)，3 DataType(String)，4 DataLength(Sint64)，5 CharLength(Sint64)，6 DataPrecision(Sint64)，7 DataScale(Sint64)，8 Nullable(String)，9 DataDefault(String)，10 ColumnID(Sint64)，11 CharUsed(String)，12 Schema(String)，13 ColumnType(String)。

TypedTuple 的逻辑 Type 由 `(Header.DBType, Header.Version, Column.DataType, Column.ColumnType, precision/scale)` 映射得到，属于派生索引，不替换上述源字段。IsUnsigned 仍由 ColumnType 判断。VirtualColumns 必须使用 RecordType=13，不能与 Columns 合并后丢失分类。

### 7.4 PrimaryKey (RecordType=4)

1 ConsName，2 TableName，3 ColumnName，4 KeySeq(Sint64)，5 Schema，全部 String 除 KeySeq。复合主键每列一条，以 KeySeq 排序；不可只保存 ColumnNames 聚合结果。

### 7.5 Index (RecordType=5)

1 IndexName(String)，2 TableName(String)，3 IdxComment(String)，4 Columns(String)，5 Schema(String)。Columns 保存当前采集结果原文，未来结构化 KeyPart 使用扩展字段，不覆盖原值。

### 7.6 UniqueKey (RecordType=6)

1 TableName(String)，2 ConsName(String)，3 Columns(String)，4 Schema(String)。

### 7.7 ForeignKey (RecordType=7)

1 TableName，2 ConsName，3 ColumnName，4 RefTableName，5 RefColumnName，6 UpdateRule，7 DeleteRule，8 Schema，9 RefSchema，全部为 String。复合外键按采集顺序逐列保存，不强制聚合。

### 7.8 AutoInc (RecordType=8)

1 TableName(String)，2 ColumnName(String)，3 Next(Sint64)，4 ColumnType(String)，5 Schema(String)，6 Seed(Sint64)，7 Increment(Sint64)，8 Kind(String)，9 Generated(String)，10 Cache(Sint64)。

### 7.9 TableComment / ColComment (RecordType=9/10)

- TableComment：1 TableName，2 TableType，3 Comments(String)，4 Schema。
- ColComment：1 TableName，2 ColumnName，3 Comments(String)，4 Schema。

### 7.10 View / Function (RecordType=11/12)

- View：1 ViewName，2 ViewText，3 Schema。
- Function：1 FuncName，2 FuncText，3 Schema。

ViewText/FuncText 是恢复对象的权威原文，不自动执行、不做空白规范化。

### 7.11 对象身份与重复记录

核心列表允许多个记录指向同一表。ObjectID 分配建议：Header 固定使用 1；Table 按限定名稳定分配；Column 按表+列；PrimaryKey/ForeignKey 按约束名+KeySeq；其余按 Schema+表/对象名+记录类型。身份映射必须写入索引，不能依赖每次重新 hash，以兼容大小写 Policy 变化。

同一 FULL 中列表顺序应保留。语义需要排序时（例如 PrimaryKey.KeySeq、Column.ColumnID）由读取 API 返回派生视图，但原始顺序仍可读取。

### 7.12 后续标准扩展

Trigger、Sequence、CheckConstraint、Partitioning、Statistics、UserDefinedType 等当前 `meta.Store` 未包含的对象使用 1024–65535 类型。它们遵循相同 Envelope/TLV 规则，不改变 1–13 的核心字段。

## 8. Expression

核心 v1 中 `DataDefault`、`ViewText` 和 `FuncText` 按源 string 原样保存。未来结构化表达式不能只保存内部 AST；Expression FieldSet 定义：1 Language，2 Text（权威内容），3 NormalizedText，4 BinaryAST，5 ASTFormat。未知 AST 不影响读取 Text，且元数据绝不能被自动执行。

## 9. 索引

每条 MetadataIndexEntry 固定 48 字节：

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

内存索引：

```text
metadata[(SnapshotID,ObjectID)] -> location/tombstone
metadataByType[(SnapshotID,RecordType)] -> sorted ObjectIDs
```

索引是快照增量，查询时沿父链解析。Table+Column 核心记录派生出用于 TypedTuple 的 Schema，不再保存独立 SchemaIndexEntry。

## 10. 厂商扩展

示例 MySQL Column 扩展：Namespace=`com.mysql`、RecordType=65537、ParentObjectID 指向核心 Column；Fields 保存 DisplayWidth、ZeroFill、OnUpdateExpression、EnumValues。

Oracle 可扩展 Compression/PCTFREE/Logging，SQL Server 可扩展 Sparse/RowGUIDCol，PostgreSQL 可扩展 Storage/Compression，DM 可扩展其专有类型属性。扩展不得改变核心 TypedTuple 的解释；若会改变行解码，必须 Critical 且分配 Required Feature Bit。

## 11. 安全与兼容

- 元数据字符串、Bytes、Expression 受 MaxValueBytes 限制。
- Object 引用必须验证存在性和允许的父子关系。
- 未知非 Critical 内容必须无损透传。
- 未知 Critical 内容必须拒绝，不能猜测语义。
- 元数据中的 DDL、Trigger 和表达式只是数据，读取时绝不执行。

## 12. 写入示例

例如保存 MySQL 的 `app.users`：

```text
Header ObjectID=1 Revision=1
  DBType="mysql", DBName="production", DBVersion="8.4.2"
  Charset="utf8mb4", Collation="utf8mb4_0900_ai_ci"

Table ObjectID=2 Revision=1
  TableName="users", Schema="app", TotalRows=100000
  BatchSize=1000, Bytes=8388608

Column ObjectID=1001 Parent=2 Revision=1
  TableName="users", ColumnName="id", DataType="bigint"
  DataLength=8, Nullable="NO", ColumnID=1
  ColumnType="bigint unsigned"

Column ObjectID=1002 Parent=2 Revision=1
  TableName="users", ColumnName="email", DataType="varchar"
  DataLength=1280, CharLength=320, Nullable="NO", ColumnID=2
  CharUsed="C", ColumnType="varchar(320)"

PrimaryKey ObjectID=2001 Parent=2 Revision=1
  ConsName="PRIMARY", TableName="users", ColumnName="id", KeySeq=1

UniqueKey ObjectID=3001 Parent=2 Revision=1
  TableName="users", ConsName="uk_users_email", Columns="email"
```

这些 Record 先组成 Metadata Block。Table ObjectID=2 转换为 Rows Block 的 TableID=2；Column 按 ColumnID 排序并通过数据库方言适配器映射 TypedTuple 类型。

若之后增加 `status` 列：保留 Table ObjectID，增加 Table Revision，新增 Column ObjectID，并为该表分配新的派生 SchemaVersion。历史行继续按旧派生 Schema 解码，新行使用新版本。
