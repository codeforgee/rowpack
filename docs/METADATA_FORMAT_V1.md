# RowPack 元数据格式 v1

> 状态：设计基线
> 配套格式：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md)

## 1. 目标

元数据 TLV 只服务一个目的：持久化**引擎自身需要的、行解码所依赖的**信息。它当前承载两类记录：

- **Canonical Schema 契约（RecordType 1/2）**：`Tx.DefineTable` 写入，描述行负载的编码/解码
  方式（列顺序、逻辑类型、可空性、Decimal 精度）；
- **表身份（Table 记录的一个字段）**：ns 是 Table 记录上的一个字段，不需要单独记录类型，也没有
  需要跨快照共享的 ns 对象。

**源数据库的设计元信息不走 TLV。** 列定义、约束、索引、视图、触发器、注释和厂商扩展等，由上层
以**普通行数据**存放在调用方自选 ns 的目录表里（见 [§9](#9-源库元信息的承载方式)）。理由：这些
属性跨源库方言无法穷举，而 TLV 字段编号一经发布即冻结，把它们做成 TLV 字段等于把「枚举不完的
方言属性」写进不可变契约。引擎不内建 CoreMetadata 或任何数据库对象模型，也不按源库方言建表或
解释约束、索引、视图等语义。

当前版本**没有**通用元数据读写 API，也没有计划新增：本文档描述的是持久化格式能力，不代表
`PutMetadata`、`Metadata` 或 `ListMetadata` 已存在；上层元数据用 `DefineTable` +
`Insert/Update/Delete` 表达即可。

TLV 机制位于 `../internal/metadata`：记录信封（Envelope）、字段 TLV、元数据块载荷（头部 + 目录）
都按本文档布局。未知非 Critical 记录/字段无损保留，未知 Critical 内容拒绝打开，扩展新记录类型不需要改
外层 Block 格式。元数据和行属于同一个 Snapshot 并原子提交：

```text
Snapshot
├── Metadata Block
│ ├── Header
│ └── Table / Column schema records（DefineTable / DefineTableIn 产生）
└── Rows Block... ← 源库元信息目录表的数据就在这里
```

FULL 保存完整有效元数据；DELTA 只保存 UPSERT/DELETE。解释 Rows Block 所需的 Schema 记录必须先
于该 Block 出现，或已存在于父快照。

## 2. 标识

| 字段 | 类型 | 含义 |
| --- | --- | --- |
| ObjectID | u64 | Store 内稳定对象 ID，非零 |
| ParentObjectID | u64 | 所属对象；根对象为 0 |
| Revision | u32 | 同一对象严格递增，从 1 开始 |
| RecordType | u32 | 元数据种类 |
| Namespace | string | 记录命名空间（信封字段，非表 ns） |
| ExternalKey | string | 关联键，可为空 |

ObjectID 是 RowPack 身份，不等同于源数据库 OID。Table 对象保持 `ObjectID == TableID`（uint32
空间内），其他对象使用完整 u64 空间（分配自 `TableSpaceEnd = 1<<32`）。引擎写入的记录使用命名
空间 `rowpack.meta.v1`。引擎认识的 RecordType 只有：

| ID | 名称 | 说明 |
| ---: | --- | --- |
| 1 | Table | DefineTable 写的表 Schema 记录，含 ns |
| 2 | Column | DefineTable 写的列 Schema 记录 |

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
u32 RecordLength // 包含整个 Record
u32 RecordType
u64 ObjectID
u64 ParentObjectID
u32 Revision
u32 Flags // bit 0=Critical
u32 NSLength
u32 ExternalKeyLength
u32 FieldCount
u32 FieldsLength
bytes NSUTF8
bytes ExternalKeyUTF8
Field × FieldCount
u32 RecordCRC32C
```

未知 NS 或 RecordType：Critical=0 时跳过但保留原始字节；Critical=1 时拒绝打开。再次写出未知
记录时必须无损保留。

## 5. Field TLV

```text
u16 FieldID
u8 WireType
u8 Flags // bit 0=Critical, bit 1=Repeated
u32 ValueLength
bytes Value
```

引擎**实现**的 WireType 只有两种：

| WireType | ID | Value |
| --- | ---: | --- |
| Sint | 3 | 1/2/4/8 byte 二补码 Little Endian |
| String | 4 | UTF-8 |

编号 1、2、5–10 是 v1 的**保留值**，在 `../internal/fileformat/constants.go` 中已命名
（Bool/Uint/Bytes/ObjectRef/StringList/ObjectRefList/Expression/FieldSet），但引擎不解释其值
编码：含保留 WireType 的字段按未知字段规则处理——非 Critical 保留原始字节并无损透传，Critical
拒绝。

> **保留编号不可回收。** 编号冻结与是否实现无关：一旦某份已写出的文件里含 WireType 5 的非
> Critical 字段（原始字节被无损保留），后续版本把 5 重新分配给别的语义就会把旧字节解释成错的
> 东西。因此这些编号永久保留，永远不得重编号、不得重新分配。
>
> **当前没有实现它们的计划。** 在「源库元信息用普通表承载」的决策下（§9），这八个 WireType
> 没有任何在途消费者；保留纯粹是为前向兼容与前向扩展留余地，不代表路线图。`WireBytes(5)` 尤其
> 容易误认：它属于**元数据字段**编码，与行负载的 `TypeBytes`（TypedTuple 值类型，目录表正常
> 使用）完全无关。

未知 Field：Critical=0 时跳过并保留；Critical=1 时拒绝。Field 按 ID 升序规范编码；只有
Repeated=1 可重复，并保持原顺序。

## 6. 引擎 Schema 记录

`Tx.DefineTable(name, columns)` / `Tx.DefineTableIn(ns, name, columns)` 把 Schema 契约写成两类
记录（字段 ID 与 `../internal/metadata/corefields.go` 一致）：Table 记录（RecordType=1，
ObjectID = TableID，Revision = Schema Version，ExternalKey = 表名，`NS` 字段记 ns，默认 ns
省略）；Column 记录（RecordType=2，ParentObjectID = Table ObjectID，ObjectID 由表内分配器分配，
自 `TableSpaceEnd` 起，列按 ColumnID 排序）。

### 6.1 Table（RecordType=1）

| ID | 字段 | WireType | 说明 |
| ---: | --- | --- | --- |
| 1 | TableName(String) | String | 裸表名 |
| 2 | NS(String) | String | 默认 ns（`user`）时省略 |

ID 2 是表的 ns，与 `TableName` 一起构成地址（`ns.name`）。默认 ns **省略该字段**，所以既有
store 的 Table 记录字节完全不变；读取侧缺字段即视为 `user`。ns 是表身份的一半且不可变，因此
不需要单独的记录类型，也没有需要跨快照共享的 ns 对象。早期设计曾预留 `TotalRows`(2) /
`Schema`(3) 两个编号，但它们**从未被任何一份文件写出**，而 FieldID 冻结的目的只是保护已写出的
字节，所以已回收：ID 2 现在就是 ns（§8）。若未来确实需要表级行数，用 `Stats` 或上层目录表的列
表达；源库的库/模式名属于上层元数据（§9）。

### 6.2 Column（RecordType=2）

ColumnID 领先作为记录的第一个字段，其余按 DefineTable 语义顺序：

| ID | 字段 | WireType |
| ---: | --- | --- |
| 1 | ColumnID(Sint64) | Sint |
| 2 | ColumnName(String) | String |
| 3 | ColumnType(String) | String |
| 4 | Nullable(String) | String |
| 5 | DataScale(Sint64) | Sint |

引擎只识别 Column 记录字段 3（`ColumnType`，即 `metadata.ColColumnType`）中的规范类型字符串
（`bool`、`int8`…`uint64`、`string`、`bytes`、`date`、`time`、`datetime`、`decimal`，即
`DefineTable` 自产自销的值）；遇到无法解释的类型字符串时，该表的记录按普通数据跳过
SchemaIndex，绝不因此导致 Open 失败。`ColumnType` 与 `Nullable` 字段按原文保存，不做规范化。读取侧
按父链解析 Table/Column 记录，按 ColumnID 排序后派生出用于 TypedTuple 的行解码 Schema，不再
保存独立 Schema 记录。

## 7. 索引

每条 MetadataIndexEntry 固定 48 字节：

```text
u64 SnapshotID
u64 ObjectID
u32 Revision
u32 RecordType
u64 BlockID
u32 ItemOrdinal
u8 Operation
u8 Flags
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

- 元数据字符串受 MaxValueBytes 限制。
- Object 引用必须验证存在性和允许的父子关系（读取侧对悬空引用退化为默认值，由 `Verify` 报为
  `ErrCorruptIndex`）。
- 未知非 Critical 内容必须无损透传；未知 Critical 内容必须拒绝，不能猜测语义。
- **编号冻结的目的是保护已写出的字节**：**FieldID** 在同一记录类型内从 1 密集分配，一旦被任何
  一份文件写过就永久冻结，**从未写出的预留编号可以回收**；**WireType** 是**全局共享**的编号
  空间（对所有记录类型、所有未来字段都一样），重新分配影响面大且回收无收益，因此 1、2、5–10
  **一律保留**，即使当前无实现也无计划。
- 扩展范围已收窄：本格式只承载**引擎自有的**记录（Table / Column）。新增记录类型或字段时不修改
  外层 Block 格式、不改变既有字段编号；若会让引擎误解行解码契约，必须 Critical 且分配 Required
  Feature Bit。源库属性不在扩展范围内（§9）。

## 9. 源库元信息的承载方式

**决策记录（decision record）：源库的表/视图/字段列表等设计元信息以普通行数据存放在调用方自选
ns 的目录表里，不做成 TLV 记录类型。** 理由：

1. **属性量不可穷举。** MySQL 的 `charset`/`collation`/`unsigned`/`generated`/`srid`，Oracle
   的 `char_used`/`virtual_column`/`identity`，MSSQL 的
   `is_sparse`/`generated_always_type`，方言与版本都在增长；而 TLV 字段编号一经发布即冻结
   （§8），把它当方言属性的容器等于把永远在变的集合写进不可变契约。
2. **普通表的列是「数据」，TLV 字段是「契约」。** `DefineTable` 的列决定行解码方式，源库属性
   只是行的内容；放在普通表里，属性增删改一律是普通 `INSERT/UPDATE/DELETE`，不触碰
   `DefineTable`，因此永远不会触发 `ErrSchemaConflict`。
3. **不用新 API。** 普通表不需要 `PutMetadata`/`ListMetadata`，也不需要实现任何保留 WireType
   （§5）。

目录表放调用方自己的 ns（`TablesIn(ctx, snap, rowpack.NSUser)` 一次拿到源库表）、必须预留变长
`TypeBytes` 逃生舱、属性存原文不派生建列等配套约定属上层纪律，引擎不做约束；完整用户端指南见
[SOURCE_CATALOG_GUIDE_V1.md](SOURCE_CATALOG_GUIDE_V1.md)（§1 R1/R2、§2、§6）。

## 10. 写入示例

`DefineTable` 写入 `users` 表（TableID=2，Version=1，列 id uint64 / name string，均非空）：

```text
Table ObjectID=2 Revision=1 Namespace=rowpack.meta.v1 ExternalKey="users"
 TableName="users"
```

同一张表定义在非默认 ns（举例 `catalog`）时，Table 记录多一个字段：

```text
Table ObjectID=2 Revision=1 Namespace=rowpack.meta.v1 ExternalKey="_src_columns"
 TableName="_src_columns", NS="catalog"
```

列记录（默认 ns）：

```text
Column ObjectID=4294967296 Parent=2 Revision=1 Namespace=rowpack.meta.v1
 ColumnID=1, ColumnName="id", ColumnType="uint64", Nullable="NO", DataScale=0
Column ObjectID=4294967297 Parent=2 Revision=1 Namespace=rowpack.meta.v1
 ColumnID=2, ColumnName="name", ColumnType="string", Nullable="NO", DataScale=0
```

这些记录组成 Metadata Block；列按 ColumnID 排序派生成行解码 Schema。若某表增加一列：保留 Table
ObjectID，增加 Table Revision，新增 Column ObjectID，为该表分配新的派生 SchemaVersion；历史行
继续按旧 Schema 解码，新行使用新版本。
