# Golden files 生成与保护策略

`testdata/golden/` 保存格式兼容性样本，由实现生成、人工审查后锁定，纳入 CI。
格式锁定后任何提交不得重写这些样本；需要修改时视为格式变更，必须同时提升
格式版本并创建新的样本族。

## 样本清单

| 文件 | 内容 | 里程碑 |
| --- | --- | --- |
| `empty-store.rpk` | 仅 128 字节 FileHeader 的空 Store（确定性 UUID/时间） | v1 |
| `rows-payload-all-types.bin` | 覆盖全类型值 + NULL 的确定性未压缩 Rows Payload | v1 |
| `full-delta-store.rpk` | FULL + DELTA + 空 DELTA + 超大行（单文件，含内嵌 IndexTxn；Row Index Page + Fence Directory） | v1 |
| `encrypted-store.rpk` | 加密 FULL Store（None 压缩 + 固定 key，锁定 Header 加密字段/块 Flags/KeyEpoch/密文布局；IndexTxn 页按 Index 域 chunk-nonce/AAD 密封） | v1 |

## 锁定摘要（SHA-256）

| 文件 | SHA-256 |
| --- | --- |
| `empty-store.rpk` | `359fb844c16095678cac65efd8c93b0e31d94639ae178cfc336b3def54f5c401` |
| `rows-payload-all-types.bin` | `ae6f94f72c1b08f8c0a6727c97cb57cfad18b6f0ffc732a625db23be907b8769` |
| `full-delta-store.rpk` | `c32072a7d9801a983b2742616c9fd3d50b621d4b08512c18809e5cb7652a4a9e` |
| `encrypted-store.rpk` | `afd3c1b56b8a10b3913021802c16c26baf7d7c581c0f84db324b48368297fae3` |

这些摘要描述当前冻结的 v1 IndexTxn 格式（排序 Row Index Page + Fence
Directory）。修改任一摘要均视为有意的磁盘格式变更，必须经过格式审查并按版本策略建立新的
golden 样本族。

损坏样本由 `TestM10CorruptSamples` 从健康 Store 动态构建（坏 Magic、未知主版本、
负载损坏、IndexTxn 损坏），不静态保存。

## 生成命令

```sh
make golden     # 一条命令再生 empty-store / rows-payload-all-types / full-delta（生成器已合并到根包 golden_store_test.go）
```

生成后必须人工 diff 审查（`git diff --stat testdata/golden`），确认字节变化只来自
有意的格式变更。

> 注：`encrypted-store.rpk` 由 `encryption_test.go` 的 `TestGoldenEncryptedStoreGenerate`
> 在 `-update-golden` 下重新生成（固定 UUID/now/nonce + 静态 key），哈希由
> `TestGoldenManifest` 锁定；重写它需要走格式审查。

## 校验命令

CI 中 `go test ./...` 的 golden 测试以只读方式校验：任何样本与实现不一致即失败，
锁定的 Store 样本必须可打开且内容一致。

## 确定性来源

golden 生成固定了三个非确定性源（`rowpack.go` 的测试钩子）：
StoreUUID（`testUUIDOverride`）、CreatedAt（`testNowOverride`）、
SnapshotHeader.WriterNonce（`testNonceOverride`）。生产路径下 WriterNonce
每次写入随机；任何新引入的非确定性字段都必须同样纳入钩子覆盖，否则每次
`make golden` 都会产生无关 diff，破坏「人工 diff 审查只看格式变更」的前提。
