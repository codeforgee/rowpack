# RowPack 元数据格式 v1

> 状态：设计基线  
> 配套格式：[BINARY_FORMAT_V2.md](BINARY_FORMAT_V2.md)（原配套 BINARY_FORMAT_V1.md 已随 v1 历史文档移除）

## 1. 目标

元数据格式服务于两层信息：引擎用 `DefineSchema` 写入行解码所需的
Canonical Schema；未来的上层适配器可用同一 TLV 机制保存源数据库的原始设计元信息。
两者不能混同：Canonical Schema 决定 RowPack 行负载的编码/解码，Source Metadata
用于恢复、审计和 Schema 对比。引擎不内建 CoreMetadata 或任何数据库对象模型，
也不按源数据库方言建表或解释约束、索引、视图等语义。

当前版本通用元数据读写 API 尚未公开，因此本文档描述的是持久化格式能力，不代表
`PutMetadata`、`Metadata` 或 `ListMetadata` 已存在。

TLV 机制位于 `../internal/metadata`：记录信封（Envelope）、字段 TLV、元数据块
载荷（头部 + 目录）都按本文档布局。未知非 Critical 记录/字段无损保留，
未知 Critical 内容拒绝打开，扩展新记录类型不需要改外层 Block 格式。

元数据和行属于同一个 Snapshot 并原子提交：

```text
Snapshot
├── Metadata Block
│   ├── Header
│   ├── Table / Column schema records（DefineSchema 产生）
│   └── 其他记录（为未来 Source Metadata 扩展保留）
└── Rows Block...
```

FULL 保存完整有效元数据；DELTA 只保存 UPSERT/DELETE。解释 Rows Block 所需的
Schema 记录必须先于该 Block 出现，或已存在于父快照。

## 2. 标识

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| ObjectID | u64 | Store 内稳定对象 ID，非零 |
| ParentObjectID | u64 | 所属对象；根对象为 0 |
| Revision | u32 | 同一对象严格递增，从 1 开始 |
| RecordType | u32 | 元数据种类 |
| Namespace | string | 记录命名空间 |
| ExternalKey | string | 关联键，可为空 |

ObjectID 是 RowPack 身份，不等同于源数据库 OID。Table 对象保持
`ObjectID == TableID`（uint32 空间内），其他对象使用完整 u64 空间
（分配自 `TableSpaceEnd = 1<<32`）。

引擎写入的记录使用命名空间 `rowpack.meta.v1`。引擎认识的 RecordType 只有：

| ID | 名称 | 说明 |
| ---: | --- | --- |
| 2 | Table | DefineSchema 写的表 Schema 记录 |
| 3 | Column | DefineSchema 写的列 Schema 记录 |

其他 RecordType 按第 4/5 节规则作为普通数据处理。

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

未知 Namespace 或 RecordType：Critical=0 时跳过但保留原始字节；Critical=1 时
拒绝打开。再次写出未知记录时必须无损保留。

## 5. Field TLV

```text
u16 FieldID
u8  WireType
u8  Flags        // bit 0=Critical, bit 1=Repeated
u32 ValueLength
bytes Value
```

引擎识别并写入的 WireType：

| WireType | ID | Value |
| --- | ---: | --- |
| Sint | 3 | 1/2/4/8 byte 二补码 Little Endian |
| String | 4 | UTF-8 |

编号 1、2、5–10 是 v1 格式的保留值：引擎不解释其值编码。含保留
WireType 的字段按未知字段规则处理——非 Critical 保留原始字节并无损透传，
Critical 拒绝。

未知 Field：Critical=0 时跳过并保留；Critical=1 时拒绝。Field 按 ID 升序
规范编码；只有 Repeated=1 可重复，并保持原顺序。

## 6. 引擎 Schema 记录

`DefineSchema(schema)` 把 Schema 契约写成两类记录（字段见下表，字段 ID
与 `../internal/metadata/corefields.go` 一致）：

- **Table 记录（RecordType=2）**：ObjectID = TableID，Revision = Schema
  Version，ExternalKey = 表名。
- **Column 记录（RecordType=3）**：ParentObjectID = Table ObjectID，
  ObjectID 由表内分配器分配（`TableSpaceEnd` 起），列按 ColumnID 排序。

### 6.1 Table（RecordType=2）

| ID | 字段 | WireType |
| ---: | --- | --- |
| 1 | TableName(String) | String |
| 2 | TotalRows(Sint64) | Sint |
| 3 | Schema(String) | String |

### 6.2 Column（RecordType=3）

ColumnID 领先作为记录的第一个字段，其余按 DefineSchema 语义顺序：

| ID | 字段 | WireType |
| ---: | --- | --- |
| 1 | ColumnID(Sint64) | Sint |
| 2 | ColumnName(String) | String |
| 3 | ColumnType(String) | String |
| 4 | Nullable(String) | String |
| 5 | DataScale(Sint64) | Sint |

引擎只识别 `ColumnType`/`DataType` 中的规范类型字符串
（`uint64`、`string`、`decimal`…，即 `DefineSchema` 自产自销的值）；遇到
无法解释的类型字符串时，该表的记录按普通数据跳过 SchemaIndex，绝不因此
导致 Open 失败。`TypeString` 与 `Nullable` 原文保存，不做规范化。

读取侧按父链解析 Table/Column 记录，按 ColumnID 排序后派生出用于
TypedTuple 的行解码 Schema，不再保存独立 Schema 记录。

## 7. 索引

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

索引是快照增量，查询时沿父链解析。

## 8. 安全与兼容

- 元数据字符串、Bytes、FieldSet 受 MaxValueBytes 限制。
- Object 引用必须验证存在性和允许的父子关系。
- 未知非 Critical 内容必须无损透传；未知 Critical 内容必须拒绝，不能猜
  测语义。
- 扩展新记录类型或字段时，不修改外层 Block 格式、不改变既有字段编号；
  若会让引擎误解行解码契约，必须 Critical 且分配 Required Feature Bit。

## 9. 写入示例

`DefineSchema` 写入 `users` 表（TableID=2，Version=1，列 id uint64 /
name string，均为非空）：

```text
Table ObjectID=2 Revision=1  Namespace=rowpack.meta.v1  ExternalKey="users"
  TableName="users"

Column ObjectID=4294967296  Parent=2 Revision=1  Namespace=rowpack.meta.v1
  ColumnID=1, ColumnName="id", ColumnType="uint64", Nullable="NO", DataScale=0

Column ObjectID=4294967297  Parent=2 Revision=1  Namespace=rowpack.meta.v1
  ColumnID=2, ColumnName="name", ColumnType="string", Nullable="NO", DataScale=0
```

这些记录组成 Metadata Block；列按 ColumnID 排序派生成行解码 Schema。若某
表增加一列：保留 Table ObjectID，增加 Table Revision，新增 Column ObjectID，
为该表分配新的派生 SchemaVersion；历史行继续按旧 Schema 解码，新行使用
新版本。
