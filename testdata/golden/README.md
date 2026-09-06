# Golden files 生成与保护策略

`testdata/golden/` 保存格式兼容性样本，由实现生成、人工审查后锁定，纳入 CI。
v1.0 发布后任何提交不得重写这些样本；需要修改时视为格式变更，必须同时提升
格式版本并创建新的样本族。

## 样本清单（按里程碑逐步补齐）

| 文件 | 内容 | 里程碑 |
| --- | --- | --- |
| `empty-store.rpk/.rpi` | 仅两个 128 字节 Header 的空 Store | M1 |
| `none-full-*.rpk/.rpi` | None 压缩的 FULL 快照 | M3 |
| `zstd-full-*.rpk/.rpi` | 覆盖全类型值的 Zstd FULL 快照 | M5 |
| `full-delta-*.rpk/.rpi` | FULL + DELTA（INSERT/UPDATE/DELETE） | M6 |
| `empty-delta-*.rpk/.rpi` | 空 DELTA 快照 | M6 |
| `oversize-row-*.rpk/.rpi` | 超过目标块大小的单行 | M6 |
| `data-ahead-*.rpk/.rpi` | 数据领先索引的恢复样本 | M8 |
| `corrupt-*.rpk/.rpi` | 各类损坏文件（Header/Block/Footer/Index） | M8 |

## 生成命令

```sh
make golden     # 等价于 go test ./... -run TestGolden -args -update-golden
```

生成后必须人工 diff 审查（`git diff --stat testdata/golden`），确认字节变化
只来自有意的格式变更。

## 校验命令

CI 中 `go test ./...` 的 golden 测试以只读方式校验：任何样本与实现不一致即失败。
