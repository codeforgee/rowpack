# v1.0 基准记录

> 记录时间：2026-09-06（性能迭代完成，最终基线；v1.1 见文末）。注：文中 GetInto/NextInto/ScanInto 等 API 名已在 1.2 前 API 定型中分别更名为 Get/Next（借用/复用语义并入主入口），历史记录不另改写。
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
5. **冷读 mmap**（同日第二批）：`iofile.Appender.View` 只读整文件映射、
   增长 remap（RWMutex 串行化，mmap 失败永久回退 ReadAt，Truncate/Close
   释放映射）；`block.Reader` 直接切片映射区的 stored payload，zstd 解压
   到全新缓冲，None 块复制后返回（缓存永不别名映射）；`fileformat.verifyCRC`
   改非变异实现，解码器契约升级为「输入不可变」。冷读 300µs/418KB →
   273µs/361KB/op（消除 256 KiB stored 堆分配与一次用户态拷贝，-9% 耗时）。
   新增 mmap/ReadAt 逐块等价、增长/截断边界、并发 remap race 测试。
6. **索引紧凑分片**（同日第三批）：增量 Row Index 从三层嵌套 map（~120 B/行）
   重构为按快照/表分片的紧凑排序切片（24 B/行）：1M 行常驻 ~320 MB →
   **22 MB（-93%）**；`Row` 二分查找、`RowKeys` 零拷贝返回分片，全表与
   深链 Scan 32/52 ms（较重构前再 -32%/-27%，ScanInto 928 krows/s）。
   `Stats.IndexMemoryBytes` 反映真实分片体积。

正确性：`go test ./...`、`go test -race ./...`、fuzz（TupleDecode/
DecimalBytes）、golden files 全绿；`TestPerfEndToEnd` 端到端回归通过。
新增 `TestDecodeIntoReuse`、`TestAppendDecimalEquivalence`、`TestGetIntoReuse`、
`TestNextIntoReuse`、`TestNextIntoEndRowID`。
## v1.1+ 增量基准（2026-09-07 第二批：写路径 Build 逃逸、Scan 零分配、CRC 变参）

> 环境同 v1.1 节（Go 1.27 / M1 Pro / zstd v1.20）。运行方式：
> `go test ./ -run '^$' -bench ... -benchtime=5x -benchmem`
> 对比列为 v1.1 记录基线（同机重跑「上一批」数值见正文）。

| 基准 | v1.1 基线 | v1.1+ | 变化 |
| --- | --- | --- | --- |
| BenchmarkScan（100k 行） | 32–47 ms / 401K allocs / 90 MB | **15.6 ms / 189 allocs / 5.3 MB** | 耗时 -50%+，allocs -99.9% |
| BenchmarkScan1M | 混入旧基线 ~770ms | **238 ms / 4.2K allocs / 156 MB（841 krows/s）** | allocs -99.6% |
| BenchmarkScanDeepChain（32 层） | 71 ms / 529K allocs | **30.8 ms / 319 allocs / 6.3 MB** | -57% |
| BenchmarkGetColdRead | ~300 µs / 14 allocs | **269 µs / 10 allocs** | -11% |
| BenchmarkConcurrentGet g1/g8 | ~140 µs / 17–26 allocs | **80 / 78 µs / 12 / 11 allocs** | -43% |
| BenchmarkOpenReplay（100k 行） | 8.2–9.2 ms / 100K allocs | **6.4 ms / 512 allocs** | -90%+ allocs |
| BenchmarkRebuildIndex（100k 行） | ~92 ms / 405K allocs | **62.1 ms / 4.6K allocs** | -99% allocs |
| BenchmarkWrite1M | 1.49 s（旧）/ 2.0M allocs | **216 ms / 925 krows/s / 1.0M allocs** | 耗时 -47% |
| BenchmarkIsolatedInsert（仅库写，无 benchRow 噪声） | — | **63 ms / 100k 行 / 1.6K allocs（~1.6M 行/s）** | 库写路径 ~0.016 alloc/行 |
| BenchmarkGetHotRead / Into | 9.9 / 7.1 µs | 9.5 µs / 4 allocs；2 allocs | 持平 |

> 注：FullSequentialWrite / Write1M 报告的 allocs/B 大部分来自基准自身的
> benchRow 构造（fmt.Sprintf + big.NewInt），库写路径的真实分配见
> BenchmarkIsolatedInsert。内核码 EncodeInto 0 allocs/调用（602 ns），
> DecodeInto 复用路径 1 alloc/行（string 拷贝，见下）。

本批优化内容：

1. **index.Builder.Build 消除按项逃逸分配**：entries 直接 marshal 进预分配
   body 的预留区（MarshalTo 因错误路径不可内联，原 `var e [Size]byte` 按行
   逃逸到堆，占写路径 allocs 的 85%）。100k 行提交 allocs 100K → ~2K。
2. **Scan 字符串 arena**（codec.DecodeInto + StringSink）：
   Iterator 持 32 KiB 仅追加分块，String 解码为分块上的 `unsafe.String`
   视图；分块满则换新、绝不原地扩容，旧分块仅随引用被 GC 回收——跨 Next
   保留的字符串与「访问器返回拷贝」行为一致（同内存测试覆盖）。整表 Scan
   从每行 1 次字符串拷贝分配降为每块级少量分块分配。
3. **Scan 目录复用**：`block.ParseRowsDirectory(raw, n, entries)` 让迭代器
   跨块复用 `RowsIndex.Entries` 切片，消除每块目录构建
   （~78 块/scan 的切片增长）。
4. **Scan 单层快路径**：FULL（无父链）场景线性遍历排序分片，跳过 k-way
   heap 机制；多层 (DELTA 链) 路径不变。
5. **读写热路径跳过逐行 schema 重校验**：EncodeInto/DecodeInto 改为
   「schema 必须已验证」契约（DefineSchema/重放时验证），入口只保留廉价
   列数检查；payload 字节级校验与 limit 不变。
6. **codec.verifyCRC 分段 CRC**：`crc32cZeroGap` 消除 CRC32CConcat 变参
   分配——每条索引/块条目 Unmarshal 不再分配（OpenReplay 100K→512 allocs
   的主因）。

正确性：`go test ./...`、`go test -race ./...`、codec/fileformat fuzz
冒烟、golden 全绿；新增 `TestScanStringViewsSurviveArenaRotation`、
`TestCodecSinkParity`、`TestParseRowsDirectoryReuse`。

## 剩余热点与后续方向

- 冷读受「点查需解压整块 256 KiB + 全块 CRC」固有限制：行粒度索引到块内
  偏移（v2 格式）或列存分块可突破；当前 mmap 已消除一次用户态拷贝。
- Scan 冷首轮需逐块解压（热迭代命中 64 MiB 块缓存），1M 行全扫的 156 MB
  B/op 中大部分是首轮 78 块的解压缓冲（缓存持有，非逐行抖动）。
- 写路径已 ~0.016 alloc/行（隔离基准），剩余是 zstd 编码吞吐
  （fastEncoder，~590 ns/行）；调低压缩级别可换吞吐，属格式/空间权衡。
- Open 后索引全量驻留（1M 行 ~22 MB，24 B/行，已紧凑化）；按快照分片的
  磁盘映射/惰性加载为 v1.2 方向，收益有限。
