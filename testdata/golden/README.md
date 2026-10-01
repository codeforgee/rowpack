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

## 锁定摘要（SHA-256）

| 文件 | SHA-256 |
| --- | --- |
| `empty-store.rpk` | `359fb844c16095678cac65efd8c93b0e31d94639ae178cfc336b3def54f5c401` |
| `rows-payload-all-types.bin` | `5f9b9022e858016dc2490b347de8edc58cba2a843797912e4fb210d01f9331a3` |
| `full-delta-store.rpk` | `886a7e882a963c47cf2662ba6b179a52a11192ff0fd0df7542392686d57e6fd7` |
| `encrypted-store.rpk` | `0aca420805e0fe1d6af8b489b35ad8fb87a8e43c4db84f973e4ab8e3c395dc6c` |

摘要对应冻结的 v1 IndexTxn 格式（排序 Row Index Page + Fence Directory）。修改任一摘要即视为
有意的磁盘格式变更，必须经过格式审查并按版本策略建立新的 golden 样本族。

最近一次变更：DateTime 改为秒+纳秒 12B(TZ 16B 含偏移)、RowsBlockHeader 从 24B 扩展到 32B（新增 BoundsOffset/BoundsLen，承载声明主键的
每块 PK 边界段：`[u32 len][first][u32 len][last][u32 CRC32C]`，位于最后一个存储页之后），全部
含 Rows 块的样本摘要随之更新；`empty-store.rpk` 无 Rows 块，字节与摘要保持不变。生成器见
`golden_store_test.go` 的 `goldenMetaFull`/`goldenMetaDelta` 与 `encryption_test.go` 的
`TestGoldenEncryptedStoreGenerate`。

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
