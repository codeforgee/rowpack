# RowPack 性能测试报告

> 文档状态：统一基线；历史批次数据保留在本文末尾和 docs/benchmarks-v1.md
> 日期：2026-09-07（当前基线）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon, M 系列) / klauspost/compress v1.20.0 (zstd)
> 格式：BlockSize 256 KiB / Zstd / SyncCommit / 块缓存 64 MiB
> 复现命令见 §1；端到端：`go test ./ -run TestPerfEndToEnd -v`

## 1. 当前统一基线（2026-09-07）

以下数据是当前性能比较的唯一基准。除特别说明外，均为 Go 1.27 / darwin arm64
(Apple M1 Pro) / zstd v1.20 / BlockSize 256 KiB / Cache 64 MiB / SyncCommit，
数据集为 100k 行 × 7 列；运行方式为：

```sh
go test ./ -run '^$' -bench 'Benchmark(FullSequentialWrite|GetColdRead|GetHotRead|GetHotReadInto|ConcurrentGet|Scan|ScanInto|ScanDeepChain|GetDeepChain|OpenReplay|RebuildIndex|IsolatedWrite)$' -benchtime=5x -benchmem
```

| 场景 | 当前结果 | 备注 |
| --- | --- | --- |
| FULL 顺序写 | 95 ms/op | 包含一次 SyncCommit |
| 隔离写入 | 49.8–51.3 ms/100k 行 | 预构造 Row，约 2M 行/s |
| Get 热读 | 9.5 µs/op，4 allocs | 缓存命中 |
| Get 热读（复用 dst） | 7.1 µs/op，2 allocs | 缓存命中 |
| Get 冷读 | 269 µs/op，10 allocs | 缓存关闭，mmap |
| 并发 Get | 5.9/5.0/6.0/4.9 µs | 1/8/32/64 goroutine，热缓存 |
| Scan 100k | 15.6 ms/op，189 allocs | FULL，热缓存 |
| Scan 1M | 238 ms/op | 约 841k rows/s |
| Scan DeepChain | 30.8 ms/op | FULL + 32 层 DELTA |
| Get DeepChain | 4.2 µs/op | 32 层，热缓存 |
| Open Replay | 5.1 ms/op | 100k 行索引 |
| RebuildIndex | 31.4 ms/op | 100k 行 |

说明：历史报告中的 10.3/20/9.5 µs、47/62/69/15.6 ms 等数值来自不同优化批次或不同
运行参数，不再并列作为当前结果。`benchmarks-v1.md` 保留这些历史数据，仅用于回溯。

访问模式说明：快照的主要使用方式预计是按表、范围或 RowID 集合批量读取，因此单行冷读
269 µs 仅作为诊断指标。v1.2 应以批量请求涉及的 Block 数、实际读取次数、解压次数、
返回行吞吐和 p95 延迟作为主要读取指标。

## 2. 历史微基准（v1.0，100k 行 × 7 列）

| 场景 | 指标 | 说明 |
| --- | --- | --- |
| FULL 顺序写 | 122 ms/op，196 MB 分配/op | 提交边界一次 fsync |
| Get 热读（缓存命中） | 10.3 µs/op，898 B/op，8 allocs | 零磁盘 I/O |
| Get 冷读（缓存关闭） | 291 µs/op，439 KB/op | 单块读+解压+单条解码 |
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
| 链头全表 Scan（132k 行合并） | 89 ms（1480 krows/s） |

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

1. zstd encoder/decoder sync.Pool 池化 + encoder 输出缓冲池——消除每次块压缩/解压
   新建实例的 ~1 MiB 直方图分配及 EncodeAll 的 256 KiB 输出预分配。
2. `block.ParseRowAt` 单条读取——随机读 O(1) 于块大小。
3. Scan 块内游标 + `ParseRowsDirectory` 轻量目录解析——同块连续行复用目录，
   记录字节按需切片，不解析逐条记录头。
4. `FlushedBlock`——builder 直接传递已构建目录条目，提交不再重复解析 payload。
5. index.Builder body 预分配 + Build 直接返回结构化 Txn；`Reserve` 预分配
   entries 切片与去重 map。
6. codec.EncodeInto——写路径复用编码缓冲区。
7. buildRawPayload 复用 uncompressed payload scratch 缓冲。

## 7. 正确性保障

- 优化全程 `go test ./...` 与 `go test -race ./...` 通过（含 32 goroutine 并发测试）。
- zstd 池：确定性输出测试 + 8 goroutine 并发 round-trip 测试。
- `TestPerfEndToEnd`：200k 行写→随机读逐值校验→全表 Scan 校验→Close/Reopen
  再校验，作为性能回归闸门。
