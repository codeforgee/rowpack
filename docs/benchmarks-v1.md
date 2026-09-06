# v1.0 基准记录

> 记录时间：2026-09-06（性能迭代完成，最终基线；v1.1 见文末）
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

> 2026-09-07 更新：以下前两条已在 v1.1 首批优化中落地（见 §v1.1 基准），
> 后两条为架构级，见 docs/plan-v11.md。

- ~~Scan/Get 每行仍需分配独立 `Row`~~ → 已落地 `GetInto`/`NextInto` 复用模式
  与 decimal 解码快速路径；ScanInto 每行分配降至 ~1（string 拷贝）。
- ~~写路径剩余 ~8 allocs/行~~ → schema.Validate 零分配化、bitmap 栈数组、
  decimal int64 零分配编码已落地；写路径 allocs 835K→702K（-16%）。
- 冷读受 zstd 解压 256 KiB + CRC 固有限制；mmap 读可减少用户态拷贝，属架构级。
- 索引全量载入内存（1M 行 ~320 MB）；mmap/分页索引为 v1.1 计划项。

## v1.1 基准（首批：Row 复用 + 写路径削减）

> 记录时间：2026-09-07
> 环境：Go 1.27.0 / darwin arm64 (Apple M1 Pro) / klauspost/compress v1.20.0
> 运行方式：`go test ./ -run '^$' -bench ... -benchtime=5x -benchmem`
> 注：FULL 写 B/op 受 zstd encoder 池冷启动影响波动较大（±40 MB），
> allocs/op 为稳定指标。

| 基准 | v1.0 基线 | v1.1 | 说明 |
| --- | --- | --- | --- |
| BenchmarkScan（100k 行） | 733K allocs / 92–100 MB | 47 ms / **401K allocs** / 90 MB | Validate 零分配 + decimal 快速解码 |
| BenchmarkScanInto（新） | — | 37 ms / **101K allocs** / 15.5 MB | 复用模式：每行仅剩 string 拷贝（541 krows/s） |
| BenchmarkGetHotRead | 8 allocs / 898 B | 7.8 µs / **4 allocs** / 846 B | 非 GetInto 路径同步受益 |
| BenchmarkGetHotReadInto（新） | — | 7.1 µs / **2 allocs** / 254 B | 复用模式（-75% allocs） |
| BenchmarkGetColdRead | 14 allocs | 300 µs / **10 allocs** | Validate + decimal 收益 |
| BenchmarkFullSequentialWrite | 835K allocs | 111 ms / **702K allocs** | decimal int64 零分配编码（-16%） |
| BenchmarkScanDeepChain | 89–109 ms | 71 ms / 529K allocs | 同步受益 |

v1.1 首批优化内容：

1. **Row 借用/复用 API**：`Store.GetInto`、`Iterator.NextInto`（GO_API_DESIGN
   §1/§8/§9 契约更新）。`codec.DecodeInto` 复用行切片与 Decimal `*big.Int`。
2. **decimal 解码零临时 big.Int**：canonical 校验改为字节级规则（不再
   encode 回比），≤8 字节走 int64 符号扩展快速路径；`decodeDecimalBytesInto`
   复用目标 big.Int。
3. **decimal 编码零分配**：int64 值直接按位构造两补码字节写入目标缓冲
   （`appendDecimalBytes`），消除 `Bytes()` + 补码前置的每行 1–2 次分配；
   与参照编码器逐字节等价性测试覆盖。
4. **schema.Validate 零分配**：≤64 列用 O(n²) 名称比较替代 map（读写路径
   每行调用不再分配）；bitmap 零填充改栈数组。

正确性：`go test ./...`、`go test -race ./...`、fuzz（TupleDecode/
DecimalBytes）、golden files 全绿；`TestPerfEndToEnd` 端到端回归通过。
新增 `TestDecodeIntoReuse`、`TestAppendDecimalEquivalence`、`TestGetIntoReuse`、
`TestNextIntoReuse`、`TestNextIntoEndRowID`。