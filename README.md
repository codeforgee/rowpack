# RowPack

[![CI](https://github.com/codeforgee/rowpack/actions/workflows/ci.yml/badge.svg)](https://github.com/codeforgee/rowpack/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)

RowPack 是一个用 Go 实现的**单文件嵌入式二维表存储引擎**，面向备份、快照和本地分析等
「顺序写入、随机读取」的工作负载。整库——数据、索引、Schema、快照历史——就是**一个
`.rpk` 文件**：备份、迁移、复制即拷贝单个文件。

- **零部署**：无进程、无端口，运行时仅依赖 zstd，`go get` 即用。
- **快照即版本**：FULL / DELTA 快照组成父子链，历史快照不可变，可按快照随机读任意行。
- **崩溃安全无需 WAL**：提交由 SnapshotFooter 一次性原子落盘；打开时截断未提交尾部，
  单个索引损坏可在内存中从数据块重建。

## 特性

| 特性 | 说明 |
| --- | --- |
| **单文件格式** | `<base>.rpk` 承载全部数据与索引，Footer 原子提交（一次 fsync） |
| **快照模型** | FULL 基线 + DELTA 增量；INSERT / UPDATE / DELETE；任意时刻可提交新 checkpoint |
| **表寻址** | 表身份是 `(NS, Name)`，同名表可跨 ns 共存 |
| **随机读** | `Get` / `ReadBatch` / `Scan` 按快照、表、RowID 访问，历史快照照常可读 |
| **块压缩** | Zstandard，默认 256 KiB 目标块 |
| **静态加密** | AES-256-GCM 分域封装，密钥由 KeyProvider 注入，支持轮换 |
| **并发模型** | 多读单写，提交原子可见；`Close` 等待在途读取 |
| **完整性与恢复** | 全链路 CRC + AEAD；无 WAL，索引损坏可从数据块内存重建 |
| **行复用** | 借用/复用语义：整表 Scan 100k×7 列仅约 135 次分配 |

## 安装

```sh
go get github.com/codeforgee/rowpack   # Go 1.25+；Linux / macOS / Windows，amd64 / arm64
```

## 快速开始

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/codeforgee/rowpack"
)

func main() {
	ctx := context.Background()

	// basePath 不带扩展名，实际文件是 /data/users-backup.rpk
	db, err := rowpack.Create("/data/users-backup", rowpack.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// FULL 基线快照
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
	for _, u := range []struct {
		id   rowpack.RowID
		name string
	}{{1001, "张三"}, {1002, "李四"}} {
		if err := tx.Insert("users", u.id, rowpack.Row{
			rowpack.Uint64(u.id),
			rowpack.String(u.name),
			rowpack.DateTime(time.Now()),
		}); err != nil {
			log.Fatal(err)
		}
	}
	full, err := tx.Commit(ctx) // 返回快照 ID，是读路径的唯一凭证
	if err != nil {
		log.Fatal(err)
	}

	row, err := db.Get(ctx, full, "users", 1001, nil)
	if err != nil {
		log.Fatal(err)
	}
	name, _ := row[1].String()
	fmt.Println("name:", name) // 张三

	// DELTA 增量快照（也可传 rowpack.Latest 自动取最新）
	d, err := db.Begin(ctx, full)
	if err != nil {
		log.Fatal(err)
	}
	_ = d.Update("users", 1001, rowpack.Row{
		rowpack.Uint64(1001),
		rowpack.String("张三 (已改名)"),
		rowpack.DateTime(time.Now()),
	})
	_ = d.Delete("users", 1002)
	delta, err := d.Commit(ctx)
	if err != nil {
		log.Fatal(err)
	}
	_ = delta // full 与 delta 都可随时读：历史快照不可变
}
```

批量写入按顺序消费且不持有调用方的 slice，返回后可立即复用缓冲：

```go
changes := []rowpack.Change{
	{Type: rowpack.Insert, Table: "users", RowID: 1003, Row: row3},
	{Type: rowpack.Delete, Table: "users", RowID: 1002},
}
if err := tx.ApplyBatch(changes); err != nil {
	log.Fatal(err)
}
changes = changes[:0]
```

## 核心概念

| 概念 | 说明 |
| --- | --- |
| **Store** | 一个 `.rpk` 文件及其运行时状态；`Create`/`Open` 打开，写路径持跨进程锁 |
| **Snapshot** | 不可变、原子提交的变更集；FULL 为基线，DELTA 基于父快照，构成父子链 |
| **RowID** | 表内稳定逻辑行标识（`uint64`，非 0），独立于业务主键 |
| **Block** | 属于一个快照、一张表的压缩/校验单位 |
| **TypedTuple** | 按 Schema 顺序编码的行负载，NULL 用位图表示 |
| **NS / 地址** | 表归属命名空间（默认 `user`）：默认 ns 用裸名 `"users"`，其他 ns 加前缀 `"public.users"` |

支持 17 种列类型：`bool`、`int8`–`int64`、`uint8`–`uint64`、`float32/64`、`string`、`bytes`、
`date`、`time`、`datetime`、`decimal`。

## API 导览

```go
// —— 写入（同一时刻仅一个活跃 Writer）——
db.Begin(ctx, parent)              // NoParent → FULL；快照 ID 或 Latest → DELTA
tx.DefineTable(name, cols)         // 默认 ns；DefineTableIn 可指定 ns
tx.Insert / tx.Update / tx.Delete  // 流式逐行
tx.ApplyBatch([]Change)            // 批量，按顺序消费
tx.SetMeta([]byte)                 // 每快照一个 opaque 元信息块（可选）
tx.Commit(ctx) → SnapshotID        // 原子发布，读者立即可见
tx.Rollback()                      // Begin 后即可 defer；已提交则返回 ErrSnapshotCommitted

// —— 读取（以快照 ID 为凭证，沿父链解析）——
db.Get(ctx, snap, table, id, dst)  // 单行；dst 可复用（nil 每次分配）
db.Exists(ctx, snap, table, id)    // 只查索引/墓碑，不读块
db.ReadBatch(ctx, snap, table, ids)
db.Scan(ctx, snap, table, ScanOptions{Start, End})
db.Tables / TablesIn / Schema / ListSnapshots / Meta

// —— 运维与诊断 ——
db.Blocks(ctx, snap, table)             // 块清单：主键范围 + 字节数，零块 I/O
db.ScanBlocks(ctx, snap, table, lo, hi) // 快照自身的原始变更流
db.Verify(ctx, VerifyQuick | VerifyFull)
db.Stats()                              // 缓存 / 读放大 / 批量聚合 / 恢复统计
```

## 进阶

### 批量读取

`ReadBatch` 按块聚合：每个块至多加载、解密、解压、校验一次，不管请求中有多少行落在该块里。
语义与逐行 `Get` 一致，返回顺序与输入 ids 一一对应；聚合效果用 `db.Stats().Batch`
（`Blocks << len(ids)`）量化，热读千行的分配数约为逐行 `Get` 的 1/250。

```go
rows, err := db.ReadBatch(ctx, full, "users", []rowpack.RowID{1001, 1002})
```

### 行复用

读入口统一为借用/复用模式，消除每行的 Row 切片与 payload 复制：

- `Iterator.Next()` 解码进内部缓冲并跨调用复用，返回的 Row 仅在下一次 `Next` 前有效。
- `Get(..., dst)` 解码进调用者提供的 dst；把返回值作为下一次的 dst 可持续复用缓冲。
- `String()` / `Bytes()` / `Decimal()` 访问器始终返回副本，读取永远安全；跨调用保留裸
  Value 须自行拷贝。

完整所有权规则见 [docs/GO_API_DESIGN_V1.md](docs/GO_API_DESIGN_V1.md) §6。

### 静态加密

加密在 `Create` 时一次性固定：AES-256-GCM，Rows Page / Metadata Block / IndexTxn 三域分域
封装，Footer 绑定落盘字节 CRC，无需密钥即可检出撕裂。密钥从不落盘：引擎只持久化 KeyID，
原始密钥由调用方通过 `KeyProvider` 按 ID 与 epoch 注入，支持轮换。

```go
db, err := rowpack.Create(base, rowpack.Options{
	Encryption: &rowpack.EncryptionConfig{
		KeyProvider: myProvider, // Key(ctx, keyID, epoch) ([]byte, error)
		KeyID:       "key-1",
	},
})
```

加密 store 打开时必须提供 Provider，否则返回 `ErrKeyRequired`。详见
[docs/ENCRYPTION_V1.md](docs/ENCRYPTION_V1.md)。

### 崩溃恢复与校验

- 提交的原子性由 SnapshotFooter 保证；打开时自动截断未提交尾部。
- 单个 IndexTxn 损坏时，从该快照自身的数据块在内存重建索引，后续快照照常重放。
- 提交失败且结局未知时（`CommitError.Unknown`），store 以 `ErrMustReopen` 拒绝新写入，
  重新打开后由恢复流程对齐视图；读不受影响。
- `Verify` Quick 档校验头、索引、Footer 与父链；Full 档额外解压每个块并逐行验证。

## 命令行工具

```sh
go run ./cmd/rowpack-inspect header <base>                  # 文件头 + 统计（含 recovery 报告）
go run ./cmd/rowpack-inspect list <base>                    # 快照与表
go run ./cmd/rowpack-inspect verify <base>                  # VerifyFull，失败退出码 1
go run ./cmd/rowpack-inspect dump <base> <snap> <tableID>   # 按表 ID dump 可见行
```

退出码：0 成功、1 store 错误、2 用法错误。

## 开发

```sh
make test         # go test ./...
make race         # go test -race ./...
make lint         # golangci-lint（CI 同时跑 gofmt / go vet / staticcheck）
make bench        # 统一基线套件 → bench/results.txt
make bench-quick  # 快速档：20k 行冒烟（~15s）
make baseline     # 归档性能基线 → testdata/baseline/<date>.txt
make golden       # 重新生成 golden files
```

golden files 纳入 CI，任何字节级漂移视为格式变更；CI 在 Linux / macOS / Windows 三平台运行
单元测试、竞态检测与 fuzz 冒烟。

## 性能参考

基线环境与完整口径见
[docs/PERFORMANCE_BASELINE_V1.md](docs/PERFORMANCE_BASELINE_V1.md)；数值随磁盘与 CPU 变化，
仅作相对参考，可由 `make bench` 复现（100k 行 × 7 列，256 KiB 块 / Zstd / SyncCommit）。

| 基准 | 结果 |
| --- | --- |
| FULL 顺序写 | 1109 krows/s |
| 同构行写（压缩比 0.00639） | 2149 krows/s |
| Get 热读（复用 dst） | 363.8 ns · 1 alloc · 16 B |
| Get 冷读 | 49.75 µs · 14 allocs |
| Scan 100k×7（热） | 6970 krows/s · 63 allocs |
| ReadBatch(1000) vs Get×1000 | 4 vs 1000 allocs，延迟相当 |
| Open 重放 | 2.36 ms |

## 文档

| 文档 | 内容 |
| --- | --- |
| [REQUIREMENTS.md](docs/REQUIREMENTS.md) | 需求规格与核心概念定义 |
| [BINARY_FORMAT_V1.md](docs/BINARY_FORMAT_V1.md) | v1 单文件二进制格式 |
| [GO_API_DESIGN_V1.md](docs/GO_API_DESIGN_V1.md) | Go API 设计与所有权规则 |
| [METADATA_FORMAT_V1.md](docs/METADATA_FORMAT_V1.md) | 元数据 TLV 格式 |
| [INDEX_TXN_FORMAT_V1.md](docs/INDEX_TXN_FORMAT_V1.md) | IndexTxn（快照索引）格式 |
| [ENCRYPTION_V1.md](docs/ENCRYPTION_V1.md) | 数据块加密 |
| [PERFORMANCE_BASELINE_V1.md](docs/PERFORMANCE_BASELINE_V1.md) | 性能基线与解读 |
| [file-explorer.html](docs/file-explorer.html) | 文件结构查看器（HTML） |

## 兼容性

- **v1（单文件，Magic `ROWPACK1`）是当前且唯一的格式线**；未知 major 版本拒绝打开，更高
  minor 版本仅在所有必需 feature bits 均可识别时打开。
- Schema 由引擎自产自销（内部 TLV Table/Column 记录），只服务行编码/解码；上层自有元信息
  可用 `DefineTableIn` 建目录表存放，引擎不解释其语义。
- 跨进程写锁依赖 `flock`，无 flock 平台明确报错而非静默降级。
- 错误不做字符串匹配：公开 API 均返回支持 `errors.Is` 的 sentinel / 结构化错误
  （`CorruptionError` 携带文件、偏移与快照/表/块 ID）。
