# Golden files 生成与保护策略

`testdata/golden/` 保存格式兼容性样本，由实现生成、人工审查后锁定，纳入 CI。格式锁定后任何
提交不得重写这些样本；需要修改时视为格式变更，必须同时提升格式版本并创建新的样本族。

## 样本清单

| 文件 | 内容 | 里程碑 |
| --- | --- | --- |
| `empty-store.rpk` | 仅 128 字节 FileHeader 的空 Store（确定性 UUID/时间） | v1 |
| `rows-payload-all-types.bin` | 覆盖全类型值 + NULL 的确定性未压缩 Rows Payload | v1 |
| `full-delta-store.rpk` | FULL + DELTA + 空 DELTA + 超大行（单文件，含内嵌 IndexTxn；Row Index Page + Fence Directory）；含**快照 Meta 块**：FULL 设置、DELTA 覆盖、空 DELTA 沿父链继承 | v1 |
| `encrypted-store.rpk` | 加密 FULL Store（None 压缩 + 固定 key，锁定 Header 加密字段/块 Flags/KeyEpoch/密文布局；IndexTxn 页按 Index 域 chunk-nonce/AAD 密封）；含**整容器密封的快照 Meta 块** | v1 |

## 变更日志

这里不再静态列出样本的 SHA-256：摘要就是文件内容本身的函数，真正的锁定在测试侧
（`TestGoldenManifest`），再抄一份到文档只会多一处必须同步、又不同步也不会报错的地方。

最近一次变更：RowIndexFenceEntry 删掉 StoredOffset（44→36 B）。页在 IndexTxn 正文里连续
排列，偏移由解析侧按「页区起点 + 前序 StoredSize」累加回填——与上一次变更里
RowsPageDirEntry 的做法一致。原先还要逐条校验偏移连续，现在那是构造出来的恒等式。
`full-delta-store.rpk` 4629→4605 B、`encrypted-store.rpk` 2021→2013 B。

上一次变更：RowIndexFenceEntry 删掉 SnapshotID（52→44 B）。一个 Fence Directory 从属于
单个 IndexTxn，快照号已由 IndexTxnHeader 声明，逐条目再存一份只是给伪造者多一个「可以与头
不一致」的位置——旧版解析因此还要专门拒绝不匹配的副本。现在由解析侧注入字段，消费方不变。
`full-delta-store.rpk` 4653→4629 B、`encrypted-store.rpk` 2029→2021 B。

上一次变更：RowsPageDirEntry 由定长 56 B 改为 8 个 uvarint（典型 ~10 B），并删掉
`StoredOffset`（页在容器内连续排列，解析时由前序 StoredSize 累加回填）与 `PageCRC32C`
（页头 CRC32C 已覆盖页流、ParseRowsPage 会校验）。`full-delta-store.rpk` 4793→4653 B、
`encrypted-store.rpk` 2076→2029 B、`rows-payload-all-types.bin` 378→332 B。AAD 字段集合
未变，`internal/seal` 无需改动。

上一次变更：Time/DateTime/DateTimeTZ 改为 varint 编码（秒 zigzag + 纳秒 varint，TZ 再加
偏移 zigzag），整秒时间 12 B→6 B、带时区 16 B→7 B；`rows-payload-all-types.bin` 390→378 B。
代价：这些类型 width 归 0，含它们的 Schema 退出定宽解码内核（实测
BenchmarkPreparedDecodeMixed +12%）。

上一次变更：无任何可空列的 Schema 不再写 null bitmap（原本每行固定
`ceil(ncols/8)` 字节纯属浪费），`full-delta-store.rpk` 4816→4793 B、
`encrypted-store.rpk` 2079→2076 B。宽度由 `Schema.nullBitmapBytes()` 单点决定，编解码两侧共用。

上一次变更：String/Bytes/Decimal 的值长度前缀由定长 u32 改为 uvarint（每值省 3 B，
短值场景收益最大），全类型负载样本 `rows-payload-all-types.bin` 因此 417 B → 390 B。
生成器见 `golden_store_test.go` 的 `goldenMetaFull`/`goldenMetaDelta` 与
`encryption_test.go` 的 `TestGoldenEncryptedStoreGenerate`。

上一次变更：DateTime 改为秒+纳秒 12B(TZ 16B 含偏移)、RowsBlockHeader 从 24B 扩展到 32B（新增 BoundsOffset/BoundsLen，承载声明主键的
每块 PK 边界段：`[u32 len][first][u32 len][last][u32 CRC32C]`，位于最后一个存储页之后）。

损坏样本不静态保存，由测试从健康 Store 动态构造：坏 Magic / 短头 / 未知主版本 →
`TestReadDataHeaderCorruptFiles`（未知主版本要求 `ErrVersionUnsupported`）；Block 头与负载损坏 →
`TestCorruptBlockHeaderMidFile`、`TestCorruptBlockPayloadDamagesRead`；IndexTxn 损坏内存重建 →
`TestCorruptIndexTxnRebuild`；加密中间损坏 → `TestEncryptedMidFileCorruption`。

## 生成命令

```sh
make golden     # 再生 empty-store / rows-payload-all-types / full-delta（生成器在根包 golden_store_test.go）
```

生成后必须人工 diff 审查（`git diff --stat testdata/golden`），确认字节变化只来自有意的格式
变更。`encrypted-store.rpk` 由 `encryption_test.go` 的 `TestGoldenEncryptedStoreGenerate` 在
`-update-golden` 下重新生成（固定 UUID/now/nonce + 静态 key），哈希由
`TestGoldenManifest` 锁定；重写它同样需要走格式审查。

## 校验命令

CI 中 `go test ./...` 的 golden 测试以只读方式校验：任何样本与实现不一致即失败，锁定的
Store 样本必须可打开且内容一致。

## 确定性来源

golden 生成固定三个非确定性源（`rowpack.go` 的测试钩子）：StoreUUID（`testUUIDOverride`）、
CreatedAt（`testNowOverride`）、SnapshotHeader.WriterNonce（`testNonceOverride`）。生产路径
下 WriterNonce 每次写入随机；任何新引入的非确定性字段都必须同样纳入钩子覆盖，否则每次
`make golden` 都会产生无关 diff，破坏「人工 diff 审查只看格式变更」的前提。
