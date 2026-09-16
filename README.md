# RowPack

RowPack 是一个使用 Go 实现的轻量级嵌入式二维表存储引擎，面向备份、快照、差异归档和本地分析等
「顺序写入、随机读取」场景。

- **单文件格式**：`<base>.rpk` 承载全部数据与索引。数据块与每快照 IndexTxn 交错追加，由扩展
  SnapshotFooter 一次性原子提交（一次 fsync）；备份/迁移/复制即拷贝单个文件。
- **按表地址寻址**：`Begin(ctx, NoParent)` 创建首个 FULL，`Begin(ctx, parent)` 创建 DELTA；
  `DefineTable` 后用 `Insert/Update/Delete/ApplyBatch` 流式写入；`Blocks`/`ScanBlocks` 暴露
  块级主键范围与原始变更流，支撑「块扫描批量比对」。
- 支持 FULL / DELTA 快照与 INSERT / UPDATE / DELETE 变更；任意时刻可提交新 FULL checkpoint
  （快照 ID 全局递增，深度重置）。
- **表 ns 与地址**：`DefineTable` 用默认 ns（`user`）且不写额外元数据，`DefineTableIn` 可指定
  其他 ns。表身份是 `(NS, Name)`，同名表可在不同 ns 共存；收表的 API 收**地址字符串**——默认
  ns 用裸名（`"users"`），其他 ns 加前缀（`"public.users"`），`Qualify`/`SplitAddress`/
  `Table.Address()` 是配套工具。
- Zstandard 块压缩（默认 256 KiB 目标块）；按快照、表和行随机访问，历史快照不可变。
- 多读单写：读并发、写串行，提交原子可见；Close 等待在途读取。
- 校验与崩溃恢复不依赖独立 WAL：未提交尾部打开时截断；单个 IndexTxn 损坏时从该快照自身的数据块
  在内存重建索引，后续快照照常重放。
