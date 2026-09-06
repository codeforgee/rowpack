# v1.0 基准记录

> 记录时间：2026-09-06（性能迭代完成，最终基线）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon) / klauspost/compress v1.20.0 (zstd)
> 数据：100,000 行 × 7 列（uint64 + string + bool + int32 + float64 + datetime + decimal(scale=2)）
> 格式：BlockSize 256 KiB / Zstd / SyncCommit / Cache 64 MiB

运行方式：`go test ./ -run '^$' -bench . -benchtime=3x`

## 最终基线

| 基准 | 初始 | 最终 | 提升 |
| --- | --- | --- | --- |
| BenchmarkFullSequentialWrite | 300 krows/s（996 MB 分配/op，1.04M allocs） | **122 ms/op（196 MB，835K allocs）** | 分配 -80% |
| BenchmarkGetColdRead（缓存关） | 327 µs（985 KB，58 allocs） | **291 µs（439 KB，14 allocs）** | 分配 -55% |
| BenchmarkGetHotRead（缓存命中） | 466 µs（441 KB，35 allocs） | **10.3 µs（898 B，8 allocs）** | **~45x** |
| BenchmarkConcurrentGet 1/8 goroutine | 498 / 335 µs | 195 / 195 µs | ~2.5x |
| BenchmarkScan（100k 行） | 7.5 s（43.9 GB 分配） | **62 ms（97 MB，734K allocs）** | **~120x** |
| BenchmarkScanDeepChain（32 层，132k 行） | 109 ms（152 MB） | **89 ms（127 MB）** | -18% |
| BenchmarkGetDeepChain（32 层点查） | 394 µs | 384 µs | 持平 |
| BenchmarkOpenReplay（100k 行索引） | 8.7 ms | 9.2 ms | 持平 |
| BenchmarkRebuildIndex（100k 行） | 92 ms | 92 ms | 持平 |

## 大规模 / 真实场景（100 万行）

| 场景 | 指标 |
| --- | --- |
| FULL 顺序写 | ~1.5 s（670 krows/s） |
| 随机读（2k 次，缓存受限） | 165 µs/op（11 allocs） |
| 全表 Scan | 770 ms（1300 krows/s） |
| 200k 行端到端（写/随机读/Scan/重开） | 249 ms / 26 ms / 120 ms / 19 ms |

## 性能优化内容（v1.0 性能迭代）

### 读路径
1. **zstd decoder 池化**（sync.Pool，`WithDecoderLowmem`）：消除每次块解压新建 decoder。
2. **Get 单条读取**（`block.ParseRowAt`）：随机读 O(1) 于块大小，不再整块解析目录。
   Get 热读从 35 allocs / 441 KB 降到 8 allocs / 898 B。
3. **Scan 块内游标 + 轻量目录解析**（`block.ParseRowsDirectory`）：迭代器缓存当前块的
   RowsIndex（只解析 payload 头 + 定长目录，不解析逐条记录头、不构建记录切片），
   同块连续行按需切片记录字节。Scan 从 7.5 s 降到 62 ms（~120x）。

### 写路径
4. **zstd encoder 池化**（按 level 分池）+ **输出缓冲池**：klauspost `EncodeAll` 对
   nil dst 会预分配 `make([]byte, 0, len(src))`（每块 256 KiB），传自有大缓冲跳过
   该分配；压缩结果（几 KB）复制到独立缓冲。
5. **消除 flush 双重解析**：`FlushedBlock` 让 builder 直接把已构建的目录条目
   （Rows/Meta）交给 onFlush，rowsFlush/metaFlush 不再 ParseRowsPayload /
   metadata.Parse 重新提取索引条目。
6. **index.Builder.Reserve**：提交前按已 flush 的块统计总数，预分配 blocks/meta/rows
   切片与行去重 map，消除流式 Append 的增长重分配。
7. **buildRawPayload 缓冲复用**：builder 复用 uncompressed payload scratch 缓冲。
8. **codec.EncodeInto**：写路径复用行编码缓冲区。

## 剩余热点（v1.1 方向）

- Scan/Get 每行仍需分配独立 `Row`（调用者所有权语义）：Row 切片 + string/bytes
  复制 + Decimal big.Int，约占 Scan 分配 40%。引入借用/复用模式需扩展 API 契约。
- 写路径剩余 ~8 allocs/行，主要为 map 去重与编码内部小分配。
- 冷读受 zstd 解压 256 KiB + CRC 固有限制；mmap 读可减少用户态拷贝，属架构级。
- 索引全量载入内存（1M 行 ~320 MB）；mmap/分页索引为 v1.1 计划项。