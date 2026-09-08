# RowPack

RowPack 是一个使用 Go 实现的轻量级嵌入式二维表存储引擎，面向备份、快照、
差异归档和本地分析等「顺序写入、随机读取」场景。

- 双文件格式：`<base>.rpk`（数据，append-only，提交权威）+ `<base>.rpi`（索引，可重建）。
- 支持 FULL / DELTA 快照以及 INSERT / UPDATE / DELETE 变更。
- Zstandard 块压缩（默认 256 KiB 目标块）。
- 按快照、表和行随机访问，历史快照不可变、不受后续提交影响。
- 多读单写：读操作无锁并发，写操作单写者串行，提交原子可见。
- 校验、崩溃恢复与索引重建，不依赖独立 WAL。
- Schema 与源数据库设计元信息分层：`DefineSchema` 把 RowPack 自身的 Canonical
  Schema 写成引擎自产自销的 Table/Column 记录（内部 TLV）；它只服务于行编码/解码，
  不会按源数据库方言建模。源数据库原始元信息属于上层 Source Metadata，当前不提供
  通用公开读写 API，也不内建 CoreMetadata。

## 快速开始

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/rowpack/rowpack"
)

func main() {
	ctx := context.Background()
	db, err := rowpack.Create("/data/users-backup", rowpack.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	w, err := db.BeginSnapshot(ctx, rowpack.SnapshotFull, rowpack.SnapshotOptions{})
	if err != nil {
		log.Fatal(err)
	}
	defer w.Abort()

	schema := rowpack.Schema{
		TableID: 1,
		Version: 1,
		Name:    "users",
		Columns: []rowpack.Column{
			{Name: "id", Type: rowpack.TypeUint64},
			{Name: "name", Type: rowpack.TypeString},
			{Name: "created_at", Type: rowpack.TypeDateTime},
		},
	}
	if err := w.DefineSchema(schema); err != nil {
		log.Fatal(err)
	}
	if err := w.Insert(ctx, 1, 1001, 1, rowpack.Row{
		rowpack.Uint64(1001),
		rowpack.String("张三"),
		rowpack.DateTime(time.Now()),
	}); err != nil {
		log.Fatal(err)
	}
	full, err := w.Commit(ctx)
	if err != nil {
		log.Fatal(err)
	}

	row, err := db.Get(ctx, full.ID, 1, 1001, nil)
	if err != nil {
		log.Fatal(err)
	}
	name, _ := row[1].String()
	fmt.Println("row:", name)

	// DELTA 增量快照
	d, err := db.BeginSnapshot(ctx, rowpack.SnapshotDelta, rowpack.SnapshotOptions{Parent: full.ID})
	if err != nil {
		log.Fatal(err)
	}
	_ = d.Update(ctx, 1, 1001, 1, rowpack.Row{
		rowpack.Uint64(1001),
		rowpack.String("张三 (更新)"),
		rowpack.DateTime(time.Now()),
	})
	_ = d.Delete(ctx, 1, 1002)
	_, err = d.Commit(ctx)
	if err != nil {
		log.Fatal(err)
	}
}
```

## 关键概念

- **Store**：一对 `<base>.rpk` / `<base>.rpi` 文件及其运行时状态。
- **Snapshot**：不可变、原子提交的行变更集合，FULL 或 DELTA，形成父子链。
- **RowID**：表内稳定逻辑行标识，与业务主键相互独立。
- **Block**：压缩与校验单位，属于一个快照和一个表。
- **TypedTuple**：按 Schema 顺序编码的行负载，NULL 用位图表达。

## 行复用

读入口统一为借用/复用模式，消除每行的 Row 切片与 Decimal big.Int 分配
（Scan 场景每行分配降 ~86%）：

- `Iterator.Next()`：无参数，行解码进迭代器内部缓冲，缓冲跨调用复用；返回
  的 Row 到下一次 Next 前有效，整表 Scan 无逐行分配。
- `Get(ctx, snap, table, id, dst)`：解码进调用者提供的 Row 复用其底层数组；
  Get 是并发入口，nil dst 每次分配新行，dst 是唯一跨调用复用的方式。

需要跨调用保留的值需拷贝；通过 `String()`/`Bytes()`/`Decimal()` 访问器读
值始终安全（返回副本）。

```go
it, _ := db.Scan(ctx, full.ID, 1, rowpack.ScanOptions{})
defer it.Close()
for {
	row, ok := it.Next()
	if !ok {
		break
	}
	name, _ := row[1].String()
	_ = name
}
```

## 批量读取

对任意的 RowID 集合，`ReadBatch` 按块聚合：每个块至多加载、解压和校验一次，
块内多行共享同一次目录解析，然后按请求顺序一次性返回（v1.2）。与逐行 Get 相比，
冷块/大集合场景吞吐提升 3.6×–525×（`make bench-batch` 复现）；聚合效果可经
`Stats.Batch`（去重块数、解压字节）验证。错误语义与 Get 一致：任一 id 缺失或
已删除即整批返回 `ErrNotFound`。

```go
rows, err := db.ReadBatch(ctx, full.ID, 1, []RowID{1001, 1002, 1005})
if err != nil {
	log.Fatal(err)
}
for i, row := range rows {
	_ = i
	name, _ := row[1].String()
	_ = name
}
```

## 文档

- [需求规格](docs/REQUIREMENTS.md)
- [二进制格式 v1](docs/BINARY_FORMAT_V1.md)
- [单文件格式 v2 设计](docs/BINARY_FORMAT_V2.md)
- [v2 Go API 设计](docs/GO_API_DESIGN_V2.md)
- [v2 单文件开发计划](docs/DEVELOPMENT_PLAN_V2.md)
- [元数据格式 v1](docs/METADATA_FORMAT_V1.md)
- [Go API 设计](docs/GO_API_DESIGN.md)
- [开发计划](docs/DEVELOPMENT_PLAN.md)
- [v1.1 优化计划](docs/plan-v11.md)
- [v1.2 优化计划](docs/plan-v12.md)
- [性能测试报告](docs/perf-report.md)
- [源库 Key Range 映射](docs/SOURCE_KEY_RANGE_MAPPING.md)
- [ADR-001：.rpk 是提交权威](docs/adr/ADR-001.md)
- [ADR-002：元数据是引擎存储的数据](docs/adr/ADR-002.md)
- [ADR-003：v2 单文件且不持久化 Row Index](docs/adr/ADR-003.md)

## 命令

```sh
make test        # go test ./...
make race        # go test -race ./...
make fuzz-short  # 每个 fuzz 目标 5s
make golden      # 重新生成 golden files（格式变更时人工审查）
make bench       # 统一基准矩阵（v1.2 Tier 0），输出 docs/bench-results.txt
make bench-batch # 批量读对比：逐行 Get 基线 vs ReadBatch（v1.2 Tier 1）
```

## 参考基准

统一矩阵由 `make bench` 复现（`BenchmarkEnv` + `BenchmarkMainMatrix` +
`BenchmarkLatency`，BlockSize × 缓存 × 持久化 × mmap/readat 的剪枝矩阵，带
p50/p95/p99 与峰值 RSS），完整结果落盘 `docs/bench-results.txt`，要点见
[docs/perf-report.md](docs/perf-report.md) §1。环境：Go 1.27 / darwin/arm64 /
klauspost zstd v1.20 / BlockSize 256 KiB / Zstd / SyncCommit，数据集 100k 行 × 7 列。
数值随磁盘与 CPU 变化，仅作相对参考。

| 基准（256K/mmap 档） | 结果 |
| --- | --- |
| FULL 顺序写 / 隔离写 | ~1032 / 1996 krows/s |
| Get 热读（复用 dst） | ~0.5 µs / 2 allocs |
| Get 冷读 | ~255 µs / 6 allocs |
| 并发 Get 64 goroutine | ~6.7 µs（LRU 锁主导） |
| Scan 100k / Scan 1M | 13.7 ms / 243 ms |
| Get / Scan DeepChain（32 层） | 3.4 µs / 31.9 ms |
| Open 索引重放 / RebuildIndex | 5.7 ms / 27.2 ms |

> 注意：热读真实吞吐 ~2M get/s。README 早期版本的 7–12 µs、300 µs 等数值受到低
> `-benchtime` 一次性开销稀释，已由统一矩阵的预热逻辑消除；历史数据见
> docs/benchmarks-v1.md。

## 兼容性

- v1 磁盘格式冻结：golden files 纳入 CI，任何字节级变化视为格式变更。
- 枚举值、字段编号、golden 样本发布后不得修改。
- 支持 Linux / macOS / Windows amd64/arm64（跨进程写锁在无 flock 平台明确报错）。
