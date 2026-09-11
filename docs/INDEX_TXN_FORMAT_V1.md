# RowPack IndexTxn 格式（v1）

> 状态：已实现并冻结（v1 为唯一格式线）
> 适用范围：单文件 `<base>.rpk` 中每个 SnapshotTxn 的内嵌 IndexTxn
> 相关：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md) §6 · [ENCRYPTION_V1.md](ENCRYPTION_V1.md)

## 1. 目标与范围

IndexTxn 是每个已提交 Snapshot 的派生导航结构，提供
`(SnapshotID, TableID, RowID) → (BlockID, ItemOrdinal)` 以及快照、元数据、块的位置索引。目标：
降低索引落盘体积（快照/元数据/块条目分 chunk 压缩，行条目用排序页压缩）；保留完整重放、按
chunk/页定位和损坏隔离能力；加密场景下不降低完整性与 nonce 安全性；单条索引记录不要求磁盘级
随机读取（Get 依赖内存 `index.View`）。不在范围内：改变内存中的 `index.View` 表示、改变
Block/Rows Page/TypedTuple 编码。

## 2. 整体布局

IndexTxn 位于对应 Snapshot 的 Blocks 之后、SnapshotFooter 之前：

```text
[IndexTxnHeader 80B]
[SnapshotChunk]              // chunk seq 0，未压缩，定长 72B（加密 +16B tag）
[MetadataChunk × A]          // chunk seq 1..A，定长条目拼接
[BlockChunk × B]             // chunk seq A+1..A+B，定长条目拼接
[ChunkDirectory]             // 明文 (A+B+1) × 32B
[RowIndexPage × N]           // 每页独立压缩（+加密 tag）
[RowIndexFenceEntry × N]     // 明文 52B，正文 CRC 认证
[IndexTxnFooter 80B]
```

- 行条目**不**走 chunk，而是排序 Row Index Page + Fence Directory（§5）；
  `IndexChunkHeader.EntryKind` 仍保留 `Row(=4)` 枚举值，写路径不再产生 Row chunk；
- `ChunkDirectory` 明文（加密 store 亦然），使读取器无需密钥即可定位 chunk，只暴露 chunk 尺寸与
  条目数；
- `IndexTxnHeader.BodyBytes` 统一为 **stored 语义**：Header 与 Footer 之间的落盘字节数（chunks
  + directory + pages + fence，加密时含每 chunk/page 的 tag），plain 与 encrypted store 一致，
  扫描器据此跳过整条 txn，无需理解 chunk 布局；
- IndexTxnFooter 不承担最终提交语义，提交权威是 SnapshotFooter
  （[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md) §7）。

## 3. IndexTxnHeader（80B）

```text
offset  size  field
0       8     MagicIndexTxnHdr ("RPITXNBH")
8       4     size = 80
12      4     RowIndexPageCount        // 行索引页数 N，零表示无行条目
16      8     TxnSequence
24      8     SnapshotID
32      8     DataSnapshotStart
40      8     DataSnapshotEnd
48      4     MetadataEntryCount
52      4     BlockEntryCount
56      8     RowEntryCount
64      8     BodyBytes                // Header/Footer 之间的 stored 字节数
72      4     HeaderCRC32C
76      4     KeyEpoch                 // 加密 store；plain 为 0
```

`RowIndexPageCount` 必须 ≤ `RowEntryCount`（每页 ≥1 条）。`KeyEpoch` 与 `RowIndexPageCount` 分处
12..16 与 76..80 两个 reserved 字，互不冲突。

## 4. Chunk（快照 / 元数据 / 块条目）

### 4.1 IndexChunkHeader（64B）

```text
offset  size  field
0       8     MagicIndexChunkHdr ("RPICHNK1")
8       2     HeaderSize = 64
10      1     EntryKind            // 1=Snapshot 2=Metadata 3=Block 4=Row(保留)
11      1     Compression          // 0=None 1=Zstd
12      1     Encryption           // 0=None 1=AES-256-GCM
13      1     Flags (0)
14      2     reserved (0)
16      4     ChunkSequence        // txn 内自 0 递增
20      4     EntryCount
24      4     FirstEntryOrdinal    // 该 kind 流内的起始序号
28      4     RawBytes             // 压缩前字节数
32      4     StoredBytes          // 最终落盘 payload（压缩后，加密时含 tag）
36      4     KeyEpoch
40      4     PayloadCRC32C        // stored payload 诊断 CRC
44      4     HeaderCRC32C
48      16    reserved (0)
```

`IndexChunkHeader.CheckLimits` 在解压前强制上限（防整数溢出与压缩炸弹）：
`EntryCount ∈ [1, 1<<20]`、`RawBytes ∈ [1, 16<<20]`、`StoredBytes ∈ [1, 16<<20]`；加密 chunk 的
`StoredBytes ≥ tag`；`None` 压缩要求 `RawBytes == StoredBytes - tag`。

### 4.2 分 chunk 规则