- Schema 与源数据库设计元信息分层：`DefineTable` 把 RowPack 自身的 Canonical Schema 写成引擎
  自产自销的 Table/Column 记录（内部 TLV），只服务行编码/解码，不按源库方言建模。源库原始元
  信息属于上层 Source Metadata，**载体是普通行数据**——用 `DefineTableIn` 在自选 ns 建目录表
  存放，TLV 不承载它，也不新增通用元数据 API（见 docs/SOURCE_CATALOG_GUIDE_V1.md）。

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

	tx, err := db.Begin(ctx, rowpack.NoParent)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback()

	if err := tx.DefineTable("users", []rowpack.Column{
		{Name: "id", Type: rowpack.TypeUint64},
		{Name: "name", Type: rowpack.TypeString},
		{Name: "created_at", Type: rowpack.TypeDateTime},
	}); err != nil {
		log.Fatal(err)
	}
	if err := tx.Insert("users", 1001, rowpack.Row{
		rowpack.Uint64(1001),
		rowpack.String("张三"),
		rowpack.DateTime(time.Now()),
	}); err != nil {
		log.Fatal(err)
	}
	full, err := tx.Commit(ctx)
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
	d, err := db.Begin(ctx, full)
	if err != nil {
		log.Fatal(err)
	}
	_ = d.Update("users", 1001, rowpack.Row{
		rowpack.Uint64(1001),
		rowpack.String("张三 (更新)"),
		rowpack.DateTime(time.Now()),
	})
	_ = d.Delete("users", 1002)
	_, err = d.Commit(ctx)
	if err != nil {
		log.Fatal(err)
	}
}
```

批量写入按顺序消费且不持有调用方的 slice，返回后可立即复用缓冲：

```go
changes := []rowpack.Change{
	{Type: rowpack.Update, Table: "users", RowID: 1001, Row: updated},
	{Type: rowpack.Delete, Table: "users", RowID: 1002},
}
if err := d.ApplyBatch(changes); err != nil {
	log.Fatal(err)
}
changes = changes[:0]
```

## 关键概念

Store（单个 `.rpk` 及其运行时状态）、Snapshot（不可变、原子提交的行变更集，FULL/DELTA 成父子
链）、RowID（表内稳定逻辑行标识，独立于业务主键）、Block（属于一个快照和一个表的压缩/校验
单位）、TypedTuple（按 Schema 顺序编码的行负载，NULL 用位图）。**NS** 是表的归属命名空间（默认
`user`，记在 Table 记录的 `NS` 字段、默认省略不占字节）：表身份是 `(NS, Name)`，故收表 API 用
**地址**寻址（默认 ns 裸名 `"users"`，其他 ns 加前缀 `"public.users"`）。完整定义见
[docs/REQUIREMENTS.md](docs/REQUIREMENTS.md) §4。

## 行复用

读入口统一为借用/复用模式，消除每行的 Row 切片、payload 复制与 Decimal `big.Int` 分配：
`Iterator.Next()` 无参数，解码进内部缓冲并跨调用复用，返回的 Row 仅在下一次 Next 前有效（100k
行 × 7 列整表 Scan 仅 ~135 次分配，String/Bytes 为零复制 arena 视图）；`Get(ctx, snap, table,
id, dst)` 解码进调用者提供的 Row（nil dst 每次分配新行；缓存热读 1 alloc/16 B）。需要跨调用
保留的值须拷贝；`String()`/`Bytes()`/`Decimal()` 访问器始终返回副本；dst 槽自身缓冲会被复用，
保留的 Value 可能在下一次解码进同一 dst 后失效（`String` 因不可变始终复制）。完整所有权规则见
[docs/GO_API_DESIGN_V1.md](docs/GO_API_DESIGN_V1.md) §6。

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

`ReadBatch` 按块聚合：每个块至多加载、解密、解压和校验一次，不管请求中有多少行落在该块里。语义
与逐行 `Get` 一致：所有 id 必须在该快照可见（缺失或已删除整批返回 `ErrNotFound`）；返回顺序与
输入 ids 一一对应（重复输入重复返回）；返回的行归调用方所有、互不别名。

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

热读千行批量 `ReadBatch` 与逐行 Get 延迟相当、分配少 ~250×（254.6 µs · 4 allocs vs 235.9 µs ·
1,000 allocs）；收益来自物理层：同一块只加载/解密/解压/校验一次，用 `Stats().Batch`
（Blocks << len(ids)）与 `Read` 放大指标量化。`make bench-batch` 复现该对比（冷档数值未归档）。

## 文档

- [需求规格](docs/REQUIREMENTS.md)
- [二进制格式（v1 单文件）](docs/BINARY_FORMAT_V1.md)
- [Go API 设计](docs/GO_API_DESIGN_V1.md)
- [元数据格式（TLV）](docs/METADATA_FORMAT_V1.md)
- [IndexTxn 格式](docs/INDEX_TXN_FORMAT_V1.md)
- [数据块加密](docs/ENCRYPTION_V1.md)
- [源库 Key Range 映射](docs/SOURCE_KEY_RANGE_MAPPING.md)
- [性能基线（v1）](docs/PERFORMANCE_BASELINE_V1.md)
- [文件结构查看器（HTML）](docs/file-explorer.html)

## 命令

```sh
make test # go test ./...
make race # go test -race ./...
make vet # go vet ./...
make staticcheck # staticcheck ./...
make bench # 统一基线套件（Env/矩阵/延迟 + 直读档），输出 bench/results.txt
make bench-quick # 快速档：20k 行 + 3 次迭代全矩阵冒烟（~15s），输出 bench/results-quick.txt
make bench-1m # 1M 行档：scan1m / getrand1m
make bench-batch # 批量读对比：逐行 Get 基线 vs ReadBatch（10s 每场景）
make baseline # 归档性能基线 → testdata/baseline/<date>.txt（带统一环境标注）
make baseline-diff OLD=2026-09-10 NEW=2026-09-11 # 对比两个基线，>10% 回退退出码 1
make golden # 重新生成 golden files（格式变更时人工审查）
```

### 只读检查工具

```sh
go run ./cmd/rowpack-inspect header <base>            # 文件头 + 统计（含 recovery 报告）
go run ./cmd/rowpack-inspect list <base>             # 快照与表（含表地址 address=…）
go run ./cmd/rowpack-inspect verify <base>           # VerifyFull，失败退出码 1
go run ./cmd/rowpack-inspect dump <base> <snap> <tableID>  # 按表 ID dump 可见行（跨 ns 表同样可读）
```

命令实现放在 `internal/inspect`（`Run(ctx, argv, stdout, stderr)`），`cmd/` 只做退出码映射，
因此四条命令与全部参数错误分支都有测试。退出码：0 成功、1 store 错误、2 用法错误。

## 参考基准

统一套件由 `make bench` 复现（`BenchmarkEnv` + 64 格 `BenchmarkMainMatrix` + `BenchmarkLatency`
延迟分位 + 直读档），完整结果落 `bench/results.txt`（机器相关，gitignore）；`make baseline` 把带
环境标注的输出归档到 `testdata/baseline/<日期>.txt`。基线环境 Go 1.27 / darwin/arm64 /
klauspost zstd v1.20 / BlockSize 256 KiB / Zstd / SyncCommit，数据集 100k 行 × 7 列；下表取自
2026-09-10 归档基线（[PERFORMANCE_BASELINE_V1.md](docs/PERFORMANCE_BASELINE_V1.md) §2），单位保留
原始的 krows/s · kget/s · ns/µs；数值随磁盘与 CPU 变化，仅作相对参考。

| 基准（256K/mmap/sync 档） | 结果 |
| --- | --- |
| FULL 顺序写（sync / async） | 90.2 / 86.2 ms · 1109 / 1160 krows/s |
| 同构行写（sync） | 46.5 ms · 2149 krows/s（压缩比 0.00639）|
| Get 热读（矩阵，复用 dst） | 363.8 ns · 2748 kget/s · 1 alloc · 16 B |
| Get 冷读（矩阵） | 49.75 µs · 20.10 kget/s · 14 allocs |
| Scan 100k 热 / 冷 | 14.35 / 25.16 ms · 6970 / 3974 krows/s（63 / 1606 allocs）|
| ReadBatch(1000) / Get ×1000 | 254.6 µs · 4 allocs / 235.9 µs · 1000 allocs |
| Open 重放 / IndexTxn 重建 | 2.36 ms / 17.59 ms |

完整表格（冷读、并发、深链、1M 档、延迟分位数、加密档）及解读见
[docs/PERFORMANCE_BASELINE_V1.md](docs/PERFORMANCE_BASELINE_V1.md)。

## 兼容性

- v1（单文件，Magic `ROWPACK1`）是当前且唯一的格式线；早期双文件草案从未发布。
- golden files 纳入 CI：任何字节级变化视为格式变更。
- 加密 store 使用 AES-256-GCM；Rows Page / Metadata Block / IndexTxn 分域加密封装（nonce 位域
  互斥），Footer 绑定落盘字节 CRC，无需密钥即可检出撕裂。
- 支持 Linux / macOS / Windows amd64/arm64（跨进程写锁在无 flock 平台明确报错）。
