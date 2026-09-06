# RowPack v1.1 优化计划

> 文档状态：执行基线
> 目标版本：v1.1（性能与资源优化，不改 v1 磁盘格式、不破坏公开 API 契约）
> 基线：docs/benchmarks-v1.md「剩余热点（v1.1 方向）」
> 约束：不得修改已冻结的 v1 枚举、字段编号与 golden files；仅新增 API/内部实现优化。

## 0. 进度

- [x] V1.1-A 读路径 Row 复用（2026-09-07 完成）
- [x] V1.1-B 写路径分配削减（2026-09-07 完成）
- [ ] V1.1-C 冷读 mmap
- [ ] V1.1-D 索引落地内存

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

- [ ] `internal/iofile` 提供只读 mmap 视图（按需映射、分片 remap）。
- [ ] `block.Reader` 支持从 mmap 区域直接解压，减少一次用户态拷贝。
- [ ] 只读模式与缓存并存：mmap 命中不 cache，避免双份内存。

验收：冷读基准与分配下降；文件增长下 mmap 边界与截断行为正确；
Windows 平台无 mmap 时回退 ReadAt。

### V1.1-D：索引落地内存（架构级）

任务：

- [ ] 索引冷段（增量 Row Index）改为按快照分片的磁盘映射或惰性加载，
      保留热点段在内存。
- [ ] `Stats.IndexMemoryBytes` 反映真实常驻。

验收：1M 行 Open 后常驻内存显著低于全量 320 MB；随机读性能退化可控。

## 4. 完成门槛

- `go test ./...`、`go test -race ./...`、fuzz 冒烟、golden 全绿。
- v1 磁盘格式无字节变化（golden 比对）。
- docs/benchmarks-v1.md 与 docs/perf-report.md 记录 v1.1 后基线与对比。
- 新增 API 有 GoDoc 与 README 示例；GO_API_DESIGN.md 同步。

## 5. 迭代顺序

V1.1-A + V1.1-B 为增量优化，先做；V1.1-C/D 为架构级，各自独立迭代。