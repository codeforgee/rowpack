# IndexTxn 分 Chunk 压缩与加密设计

> 状态：V2 已实现并冻结；golden 基线见 `testdata/golden/README.md`
> 适用范围：V2 单文件格式的内嵌 IndexTxn
> 目标：在保持索引重放、损坏定位和未来惰性加载能力的前提下，降低 IndexTxn 的落盘大小

## 1. 背景与目标

V2 将原本独立的 `.rpi` IndexTxn 嵌入 `.rpk`。当前 IndexTxn 的条目为定长编码，尤其是
`RowIndexEntry`，每行占 40 字节。索引中存在大量可压缩的重复信息：同一事务内的
SnapshotID 相同，同一 Block 内的 BlockID 相同，RowID 和 ItemOrdinal 通常递增，Block
offset 也具有局部顺序性。

本方案将 IndexTxn Body 切分为多个独立 Chunk，每个 Chunk 单独执行：

```text
Index entries → 差分/局部编码 → Zstd 压缩 → AES-256-GCM 加密
```

读取时执行逆过程：

```text
读取 Chunk → AES-GCM 解密/认证 → Zstd 解压 → 解析条目
```

主要目标：

1. 降低索引磁盘占用；
2. 保留按 Chunk 顺序重放的简单路径；
3. 为未来按 Chunk 懒加载或分页加载索引预留能力；
4. 加密场景下不降低完整性、域隔离和 nonce 安全性。

不在本方案中解决：

- 直接对单条 RowIndexEntry 做磁盘级随机读取；
- 改变内存中的 `index.View` 表示；
- 改变 Block、Rows Payload 或 TypedTuple 编码。

## 2. 核心语义

### 2.1 Chunk 是索引的压缩和认证边界

一个 Chunk 包含一段连续的 IndexTxn 条目。建议默认每个 Chunk 包含 4096 条 RowIndexEntry，
但 Chunk 大小应由字节上限和条目数上限共同约束，例如：

- 最大原始大小：256 KiB；
- 最大条目数：4096；
- Metadata、Block 和 Snapshot 条目可单独放入各自 Chunk，或作为首个固定 Chunk。

Chunk 不跨越 IndexTxn，也不跨越不同类型的索引条目。每个 Chunk 都能独立完成：

- 长度边界校验；
- AES-GCM 认证；
- Zstd 解压；
- 条目数量和 CRC 校验。

### 2.2 Chunk 不改变提交权威

SnapshotFooter 仍是 SnapshotTxn 的唯一提交权威。Chunk 完整、IndexTxnFooter 完整但
SnapshotFooter 缺失时，整个事务仍然是未提交尾部。

如果 SnapshotFooter 有效但某个 Chunk 无法认证、解压或解析：

- 数据 Block 损坏仍是硬错误；
- IndexTxn 损坏可按 V2 现有规则从当前 Snapshot 的 Block 重建内存索引；
- 不应原地覆盖损坏的 Chunk；
- 必要时通过 Rewrite 或 checkpoint 生成新的事务。

## 3. 推荐布局

### 3.1 IndexTxn 区域

```text
[IndexTxnHeader]
[IndexTxnChunkHeader][compressed/encrypted payload] ...
[IndexTxnChunkHeader][compressed/encrypted payload]
[ChunkDirectory]
[IndexTxnFooter]
```

推荐将 Chunk Directory 放在 IndexTxnFooter 之前。Directory 记录每个 Chunk 的逻辑范围和
物理位置，使读取器可以跳过不需要的 Chunk。IndexTxnFooter 仍位于 SnapshotFooter 之前。

如果第一版只实现完整重放，可以不依赖 Directory 进行读取，按 Chunk Header 顺序扫描；但
Directory 应从第一版开始落盘，以免后续引入分页加载时再次修改格式。

### 3.2 Chunk Header 建议字段

以下字段已经冻结为 64 字节 `IndexChunkHeader`，常量与偏移由
`internal/fileformat/idxchunk_test.go` 保护：

| 字段 | 说明 |
| --- | --- |
| Magic | `RPICHNK1` |
| HeaderSize | 固定 64 字节 |
| ChunkSequence | IndexTxn 内从 0 开始递增 |
| EntryKind | Snapshot / Metadata / Block / Row |
| FirstEntryOrdinal | 该 Chunk 的逻辑起始序号 |
| EntryCount | 解压后的条目数量 |
| RawBytes | 编码后、压缩前的字节数 |
| StoredBytes | 加密后 payload 的字节数 |
| Compression | Zstd |
| Encryption | None 或 AES-256-GCM |
| KeyEpoch | 密钥代次 |
| PayloadCRC32C | 必需的 stored payload 诊断 CRC |
| HeaderCRC32C | Header 自身 CRC |

