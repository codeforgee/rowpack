# RowPack

RowPack 是一个使用 Go 实现的轻量级嵌入式二维表存储引擎，面向备份、快照、
差异归档和本地分析等「顺序写入、随机读取」场景。

- **单文件格式**：`<base>.rpk` 一个文件承载全部数据与索引，数据块与每快照
  IndexTxn 交错追加，由扩展 SnapshotFooter 一次性原子提交（一次 fsync）。
  备份/迁移/复制即拷贝单个文件。
- **核心 API 按表名寻址**：`BeginFull/BeginDelta` 开启快照，`CreateTable(表名, 列)` 后
  内部 TableID/SchemaVersion 全部由引擎分配；`Insert/Update/Delete` 逐条流式写入，
  `Blocks`/`ScanBlocks` 暴露块级主键范围与原始变更流，支撑"块扫描批量比对"场景。
- 支持 FULL / DELTA 快照以及 INSERT / UPDATE / DELETE 变更；任意时刻可提交
  新 FULL checkpoint（快照 ID 全局递增，深度重置）。
- Zstandard 块压缩（默认 256 KiB 目标块）。
- 按快照、表和行随机访问，历史快照不可变、不受后续提交影响。
- 多读单写：读操作无锁并发，写操作单写者串行，提交原子可见。
- 校验与崩溃恢复不依赖独立 WAL：未提交尾部打开时截断；单个 IndexTxn 损坏
  时从该快照自身的数据块在内存重建索引，后续快照照常重放。
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

	w, err := db.BeginFull(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer w.Abort()

	if err := w.CreateTable("users", []rowpack.Column{
		{Name: "id", Type: rowpack.TypeUint64},
		{Name: "name", Type: rowpack.TypeString},
		{Name: "created_at", Type: rowpack.TypeDateTime},
	}); err != nil {
		log.Fatal(err)
	}
	if err := w.Insert(ctx, "users", 1001, rowpack.Row{
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

	row, err := db.Get(ctx, full, "users", 1001, nil)
	if err != nil {
		log.Fatal(err)
	}
	name, _ := row[1].String()
	fmt.Println("row:", name)

	// DELTA 增量快照
	d, err := db.BeginDelta(ctx, full)
	if err != nil {
		log.Fatal(err)
	}
	_ = d.Update(ctx, "users", 1001, rowpack.Row{
		rowpack.Uint64(1001),
		rowpack.String("张三 (更新)"),
		rowpack.DateTime(time.Now()),
	})
	_ = d.Delete(ctx, "users", 1002)
	_, err = d.Commit(ctx)
	if err != nil {
		log.Fatal(err)
	}
}
```

## 关键概念

- **Store**：单个 `<base>.rpk` 文件及其运行时状态。
- **Snapshot**：不可变、原子提交的行变更集合，FULL 或 DELTA，形成父子链。
- **RowID**：表内稳定逻辑行标识，与业务主键相互独立。
- **Block**：压缩与校验单位，属于一个快照和一个表。
- **TypedTuple**：按 Schema 顺序编码的行负载，NULL 用位图表达。

## 行复用

读入口统一为借用/复用模式，消除每行的 Row 切片、payload 复制与 Decimal
big.Int 分配：

- `Iterator.Next()`：无参数，行解码进迭代器内部缓冲，缓冲跨调用复用；返回
  的 Row 到下一次 Next 前有效，整表 Scan 无逐行分配（100k 行 × 7 列全表
  仅 ~135 次分配，String/Bytes 均为零复制 arena 视图）。
- `Get(ctx, snap, table, id, dst)`：解码进调用者提供的 Row 复用其底层数组；
  Get 是并发入口，nil dst 每次分配新行，dst 是唯一跨调用复用的方式。
  缓存热读为 2 allocs/16 B（String/Bytes 列按值复制，调用方安全持有）。

需要跨调用保留的值需拷贝；通过 `String()`/`Bytes()`/`Decimal()` 访问器读
值始终安全（返回副本）。

```go
it, _ := db.Scan(ctx, full, "users", rowpack.ScanOptions{})
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

`ReadBatch` 按块聚合：每个块至多加载、解密、解压和校验一次，不管请求中有多少行
落在这个块里。语义与逐行 `Get` 一致：所有 id 必须在该快照可见（缺失或已删除整批
返回 `ErrNotFound`）；返回顺序与输入 ids 一一对应（重复输入重复返回）；返回的行
归调用方所有、互不别名。

```go
rows, err := db.ReadBatch(ctx, full, "users", []RowID{1001, 1002, 1005})
if err != nil {
	log.Fatal(err)
}
for i, r := range rows {
	name, _ := r[1].String()
	_ = name
	_ = i
}

st := db.Stats().Batch // Calls/Rows/Blocks/RawBytes：聚合效果可量化（Blocks << len(ids)）
```

冷缓存连续 1000 行场景较逐行 Get 提升数百倍（`make bench-batch` 对比
`BenchmarkGetLoop1000` vs `BenchmarkReadBatch1000`）；热读千行批量
~229 µs · **19 allocs**，逐行 Get 基线 281 µs · 2,000 allocs。

## 文档

- [需求规格](docs/REQUIREMENTS.md)
- [二进制格式（v2 单文件）](docs/BINARY_FORMAT_V2.md) · [格式参考 HTML 版](docs/file-format-v2.html)
- [v2 文件格式性能重构计划](docs/FILE_FORMAT_REFACTOR_PLAN.md)
- [v2 Go API 设计](docs/GO_API_DESIGN_V2.md)
- [元数据格式（TLV）](docs/METADATA_FORMAT_V1.md)
- [IndexTxn 分 Chunk 压缩与加密](docs/INDEX_TXN_CHUNK_COMPRESSION.md)
- [数据分块加密可行性与决策](docs/DATA_BLOCK_ENCRYPTION_FEASIBILITY.md)
- [性能测试报告](docs/perf-report.md)
- [源库 Key Range 映射](docs/SOURCE_KEY_RANGE_MAPPING.md)
- [ADR-001：.rpk 是提交权威](docs/adr/ADR-001.md)
- [ADR-002：元数据是引擎存储的数据](docs/adr/ADR-002.md)
- [ADR-003：v2 单文件且不持久化 Row Index](docs/adr/ADR-003.md)

## 命令

```sh
make test        # go test ./...
make race        # go test -race ./...
make vet         # go vet ./...
make staticcheck # staticcheck ./...
make bench       # 统一基线套件（Env/矩阵/延迟 + 直读档），输出 docs/bench-results.txt
make bench-quick # 快速档：20k 行 + 3 次迭代全矩阵冒烟（~15s），输出 docs/bench-results-quick.txt
make bench-batch # 批量读对比：逐行 Get 基线 vs ReadBatch（10s 每场景）
make golden      # 重新生成 golden files（格式变更时人工审查）
```

## 参考基准

统一套件由 `make bench` 复现（`BenchmarkEnv` + `BenchmarkMainMatrix` 64 格矩阵 +
`BenchmarkLatency` 延迟分位数 + 直读档），完整结果落盘 `docs/bench-results.txt`，
要点与指标中文对照见 [docs/perf-report.md](docs/perf-report.md) §1–§2。
环境：Go 1.27 / darwin/arm64 / klauspost zstd v1.20 / BlockSize 256 KiB / Zstd /
SyncCommit（批量与缓存档另标注），数据集 100k 行 × 7 列。数值随磁盘与 CPU 变化，
仅作相对参考。吞吐量以中文口径表述：行吞吐量 = 万行/秒，点读吞吐量 = 万次/秒。

| 基准（256K/mmap/sync 档） | 吞吐量 / 延迟 |
| --- | --- |
| FULL 顺序写行吞吐量 | ~101.6 万行/秒（sync）· ~104.5 万行/秒（async）|
| 同构行写行吞吐量 | ~186 万行/秒（sync）· ~204 万行/秒（async，压缩比 0.044）|
| Get 热读点读吞吐量（复用 dst） | ~238 万次/秒 · ~0.42 µs · 2 allocs · 16 B/op |
| Get 冷读 | ~3,000 次/秒 · ~338 µs · 6 allocs · 160 B/op（池化瞬态解压）|
| 并发 Get 点读吞吐量（64 goroutine） | ~157 万次/秒 · ~0.64 µs |
| Scan 100k 扫描行吞吐量（热/冷） | ~738 / ~365 万行/秒（13.6 / 27.4 ms）· 全表 135 allocs |
| Scan 1M 扫描行吞吐量 | ~422 万行/秒（237 ms）· 全表 2,227 allocs |
| 深链（32 层）Get / Scan | ~45 万次/秒（2.2 µs）/ ~483 万行/秒（27.3 ms）|
| Open 索引重放 | ~1.6 ms |
| IndexTxn 损坏重开（内存重建） | ~19.6 ms（文件不改写）|
| 点读延迟 p50/p95/p99（热） | 417 / 500 / 667 ns |
| 点读延迟 p50/p95/p99（冷） | 336 / 400 / 591 µs |

> 口径：以上为统一矩阵预热后的 100k 数值；直读档 `BenchmarkGetHot` 等使用 20k
> 数据集（~272 ns ≈ 368 万次/秒），绝对值不可与矩阵互比。快速冒烟用
> `make bench-quick`（~15 秒），其数值不与基线比。

## 兼容性

- v2（单文件，Magic `ROWPACK2`）是当前且唯一的格式线；v1 预发布格式从未发布，
  v2 打开器在 magic 处即拒绝 v1 文件。
- golden files 纳入 CI：任何字节级变化视为格式变更。
- 加密 store 使用 AES-256-GCM；数据块与 IndexTxn 分域加密封装（nonce 位域
  互斥），Footer 绑定落盘字节 CRC，无需密钥即可检出撕裂。
- 支持 Linux / macOS / Windows amd64/arm64（跨进程写锁在无 flock 平台明确报错）。
