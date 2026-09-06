# RowPack v1.0 性能测试报告

> 日期：2026-09-06（性能优化后）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon, M 系列) / klauspost/compress v1.20.0 (zstd)
> 格式：BlockSize 256 KiB / Zstd / SyncCommit / 块缓存 64 MiB
> 复现：`go test ./ -run '^$' -bench . -benchtime=2x`；端到端 `go test ./ -run TestPerfEndToEnd -v`

## 1. 微基准（100k 行 × 7 列）

| 场景 | 指标 | 说明 |
| --- | --- | --- |
| FULL 顺序写 | 360 krows/s（72 MB/s），293 MB 分配/op | 提交边界一次 fsync |
| Get 热读（缓存命中） | 20 µs/op，896 B/op，7 allocs | 零磁盘 I/O |
| Get 冷读（缓存关闭） | 316 µs/op，466 KB/op | 单块读+解压+单条解码 |
| 并发 Get 1/8 goroutine | 195 µs / 195 µs | 读路径无全局锁，线性扩展 |
| Scan（100k 行） | 69 ms（1450 krows/s），109 MB 分配 | 块内游标 |
| Open 索引重放（100k 行） | 9.2 ms | 全量载入内存 |
| RebuildIndex（100k 行） | 92 ms | 从 .rpk 重建 |

## 2. 大规模（1M 行 × 7 列）

| 场景 | 指标 |
| --- | --- |
| FULL 顺序写 | 1.49 s（673 krows/s）——更大块更满、压缩更高效 |
| 随机读（2000 次，块缓存热） | 165 µs/op（6 kget/s），11 allocs |
| 全表 Scan | 770 ms（1299 krows/s） |

## 3. DELTA 链（FULL 10 万行 + 32 层 DELTA，每层 1000 行）

| 场景 | 指标 |
| --- | --- |
| 链头点查（父链解析 32 层） | 394 µs/op，23 allocs |
| 链头全表 Scan（132k 行合并） | 109 ms（1210 krows/s） |

## 4. 端到端（200k 行，正确性 + 计时回归测试）

| 阶段 | 耗时 |
| --- | --- |
| FULL 写入 + 提交 | 249 ms（804 krows/s） |
| 随机读校验（2062 次逐值比对） | 26 ms |
| 全表 Scan + 校验 | 120 ms |
| Close / Reopen | 19 ms |

## 5. 优化前后对比（v1.0 性能迭代）

| 基准 | 优化前 | 优化后 | 提升 |
| --- | --- | --- | --- |
| Scan 100k | 7.5 s / 43.9 GB 分配 | 69 ms / 109 MB | **~110x** |
| Get 热读 | 466 µs / 441 KB / 35 allocs | 20 µs / 896 B / 7 allocs | **~24x** |
| 并发 Get g1 | 498 µs | 195 µs | ~2.5x |
| FULL 写吞吐 | 300 krows/s / 996 MB 分配 | 360 krows/s / 293 MB | +20% / 分配 -70% |
| Get 冷读 | 327 µs / 985 KB / 58 allocs | 316 µs / 466 KB / 14 allocs | 分配 -53% |

## 6. 优化手段

1. zstd encoder/decoder sync.Pool 池化（按 level 分池）——消除每次块压缩/解压
   新建实例的 ~1 MiB 直方图分配（此前占写路径分配 60%+）。
2. `block.ParseRowAt` 单条读取——随机读不再解析整块目录，O(1) 于块大小。
3. Scan 块内游标——迭代器缓存当前块已解析 payload，同块连续行复用。
4. onFlush 直接传 raw——提交时不再重复解压刚压缩的块。
5. index.Builder body 预分配 + Build 直接返回结构化 Txn——提交不再重复 ParseTxn。
6. codec.EncodeInto——写路径复用编码缓冲区。

## 7. 正确性保障

- 优化全程 `go test ./...` 与 `go test -race ./...` 通过（含 32 goroutine 并发测试）。
- zstd 池：确定性输出测试 + 8 goroutine 并发 round-trip 测试。
- `TestPerfEndToEnd`：200k 行写→随机读逐值校验→全表 Scan 校验→Close/Reopen
  再校验，作为性能回归闸门。