`RawBytes`、`StoredBytes`、`EntryCount` 必须在解压前受到大小限制，防止整数溢出和压缩炸弹。

### 3.3 Chunk Directory 建议字段

每个 Directory Entry 至少应包含：

```text
ChunkSequence
EntryKind
FirstEntryOrdinal
EntryCount
ChunkFileOffset
StoredBytes
RawBytes
```

Directory 本身可以不压缩，以便读取器在不解密、不解压所有 Chunk 的情况下定位目标 Chunk。
Directory 是否加密取决于是否允许隐藏 RowID 分布和数据规模；如果加密，必须保留足够的
明文扫描信息，或改变打开流程，不能破坏从文件尾定位有效 Footer 的能力。

## 4. 条目编码

### 4.1 先消除上下文重复

Chunk 内不应重复保存可以由外层推导的字段。例如 Row Chunk 已由 IndexTxn、EntryKind
和 Chunk Header 确定 Snapshot 范围，则 Row 条目可以只保存：

```text
RowID delta
BlockID delta 或局部 BlockID
ItemOrdinal delta
ChangeType / 必要 flags
```

BlockID 在同一 Block 的多条 Row 记录中通常保持不变，可使用“当前值 + 变化标记”编码。

### 4.2 差分编码约束

差分编码必须定义重置点，不能让一个 Chunk 依赖前一个 Chunk 的解码状态。推荐：

- 每个 Chunk 的第一条记录使用完整基准值；
- 后续记录使用无符号 Varint delta；
- RowID 逆序、BlockID 跳变或不适合差分时使用显式绝对值标记；
- 每个 Chunk 独立可解码。

这样既能获得压缩收益，也能保证 Chunk 级随机访问和单 Chunk 损坏隔离。

## 5. 压缩与加密流程

### 5.1 未加密 Store

```text
条目编码
  → Zstd(raw index chunk)
  → 写入 Chunk Header + compressed payload
```

Chunk Header 中的 `StoredBytes` 是压缩后的 payload 长度，`RawBytes` 是压缩前长度。

### 5.2 加密 Store

```text
条目编码
  → Zstd(raw index chunk)
  → AES-256-GCM Seal
  → 写入 Chunk Header + ciphertext/tag
```

必须先压缩再加密。加密后的字节近似随机，先加密再压缩通常几乎没有收益。

每个 Chunk 独立认证，因此一个 Chunk 的认证失败不会被误认为另一个 Chunk 的有效数据。
读取顺序固定为：认证解密、长度检查、Zstd 解压、条目数量检查、条目解析。

### 5.3 Nonce 规则

每个 Chunk 必须拥有全局唯一的 nonce。现有 `NonceIndex = (epoch|IndexDomainBit)(4B) ‖ TxnSequence(8B)`
的 96-bit 空间已满，无法容纳 ChunkSequence。经评估，**HMAC 派生是唯一干净的方案**（即初稿
"字段塞不下时用派生函数"分支，现冻结为正式设计）：

```text
chunkNonceKey = HMAC-SHA256(dataKey, "RowPack index chunk nonce key v1")  // 每 Cipher 派生一次
Nonce(chunk)  = Trunc12(HMAC-SHA256(chunkNonceKey, BE(TxnSequence) || BE(ChunkSequence)))
```

已否决的替代方案：

- **截断 TxnSequence 腾出 ChunkSequence**：TxnSequence 是 64-bit 永递增序号，截断在超长生命周期
  store 上引入碰撞风险；
- **nonce 低位 XOR ChunkSequence**：不同 `(txnSeq, chunkSeq)` 对可能得到相同 XOR 结果
  （如 1⊕2 == 3⊕0），造成 nonce 重用，直接违反 GCM 安全性。

HMAC 派生是确定性 nonce，`(txnSeq, chunkSeq)` 的注入性由 HMAC 抗碰撞性保证；每 chunk 一次
HMAC-SHA256（~1µs）相对一次 AES-GCM 可忽略。实现由 `seal.Cipher` 提供，派生子钥按 Cipher
惰性缓存。

禁止：

- 在同一密钥下复用相同 nonce；
- 将 Block nonce 与 Index Chunk nonce 混用；
- 仅使用 ChunkSequence 而不绑定 TxnSequence；
- 因重写或重试而复用旧事务的 nonce。

