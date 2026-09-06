# v1.0 基准记录

> 记录时间：2026-09-06（含性能优化后基线）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon) / klauspost/compress v1.20.0 (zstd)
> 数据：100,000 行 × 7 列（uint64 + string + bool + int32 + float64 + datetime + decimal(scale=2)）
> 格式：BlockSize 256 KiB / Zstd / SyncCommit / Cache 64 MiB

运行方式：`go test ./ -run '^$' -bench . -benchtime=2x`

## 优化后基线

| 基准 | 优化前 | 优化后 | 说明 |
| --- | --- | --- | --- |
| BenchmarkFullSequentialWrite | 300 krows/s（996 MB 分配/op） | **360 krows/s（293 MB 分配/op）** | 提交边界一次 fsync |
| BenchmarkGetColdRead（缓存关） | 327 µs（985 KB） | 316 µs（466 KB，14 allocs） | 单 Block 读+解压+单条解码 |
| BenchmarkGetHotRead（缓存命中） | 466 µs（441 KB，35 allocs） | **20 µs（896 B，7 allocs）** | 零磁盘 I/O，单条解析 |
| BenchmarkConcurrentGet 1/8 goroutine | 498 / 335 µs | 195 / 195 µs | 读路径无全局锁 |
| BenchmarkScan（100k 行） | 7.5 s（43.9 GB 分配） | **69 ms（109 MB，734 K allocs）** | 块内游标 |
| BenchmarkOpenReplay（100k 行索引） | 8.7 ms | 9.2 ms | 索引全量重放入内存 |
| BenchmarkRebuildIndex（100k 行） | 92 ms | 92 ms | 从 .rpk 重建 .rpi |

## 性能优化内容（v1.0 性能迭代）

1. **zstd encoder/decoder 池化**（sync.Pool，按 level 分池）：消除每次块压缩/解压新建
   encoder/decoder 的 ~1 MiB 直方图分配（此前占写路径分配 60%+）。
2. **Get 单条读取**（`block.ParseRowAt`）：随机读不再解析整个块的目录，O(1) 于块大小，
   热读从 ~35 allocs 降到 7 allocs。
3. **Scan 块内游标**：迭代器缓存当前块的已解析 payload，同块连续行复用，避免每行
   重新整块解析——Scan 快约 110 倍。
4. **消除 flush 双重解压**：block builder 的 onFlush 直接传递已解压 raw payload，
   提交时不再把刚压缩的块解压一遍来提取索引条目。
5. **index.Builder**：Body 按已知计数预分配，`Build` 直接返回结构化 `Txn`（提交
   不再重复 `ParseTxn`）。
6. **codec.EncodeInto**：写路径复用编码缓冲区，消除每行 Encode 分配。

## 剩余热点（v1.1 方向）

- Scan/Get 仍需为每一行分配独立的 `Row`（调用者所有权语义），可用内存池或
  mmap 映射继续压。
- 写路径每行仍有 ~8 次小分配（Decimal big.Int、AddRow 去重 map、raw 块构建），
  可进一步缓冲复用。
