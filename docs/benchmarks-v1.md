# v1.0 基准记录

> 记录时间：2026-09-06  
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon) / klauspost/compress v1.20.0 (zstd)  
> 数据：100,000 行 × 7 列（uint64 + string + bool + int32 + float64 + datetime + decimal(scale=2)）  
> 格式：BlockSize 256 KiB / Zstd / SyncCommit / Cache 64 MiB

运行方式：`go test ./ -run '^$' -bench . -benchtime=2x`

| 基准 | 数值 | 说明 |
| --- | --- | --- |
| BenchmarkFullSequentialWrite | ~286 krows/s（~57 MB/s），1.0 MB 分配/行 | 提交边界一次 fsync |
| BenchmarkGetColdRead（缓存关） | ~413 µs/op | 单 Block 读 + 解压 + 解码 |
| BenchmarkGetHotRead（缓存命中） | ~70 µs/op | 零磁盘 I/O，解析+解码 |
| BenchmarkConcurrentGet 1/8/32/64 goroutine | ~215–393 µs/op | 读路径无全局锁 |
| BenchmarkScan（100k 行） | ~7.2 s | 逐行解码，v1 正确性优先 |
| BenchmarkOpenReplay（100k 行索引） | ~8.7 ms | 索引全量重放入内存 |
| BenchmarkRebuildIndex（100k 行） | ~92 ms | 从 .rpk 重建 .rpi |

## 观测到的热点（v1 后续优化方向）

1. Scan 逐行调用 block 解析（每行重解析整块），块内顺序迭代是首要优化。
2. Get 热读每次 Get 仍产生 ~440 KB 分配（行解码 + 块解析副本），可用 arena 或
   块内游标复用减少。
3. 写路径每行编码有 ~1 KB 分配，可复用缓冲区。

这些优化不改变 v1 磁盘格式，均属于 v1.1 范畴。