- 快照条目单独成 chunk（seq 0）；元数据和块条目各自按 kind 流式切 chunk；
- 切分阈值：每 chunk ≤ `IndexChunkTargetEntries`（4096）条且 ≤
  `IndexChunkTargetRawBytes`（256 KiB）；
- 快照 chunk 恒不压缩：其 stored 大小固定（72B / 88B 加密），使正文总长可在一次构建中解析
  （快照条目的 `DataEnd` 依赖正文长度）。

### 4.3 条目编码

- 快照 chunk：1 条定长 72B `SnapshotIndexEntry`（§7）；
- 元数据 chunk：`MetadataIndexEntry`（48B/条）连续拼接；
- 块 chunk：`BlockIndexEntry`（56B/条）连续拼接；
- 每个 chunk 独立压缩、独立认证、独立可解码，不依赖前一个 chunk 的解码状态；
- 定长条目内已含各自 CRC，chunk 级 `RawBytes`/`EntryCount` 再做一次边界校验。

### 4.4 ChunkDirectory

明文，每 chunk 一条 32B `IndexChunkDirEntry`：

```text
offset  size  field
0       4     ChunkSequence
4       4     EntryCount
8       4     FirstEntryOrdinal
12      4     RawBytes
16      4     StoredBytes        // 仅 payload；chunk header 另加固定 64B
20      1     EntryKind
21      3     reserved
24      8     RegionOffset       // chunk header 相对 IndexTxn body 起点的偏移
```

## 5. Row Index Page + Fence Directory

行索引是「一组独立压缩的排序页 + 明文 Fence 目录」。Fence 让读取器按
`(SnapshotID, TableID, RowID)` 二分定位目标页，再 OPEN + 解压该页。

### 5.1 RowIndexPageHeader（64B）

```text
offset  size  field
0       8     MagicIndexPage ("RPKIDXPG")
8       1     IndexPageVersion = 1
9       3     reserved (0)
12      4     EntryCount
16      4     TableRunBytes
20      4     RowIDBytes
24      4     BlockRunBytes
28      4     OrdinalBytes
32      4     ChangeBitsBytes
36      8     FirstRowID
44      8     MinRowID
52      8     MaxRowID
60      4     CRC32C           // 覆盖流区
```

页几何在触碰流之前先校验：`64 + Σ流长 == RawBytes`，且
`ChangeBitsBytes == ceil(EntryCount/4)`。页承载 5 条流：

| 流 | 编码 |
| --- | --- |
| TableID | run-length |
| RowID | 非负 uvarint delta |
| BlockID | run-length |
| ItemOrdinal | zigzag delta |
| ChangeType | 2 bit |

- 每页 `(TableID, RowID)` 升序，**按表切页**（一个页绝不跨表 run；末页可少、表边界可产生较小
  页）；这使页内 `TableID` 唯一、`MinRowID`/`MaxRowID` 属于该表，Fence 成为 `(TableID, RowID)`
  的单调二叉索引——Lazy 二分定位正确的前提（跨表页会让全局 `MinRowID` 随页非单调）；
- 每页条目上限 4096（`indexPageEntryCount`）；
- Page CRC 覆盖流区。

### 5.2 RowIndexFenceEntry（52B，明文）

```text
offset  size  field
0       8     SnapshotID
8       4     TableID
12      8     MinRowID
20      8     MaxRowID
28      8     StoredOffset     // stored page 相对 txn body 起点
36      4     StoredSize       // 压缩 + tag
40      4     RawSize
44      4     EntryCount
48      4     PageCRC32C
```

Fence 按 `StoredOffset` 递增排列，由正文 CRC 认证；`RowIndexPageCount == 0` 表示快照无行条目
（pages == fences == 0）。

## 6. 压缩与加密

### 6.1 顺序

```text
条目编码 → Zstd 压缩 → AES-256-GCM Seal → 写入
```

快照 chunk 不压缩（长度必须内容无关）。读取顺序固定：认证解密 → 长度检查 → Zstd 解压 → 条目
数量检查 → 条目解析。

### 6.2 未加密 Store

```text
条目编码 → Zstd(raw) → [ChunkHeader][compressed payload]
```

`StoredBytes` 是压缩后长度，`RawBytes` 是压缩前长度。

### 6.3 加密 Store

每个 chunk / 每个 Row Index Page 独立认证：一个单元的认证失败不会被误认为另一个单元的有效数据。
加密只发生在 body；Header/Footer 保持明文（扫描与 Footer 校验不依赖密钥）。

### 6.4 Nonce

每个 chunk/page 必须拥有全局唯一 nonce，由 HMAC-SHA256 从独立子密钥派生（`NonceIndex` 的 96-bit
空间已满，放不下 `ChunkSequence`）：

```text
chunkNonceKey = HMAC-SHA256(dataKey, "RowPack index chunk nonce key v1")  // 每 Cipher 派生一次
Nonce(unit)   = Trunc12(HMAC-SHA256(chunkNonceKey, BE(TxnSequence) || BE(ChunkSequence)))
```

