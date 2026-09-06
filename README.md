# RowPack

RowPack 是一个使用 Go 实现的轻量级嵌入式二维表存储引擎，面向备份、快照、
差异归档和本地分析等「顺序写入、随机读取」场景。

- 双文件格式：`<base>.rpk`（数据，append-only，提交权威）+ `<base>.rpi`（索引，可重建）。
- 支持 FULL / DELTA 快照以及 INSERT / UPDATE / DELETE 变更。
- Zstandard 块压缩（默认 256 KiB 目标块）。
- 按快照、表和行随机访问，历史快照不可变、不受后续提交影响。
- 多读单写：读操作无锁并发，写操作单写者串行，提交原子可见。
- 校验、崩溃恢复与索引重建，不依赖独立 WAL。
- 元数据作为普通数据存储：通用 TLV 记录通道（保序、无损往返、未知字段透传），
  引擎不内建数据库强类型语义；行解码所需的最小 Schema 契约由 `DefineSchema` 提供。

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
	db, err := rowpack.Create("/data/users-backup", rowpack.DefaultOptions())
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

	row, err := db.Get(ctx, full.ID, 1, 1001)
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

## 行复用（v1.1）

高吞吐批量读取可用 v1.1 新增的借用/复用入口：调用者提供目标 Row，引擎复用
其存储，消除每行的 Row 切片与 Decimal big.Int 分配（Scan 场景每行分配降
~86%）。`GetInto` / `Iterator.NextInto` 的返回值别名调用者自己的 dst，下
一次调用覆盖其内容；通过 `String()`/`Bytes()`/`Decimal()` 访问器读值始终
安全（返回副本）。默认的 `Get` / `Iterator.Row` 语义不变（每次返回独立、
归调用者所有的行）。

```go
it, _ := db.Scan(ctx, full.ID, 1, rowpack.ScanOptions{})
defer it.Close()
var dst rowpack.Row
for {
	row, ok := it.NextInto(dst)
	if !ok {
		break
	}
	dst = row
	name, _ := row[1].String()
	_ = name
}
```

## 文档

- [需求规格](REQUIREMENTS.md)
- [二进制格式 v1](BINARY_FORMAT_V1.md)
- [元数据格式 v1](METADATA_FORMAT_V1.md)
- [Go API 设计](GO_API_DESIGN.md)
- [开发计划](DEVELOPMENT_PLAN.md)
- [v1.1 优化计划](docs/plan-v11.md)
- [ADR-001：.rpk 是提交权威](docs/adr/ADR-001.md)
- [ADR-002：强类型 API 与通用 TLV 边界](docs/adr/ADR-002.md)

## 命令

```sh
make test        # go test ./...
make race        # go test -race ./...
make fuzz-short  # 每个 fuzz 目标 5s
make golden      # 重新生成 golden files（格式变更时人工审查）
```

## 参考基准

环境：Go 1.27 / darwin/arm64 / klauspost zstd v1.20 / BlockSize 256 KiB / Zstd / SyncCommit，
数据集 100k 行 × 7 列。数值随磁盘与 CPU 变化，仅作相对参考。

| 基准 | 结果 |
| --- | --- |
| FULL 顺序写 | ~360 krows/s, ~72 MB/s（allocs 较 v1.0 -16%） |
| Get 冷读（缓存关闭） | ~300 µs/op |
| Get 热读（缓存命中） | ~8 µs/op（GetInto 复用 ~7 µs / 2 allocs） |
| 并发 Get 1/8 goroutine | ~195 µs/op（读路径无锁） |
| Scan 100k 行 | ~47 ms（ScanInto 复用 ~37 ms / 541 krows/s / -86% allocs） |
| Open 索引重放（100k 行） | ~9 ms |
| RebuildIndex（100k 行） | ~92 ms |

## 兼容性

- v1 磁盘格式冻结：golden files 纳入 CI，任何字节级变化视为格式变更。
- 枚举值、字段编号、golden 样本发布后不得修改。
- 支持 Linux / macOS / Windows amd64/arm64（跨进程写锁在无 flock 平台明确报错）。