### 5.4 AAD 建议

AAD 至少绑定：

```text
StoreUUID
SnapshotID
TxnSequence
ChunkSequence
EntryKind
FirstEntryOrdinal
RawBytes
StoredBytes
KeyEpoch
```

这样可以防止 Chunk 在不同 Store、不同事务、不同逻辑位置或不同长度语义下被挪用。
Chunk Header 中参与 AAD 的字段必须在最终写入前冻结，Header CRC 也必须在同一份最终字段上
计算。

**AAD 不绑定文件偏移（有意决策）**：Chunk 的 StoredBytes 依赖压缩结果，而 Footer 的
TxnStart/TxnEndOffset 又依赖全部 Chunk StoredBytes 之和；若 AAD 绑定偏移会形成
"先有偏移才能加密、先加密才知道偏移"的循环。偏移防挪用由 Footer 对整个落盘区域的 CRC
与 Footer 记录的精确字节范围承担。

## 6. 读取模型与“随机访问”边界

### 6.1 完整重放

当前 V2 的打开流程可以扩展为：

```text
读取 IndexTxn Header
  → 读取/校验 Chunk Directory
  → 逐 Chunk ReadAt
  → 解密
  → 解压
  → Apply 到 index.View
```

这不会改变 Get 的主要路径。Get 使用的是打开时构建好的内存索引，而不是每次从磁盘读取
RowIndexEntry。

### 6.2 Chunk 级随机访问

有 Directory 后，可以直接定位某个 Chunk：

```text
逻辑条目序号
  → Directory 查找 Chunk
  → ReadAt 该 Chunk
  → 解密并解压该 Chunk
```

但这仍不是单条记录级随机访问。一个 Chunk 中有 4096 条记录时，读取其中一条通常需要读取
并解压整个 Chunk。若需要更细粒度，只能减小 Chunk，代价是：

- Header、Tag 和 Directory 开销上升；
- Zstd 压缩率下降；
- ReadAt 次数增加；
- Open 重放的固定开销增加。

因此建议把 Chunk 作为实际的随机访问和缓存边界，把单条 RowIndexEntry 随机访问留给未来
专门的页式索引设计。

## 7. 大小收益与代价

预期收益来源：

1. RowID、BlockID、ItemOrdinal 的差分编码；
2. 重复上下文字段移出每条索引记录；
3. Zstd 对定长条目和重复字段的压缩；
4. 大 BlockSize 减少 BlockIndexEntry 数量。

新增开销来源：

1. 每个 Chunk Header；
2. 每个加密 Chunk 一个 AES-GCM Tag，通常 16 字节；
3. Chunk Directory；
4. Chunk 对齐和边界填充。

Chunk 太小会使固定开销和压缩损失抵消收益；Chunk 太大则降低局部读取和损坏隔离能力。

### 7.1 实测数据（本仓库 1M 行基准，zstd L3，含每 chunk 120B 全部固定开销）

| 负载 | 现行定长 40B | 纯 zstd(定长流) | 差分+zstd(提案) |
| --- | --- | --- | --- |
| 顺序插入 1M（chunk=4096） | 40.00 B/row | 2.78 B/row（7.0%） | **0.04 B/row（0.1%）** |
| 顺序插入 1M（chunk=1024） | 40.00 B/row | 2.49 B/row | 0.15 B/row（0.4%） |
| 乱序插入 1M（chunk=4096） | 40.00 B/row | 5.44 B/row | **4.49 B/row（11.2%）** |
| 小 DELTA（20 行/txn，单 chunk） | 40.00 B/row | 10.95 B/row | 7.45 B/row（18.6%） |

结论：

1. **索引是文件的最大压缩目标**：1M 顺序负载 datafile=58.1MB 中索引约 38MB（65%）、
   压缩数据块仅 19.9MB。分 chunk 差分后文件预计 58MB → 20MB（顺序）/ 24.4MB（乱序）。
2. **差分的收益高度依赖聚集度**：顺序负载下差分是决定性的（0.04 vs 2.78，~70x）；
   乱序负载下 RowID delta 方差大，zstd 对定长流已很有效，差分边际收益约 17%。
3. **小事务开销无碍**：120B/chunk 固定开销在 20 行 DELTA 上占压缩后体积比例高，
   但绝对量（149B/txn）可忽略。
