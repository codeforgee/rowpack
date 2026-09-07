# RowPack v1.1 优化计划

> 文档状态：执行基线
> 目标版本：v1.1（性能与资源优化，不改 v1 磁盘格式、不破坏公开 API 契约）
> 基线：docs/benchmarks-v1.md「剩余热点（v1.1 方向）」
> 约束：不得修改已冻结的 v1 枚举、字段编号与 golden files；仅新增 API/内部实现优化。

## 0. 进度

- [x] V1.1-A 读路径 Row 复用（2026-09-07 完成）
- [x] V1.1-B 写路径分配削减（2026-09-07 完成）
- [x] V1.1-C 冷读 mmap（2026-09-07 完成）
- [x] V1.1-D 索引落地内存（2026-09-07 完成：紧凑分片 + 惰性基础）

## 1. 目标

v1.0 已达成 110x Scan / 45x Get / 写路径分配 -80%。v1.1 聚焦 v1.0 报告中
剩余的四条热点，目标是：

- 读路径（Scan/Get）每行分配显著下降：消除 Row 切片与临时 big.Int，提供
  显式复用 API（借用/复用模式，扩展 API 契约但不破坏现有调用）。
- 写路径每行分配从 ~8 降到 ~4 以下：去掉 schema 重复校验、bitmap 分配与
  编码内部小分配。
- mmap 读（架构级，减少用户态拷贝）。
- 分页/mmap 索引（架构级，降低 1M 行 ~320 MB 的常驻内存）。

## 2. 执行原则

1. 所有改动必须保持 v1 磁盘格式字节一致（golden 回归）。
2. 现有公开 API 语义不变；复用模式只以新增方法提供。
3. 并发语义不变：`go test -race ./...` 通过；读路径无全局锁。
4. 每个优化有可复现基准与回归测试（pprof 证明优化点、端到端数值守住）。
5. 架构级改动（mmap、分页索引）单独迭代、单独评审，不与增量优化混提。

## 3. 工作项

### V1.1-A：读路径 Row 复用（codec + rowpack）

热点证据（100k 行 Scan + 1k Get pprof）：

- `codec.Decode` 34.4% allocs：`make([]Value, len(columns))` 每行 1 次，
  `schema.Validate` 每行 1 个 map。
- `decodeDecimalBytes` 15.2%：canonical 校验通过 `encodeDecimalBytes` 反向
  重编码（多个临时 big.Int），负数值还要额外 `Lsh/Sub`。

任务：

- [ ] `codec.DecodeInto(row, data, schema, limits)`：复用调用者行切片与
      十进制列已有的 `*big.Int`（`decodeDecimalBytesInto`）。
- [ ] decimal canonical 校验改为字节级规则（无 big.Int 分配），
      ≤8 字节负值走 int64 符号扩展快速路径。
- [ ] `schema.Validate` 零分配化：小列数用嵌套比较查重名，不做 map。
- [ ] `Store.GetInto(ctx, snap, table, rowID, dst Row) (Row, error)`。
- [ ] `Iterator.NextInto(dst Row) bool`：与 `Next` 共享推进逻辑，行解码进
      调用者缓冲；`Row()`/`Next()` 现有语义不变。

验收：`Decode` 行为与 `DecodeInto` 完全一致（参数化往返测试）；
`BenchmarkScanInto` 与 `BenchmarkGetHotReadInto` 每行分配显著下降；
race/fuzz/golden 全绿。

### V1.1-B：写路径每行分配削减

热点证据：`EncodeInto` 5.6%（`schema.Validate` map + `make(bitmap)`）、
`encodeDecimalBytes` 20.3%（canonical 校验临时 big.Int、负数路径）。

任务：

- [ ] `EncodeInto` 的 bitmap 区域零填充改为栈数组追加，去掉 `make`。
- [ ] `schema.Validate` 零分配化（见 A）与 `encodeDecimalBytes` 负数
      路径复用局部缓冲。

验收：写路径 allocs/行下降约一半；写吞吐不掉。

### V1.1-C：冷读 mmap（架构级）

任务：

- [x] `internal/iofile` 提供只读 mmap 视图（整文件映射、增长时在写锁下
      remap，旧映射仅在无活动视图时释放；mmap 失败永久回退 ReadAt 拷贝；
      Truncate/Close 释放映射避免 SIGBUS）。
- [x] `block.Reader` 支持从 mmap 区域直接切片 stored payload，zstd 解压到
      全新缓冲；None 压缩块复制后返回，缓存永不别名文件映射。
- [x] `fileformat.verifyCRC` 改为非变异实现（分段 CRC），解码器契约升级为
      「输入不可变」，只读映射可安全作为解码输入。

语义与边界：

- 映射生存期由 RWMutex 串行化：视图仅在 done 之前有效，remap 等待所有
  活动视图退出后才能 munmap；header 视图在解析后立即释放，避免与后续
  payload 视图的 remap 死锁。
- 平台：unix（Linux/macOS/BSD）启用；Windows 无 mmap 回退 ReadAt。
- 恢复截断（Appender.Truncate）同步释放映射；映射失败永久降级，读路径
  不会因 mmap 限制而整体不可用。

验收：冷读基准 300µs/418KB → 273µs/361KB/op（消除 256 KiB stored 堆分配
与一次用户态拷贝）；mmap 与 ReadAt 逐块等价性测试、并发 remap race 测试、
全量测试/race/fuzz/golden 全绿。

### V1.1-D：索引落地内存（架构级）

任务：

- [x] 增量 Row Index 从 `map[snapshot]map[table]map[rowID]*RowLoc` 重构为
      按快照/表分片的紧凑排序切片（`rowShard`，24 B/行，含 Tombstone），
      `Row` 走二分查找，`RowKeys` 直接返回分片（消除每次 Scan 的 map 展开
      + 全量排序）。1M 行常驻 ~320 MB → **22 MB（-93%）**，
      `Stats.IndexMemoryBytes` 反映真实分片体积。
- [x] 顺序写入快路径：分片构建时先验证已排序（顺序写常见场景 O(n)），
      乱序才排序；重放开销不回退（100k 行 OpenReplay 9.2ms → 8.2ms）。
- [x] 深链与全表扫描同步受益：ScanDeepChain 89→52 ms，Scan 47→32 ms，
      ScanInto 37→22 ms（928 krows/s）。
- [ ] 后续（v1.2 方向）：按快照分片的磁盘映射/惰性加载，Open 后冷快照
      行索引不驻留内存（当前已为零分配紧凑结构，收益有限）。

验收：1M 行 Open 后常驻 22 MB（24 B/row）显著低于 320 MB；随机读与扫描
性能无退化（深链/全表扫描反升 27–42%）；全量测试/race/fuzz/golden 全绿。

## 4. 完成门槛

- `go test ./...`、`go test -race ./...`、fuzz 冒烟、golden 全绿。
- v1 磁盘格式无字节变化（golden 比对）。
- docs/benchmarks-v1.md 与 docs/perf-report.md 记录 v1.1 后基线与对比。
- 新增 API 有 GoDoc 与 README 示例；GO_API_DESIGN.md 同步。

## 5. 迭代顺序

V1.1-A + V1.1-B 为增量优化，先做；V1.1-C/D 为架构级，各自独立迭代。