Row Index Page 复用同一 index 域，其 `ChunkSequence` 接着 chunk 序号继续编号，与 chunk 两两不同。
已否决：截断 TxnSequence（超长生命周期 store 有碰撞风险）、nonce 低位 XOR ChunkSequence
（`1⊕2 == 3⊕0` 会复用 nonce）。禁止：同密钥下复用 nonce；Block nonce 与 Index nonce 混用；只绑
`ChunkSequence` 不绑 `TxnSequence`；重写/重试复用旧事务 nonce。子密钥按 Cipher 惰性缓存，域分离
与推导细节见 [ENCRYPTION_V1.md](ENCRYPTION_V1.md) §5。

### 6.5 AAD

AAD 至少绑定 `StoreUUID · SnapshotID · TxnSequence · ChunkSequence · EntryKind ·
FirstEntryOrdinal · RawBytes · StoredBytes · KeyEpoch`。**刻意不绑定文件偏移**：StoredBytes 依赖
压缩结果，而 Footer 的 TxnStart/TxnEndOffset 又依赖全部 stored 长度之和，绑定会形成「先有偏移
才能加密、先加密才知道偏移」的循环；偏移防挪用由 Footer 对整个落盘区域的 CRC 与精确字节范围
承担。完整 AAD 字段与长度见 [ENCRYPTION_V1.md](ENCRYPTION_V1.md) §6。

## 7. 关键条目结构

### SnapshotIndexEntry（72B）

```text
SnapshotID(8) · ParentSnapshotID(8) · SnapshotType(1) · BlockCount(4)
· RowRecordCount(8) · DataStart(8) · DataEnd(8) · CreatedUnixNano(8)
· DataFooterCRC32C(4) · EntryCRC32C(4) · reserved(8)
```

### MetadataIndexEntry（48B）

```text
SnapshotID(8) · ObjectID(8) · Revision(4) · RecordType(4) · BlockID(8)
· ItemOrdinal(4) · Operation(1) · Flags(1, bit0=Critical) · EntryCRC32C(4) · reserved(4)
```

### BlockIndexEntry（56B）

```text
BlockID(8) · SnapshotID(8) · TableID(4) · BlockKind(1) · Compression(1)
· DataOffset(8) · RawSize(4) · StoredSize(4) · ItemCount(4) · RawCRC32C(4)
· EntryCRC32C(4) · reserved(4)
```

Block 条目引用同一文件中更早的 Block offset。批量 planner 的 MinRowID/MaxRowIDExclusive 由内存
索引按 RowIndexEntry 集合派生，不在磁盘块头存 envelope。

## 8. IndexTxnFooter（80B）

```text
offset  size  field
0       8     MagicIndexTxnFtr ("RPITXNEF")
8       4     size = 80
12      4     reserved (0)
16      8     TxnSequence
24      8     SnapshotID
32      8     TxnStartOffset
40      8     TxnEndOffset
48      8     DataSnapshotEnd
56      4     BodyCRC32C
60      4     DataFooterCRC32C
64      4     FooterCRC32C
68      12    reserved (0)
```

Footer 交叉校验正文与对应数据 Footer，因此 IndexTxn 只有在 footer、entries 与数据 footer 三者
一致时才有效。`BodyCRC32C` 一律对落盘字节计算（加密 store 即密文），使撕裂/位腐的密文在无密钥
路径即可检出。

## 9. 读取模型

### 9.1 完整重放（Eager，默认）

```text
读取 IndexTxnHeader
→ 读取/校验 ChunkDirectory
→ 逐 chunk ReadAt → 认证解密 → 解压 → 定长条目解析 → Apply
→ 逐 Row Index Page 流式解码直喂紧凑 SoA shard（Eager）
```

开放时把整行索引解码为紧凑 SoA shard；Get 使用内存索引，不每次从磁盘读 RowIndexEntry。

### 9.2 按页定位（Lazy）

有 Fence 后可二分定位页：

```text
(TableID, RowID) → Fence 二分 → StoredOffset/StoredSize
→ ReadAt 该页 → 认证解密 → 解压 → 解码
```

页是实际的随机访问和缓存边界，不是单条记录级随机访问。

### 9.3 损坏与重建

- 数据 Block 损坏仍是硬错误；
- IndexTxn 损坏（Footer/Blocks 有效）时按 [BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md) §10.2 从
  当前 Snapshot 的 Block 重建内存索引；
- 不原地覆盖损坏字节，必要时通过 Rewrite 生成新文件；
- 认证失败触发重建，而不是跳过损坏 chunk/page 后继续使用不完整索引。

## 10. 尺寸与限制

结构尺寸（8 字节对齐，字段布局见对应小节）：IndexTxnHeader 80、IndexTxnFooter 80、
IndexChunkHeader 64、IndexChunkDirEntry 32、SnapshotIndexEntry 72、MetadataIndexEntry 48、
BlockIndexEntry 56、RowIndexPageHeader 64、RowIndexFenceEntry 52。每 chunk 上限 4096 条 /
256 KiB raw（硬解析上限见 §4.1）；每 Row Index Page 上限 4096 条并按表切页。Directory 每 Entry
32B，超大事务场景需预留 directory 自身分 chunk 的演进空间。