4. **CPU 量级**：1M 行差分编码 + zstd 压缩约 0.2s（单线程 L3）；索引 chunk 复用 store
   压缩级别，默认配置（level 0，fastest）下显著更低。Open 侧解压字节数从 ~38MB 降至
   ~1MB（顺序负载），重放更快。
5. **内存收益（次要目标）**：chunk 化 + 流式 Apply 后，commit/open 的索引瞬时内存从
   O(条目总数)（1M 行约 80-100MB）降为 O(单 chunk)（~256KB）加稳态 shards。

Directory 扩展性注记：每 Directory Entry 约 32B；10 亿行 @4096 条/chunk = 约 24.4 万条
→ 约 7.8MB directory。v1 可接受，超大事务场景需在格式注释中预留 directory 自身分 chunk
的演进空间。

建议先以 256 KiB 原始索引数据或 4096 条 Row 条目作为基准，通过真实数据集测量：

- IndexTxn 压缩率；
- 完整 Open 时间；
- Chunk 级 ReadAt 延迟；
- 内存峰值；
- 加密与未加密的大小差异。

## 8. V2 格式冻结建议

V2 尚未发布，因此本方案可以直接作为 V2 IndexTxn 的正式布局，不需要为旧 V2 文件设计
迁移路径，也不需要同时维护两套 IndexTxn 编解码逻辑。

在实现开始前应直接冻结：

- **`BodyBytes` 统一为 stored 语义**：对 plain 与 encrypted store 一致，均表示 Header 与
  Footer 之间的落盘字节数（chunks + directory；加密时含每 chunk tag）。这消除了旧设计中
  "读路径解密后需重戳 BodyBytes"的特判（`PatchIndexTxnHeaderForStorage` 仅保留给读侧兼容，
  新写路径不再需要）；
- Chunk Header 的字节布局和固定大小；
- Chunk Directory 的字节布局和排序规则；
- EntryKind、ChunkSequence、EntryOrdinal 的语义；
- 差分编码和绝对值回退编码；
- `RawBytes`、`StoredBytes`、`EntryCount` 的长度语义；
- 未加密与 AES-GCM 加密 Chunk 的落盘边界；
- IndexTxnFooter 对整个 IndexTxn 区域的 CRC 覆盖范围。

FileHeader 的 `RequiredFeatures` 保持 V2 已冻结的 `0x0F`；Chunk 布局是 V2 基线的一部分，
不通过新增 Feature Bit 与旧 IndexTxn 布局共存。当前布局由结构测试、逐字节 writer 对比和
golden SHA-256 manifest 锁定。

加密 Store 必须保持：

- Index Chunk 的 nonce 域与 Block nonce 域分离；
- Footer 对最终落盘 IndexTxn 字节进行绑定；
- Chunk 认证失败仍触发索引重建，而不是跳过损坏 Chunk 后继续使用不完整索引。

## 9. 推荐实施顺序

> 状态：已按本节顺序实施（第一阶段）。已落地：Chunk Header/Directory 冻结布局、差分编码
> +zstd（plain 与 AES-GCM）、完整重放、边界与压缩炸弹限制、Directory 定位与损坏测试、
> 每 Chunk nonce/AAD、golden 重生成。懒加载（第 8 步）与 View.Apply 流式化（内存收益
> 的后半段）留待后续，基于同一套已冻结格式。

1. 直接冻结 Chunk Header、Directory 和 Feature Bit，作为 V2 正式布局；
2. 先实现未加密 Store 的差分编码 + Zstd；
3. 增加完整重放、边界检查、压缩炸弹限制和 golden 文件；
4. 加入 Chunk Directory 的定位测试；
5. 实现 AES-GCM 的每 Chunk nonce/AAD；
6. 增加 Chunk 级损坏、截断、乱序、跨事务挪用测试；
7. 对比未压缩 V2、分 Chunk V2 的文件大小和 Open 基准；
8. 最后再评估懒加载，但仍基于同一套已冻结的 Chunk 格式实现。

## 10. 结论

“分 Chunk Zstd + 每 Chunk AES-GCM”是比整段压缩更平衡的方案：它保留完整 IndexTxn
重放的简单性，同时提供 Chunk 级定位、缓存和损坏隔离能力。它牺牲的是单条索引记录的
直接磁盘随机访问，但当前 Get 依赖内存 `index.View`，因此不会把每次 Get 变成一次索引解压。

第一阶段建议采用 4096 条 RowIndexEntry 或 256 KiB 原始索引数据作为 Chunk 边界，并以
真实基准决定最终参数。
