# Golden files 生成与保护策略

`testdata/golden/` 保存格式兼容性样本，由实现生成、人工审查后锁定，纳入 CI。
格式锁定后任何提交不得重写这些样本；需要修改时视为格式变更，必须同时提升
格式版本并创建新的样本族。

## 样本清单

| 文件 | 内容 | 里程碑 |
| --- | --- | --- |
| `empty-store.rpk` | 仅 128 字节 FileHeader 的空 Store（确定性 UUID/时间） | M0 |
| `rows-payload-all-types.bin` | 覆盖全类型值 + NULL 的确定性未压缩 Rows Payload | M3 |
| `full-delta-store.rpk` | FULL + DELTA + 空 DELTA + 超大行（单文件，含内嵌 IndexTxn） | M2 |
| `encrypted-store.rpk` | 加密 FULL Store（None 压缩 + 固定 key，锁定 Header 加密字段/块 Flags/KeyEpoch/密文布局） | M6 |

## 锁定摘要（SHA-256）

| 文件 | SHA-256 |
| --- | --- |
| `empty-store.rpk` | `cd0a96b72ad858d8bceb946b4ae77b1667b6bf17b9d79d72c9b282a52ddc34f7` |
| `rows-payload-all-types.bin` | `db95f863c3250ee64f55e09400dd4327a790814ca2846c7accbd0b3eca1f7433` |
| `full-delta-store.rpk` | `71b2c6956c3025230f1fe99d10261c37991eb223c3dccfc766c9fea7ec348ac3` |
| `encrypted-store.rpk` | `afaefc7673d8e32e1d5d4854c956b070febc50fc3c0edcde76347984c17fd3d8` |

这些摘要描述当前冻结的 v2 IndexTxn Chunk 格式。修改任一摘要均视为有意的磁盘格式变更，
必须经过格式审查并按版本策略建立新的 golden 样本族。

损坏样本由 `TestM10CorruptSamples` 从健康 Store 动态构建（坏 Magic、未知主版本、
负载损坏、IndexTxn 损坏），不静态保存。

## 生成命令

```sh
make golden     # 依次运行含 golden 生成器的 root、internal/block、internal/fileformat 包
```

生成后必须人工 diff 审查（`git diff --stat testdata/golden`），确认字节变化只来自
有意的格式变更。

## 校验命令

CI 中 `go test ./...` 的 golden 测试以只读方式校验：任何样本与实现不一致即失败，
锁定的 Store 样本必须可打开且内容一致。

## 确定性来源

golden 生成固定了三个非确定性源（`rowpack.go` 的测试钩子）：
StoreUUID（`testUUIDOverride`）、CreatedAt（`testNowOverride`）、
SnapshotHeader.WriterNonce（`testNonceOverride`）。生产路径下 WriterNonce
每次写入随机；任何新引入的非确定性字段都必须同样纳入钩子覆盖，否则每次
`make golden` 都会产生无关 diff，破坏「人工 diff 审查只看格式变更」的前提。
