# Golden files 生成与保护策略

`testdata/golden/` 保存格式兼容性样本，由实现生成、人工审查后锁定，纳入 CI。
v1.0 发布后任何提交不得重写这些样本；需要修改时视为格式变更，必须同时提升
格式版本并创建新的样本族。

## 样本清单

| 文件 | 内容 | 里程碑 |
| --- | --- | --- |
| `empty-store.rpk/.rpi` | 仅两个 128 字节 Header 的空 Store（确定性 UUID/时间） | M1 |
| `rows-payload-all-types.bin` | 覆盖全类型值 + NULL 的确定性未压缩 Rows Payload | M3 |
| `full-delta-store.rpk/.rpi` | FULL + DELTA + 空 DELTA + 超大行（确定性 UUID/时间） | M6 |

损坏样本由 `TestM10CorruptSamples` 从健康 Store 动态构建（坏 Magic、未知主版本、
负载损坏、索引损坏），不静态保存。

## 生成命令

```sh
make golden     # 等价于 go test ./... -run TestGolden -args -update-golden
```

生成后必须人工 diff 审查（`git diff --stat testdata/golden`），确认字节变化只来自
有意的格式变更。

## 校验命令

CI 中 `go test ./...` 的 golden 测试以只读方式校验：任何样本与实现不一致即失败，
锁定的 Store 样本必须可打开且内容一致。
