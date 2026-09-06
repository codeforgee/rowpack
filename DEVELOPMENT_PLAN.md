# RowPack v1.0 开发计划

> 文档状态：执行基线  
> 目标版本：v1.0  
> 更新时间：2026-09-06  
> 需求基线：[REQUIREMENTS.md](REQUIREMENTS.md)  
> 格式基线：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md)  
> 元数据基线：[METADATA_FORMAT_V1.md](METADATA_FORMAT_V1.md)  
> API 基线：[GO_API_DESIGN.md](GO_API_DESIGN.md)

## 1. 目标

本计划用于把 RowPack v1.0 从设计推进到可发布的 Go 模块。最终交付物必须支持：

- `.rpk` 数据文件和 `.rpi` 索引文件。
- FULL、DELTA 快照及 INSERT、UPDATE、DELETE。
- 通用元数据记录（TLV）无损存储与透传；元数据即引擎中的数据。
- Zstd Block 压缩和 None 模式。
- 并发随机读、单写者和一致的快照可见性。
- TypedTuple 行编码及全部 v1 数据类型。
- Scan、Block Cache、校验与崩溃恢复。
- 元数据作为普通数据无损读写，引擎不解释其语义。
- Golden files、Fuzz、race、故障注入和基准测试。

## 2. 实施原则

1. 先锁定底层字节格式，再实现高层 API。
2. 每个阶段必须通过对应质量门槛，不能把格式和恢复问题留到最后。
3. 元数据作为引擎存储的普通数据（通用 TLV），引擎不内建强类型语义；行解码的最小 Schema 契约由 `DefineSchema` 提供。
4. 优先交付垂直闭环：先完成最小 FULL 写入和重开读取，再扩展 DELTA、缓存和恢复。
5. 文件解析默认将输入视为不可信数据；任何长度都先校验再分配。
6. 已发布的 v1 枚举值、字段编号和 golden files 不得无版本变更地修改。

## 3. 建议目录结构

```text
RowPack/
├── go.mod
├── rowpack.go                 // Store、Create、Open、公共入口
├── options.go
├── types.go
├── values.go
├── errors.go
├── snapshot.go
├── iterator.go
├── verify.go
├── stats.go
├── internal/
│   ├── fileformat/            // 固定磁盘结构
│   ├── codec/                 // TypedTuple
│   ├── metadata/              // Metadata TLV 与核心字段
│   ├── block/                 // Block 构建/读取/压缩
│   ├── index/                 // IndexTxn 与内存视图
│   ├── cache/                 // 并发 LRU
│   ├── recovery/              // 打开和恢复状态机
│   └── fault/                 // 测试故障注入
├── testdata/
│   ├── golden/
│   ├── corrupt/
│   └── recovery/
├── cmd/rowpack-inspect/       // 可选调试工具
└── docs/
```

## 4. 里程碑总览

| 里程碑 | 主要结果 | 预计工作量 |
| --- | --- | ---: |
| M0 | 工程骨架与规范冻结 | 2–3 人日 |
| M1 | 固定结构、CRC 与文件头 | 4–6 人日 |
| M2 | Metadata 和 TypedTuple 编码 | 7–10 人日 |
| M3 | Block、压缩与基础文件 I/O | 6–8 人日 |
| M4 | 索引事务与不可变索引视图 | 7–10 人日 |
| M5 | FULL 快照端到端闭环 | 6–8 人日 |
| M6 | DELTA、历史读取与 Scan | 8–12 人日 |
| M7 | Cache、并发与资源生命周期 | 6–9 人日 |
| M8 | 崩溃恢复、校验与索引重建 | 9–13 人日 |
| ~~M9~~ | 元数据适配层（引擎之外，2026-09-06 决策移出引擎核心） | — |
| M10 | 性能、兼容性与 v1.0 发布 | 6–10 人日 |

单人串行预计 66–97 人日。该估算包括测试和文档，不包括未知数据库方言的类型映射补齐。多人开发时可并行部分 codec、cache、工具和适配工作，但 M1–M5 的主路径应保持单一格式负责人审核。

## 5. M0：工程骨架与规范冻结

### 5.1 任务

- 初始化 Go module，确定最低 Go 版本。
- 建立目录、包边界和 lint/test 命令。
- 将所有磁盘常量集中到 `internal/fileformat/constants.go`。
- 将 Type、RecordType、FieldID、Feature Bit 编号生成或固定为常量。
- 增加规范一致性测试，检查声明的固定结构大小。
- 记录 Zstd 库选择及版本策略。
- 建立 CI：format、vet、unit、race、fuzz smoke test。

### 5.2 交付物

- 可执行 `go test ./...` 的工程骨架。
- `format_version.go` 和所有枚举常量。
- `testdata/golden` 目录及生成策略。
- ADR-001：为什么 `.rpk` 是提交权威、`.rpi` 可重建。
- ADR-002：元数据是引擎存储的普通数据，引擎不内建强类型语义。

### 5.3 完成标准

- CI 在 Linux、macOS 上运行。
- 所有固定编号有测试保护。
- 设计文档中不存在未决的 v1 固定字节字段。

## 6. M1：固定结构、CRC 与文件头

### 6.1 任务

- 实现 Little Endian 手写编码/解码辅助函数。
- 实现 CRC32C 计算和“CRC 字段视为零”的统一函数。
- 实现 DataFileHeader、IndexFileHeader。
- 实现 SnapshotHeader、SnapshotFooter、BlockHeader。
- 实现 IndexTxnHeader、四类 IndexEntry、IndexTxnFooter。
- 实现 `align8`、Padding 写入和验证。
- 为每个结构增加 round-trip、短输入、错误 Magic、错误 CRC、未知版本测试。
- Fuzz 所有固定结构 Decoder。

### 6.2 实现约束

- 每个结构提供 `MarshalTo([]byte)` 和 `Unmarshal([]byte)` 或等价无反射实现。
- Decoder 不持有输入切片，不发生越界 panic。
- 禁止使用 `binary.Write(struct)`，避免 padding 和字段顺序变化。
- 错误必须携带文件类型、offset 和结构名称。

### 6.3 完成标准

- 固定结构 round-trip 覆盖率达到 100% 字段覆盖。
- Fuzz 任意字节至少运行 1 分钟无 panic。
- 生成并锁定“空 Store”golden files。

## 7. M2：Metadata 和 TypedTuple 编码

### 7.1 Metadata TLV

- 实现 MetadataPayloadHeader 和 Directory。
- 实现 MetadataRecord Envelope。
- 实现所有 WireType、规范排序和嵌套深度限制。
- 实现未知非 Critical 字段无损保留。
- 实现未知 Critical 字段拒绝逻辑。
- 固定 13 种核心 RecordType 及其 FieldID。
- 字符串字段原样保存（不做规范化）。
- 实现 ObjectID 分配器和稳定 ExternalKey 映射。

### 7.2 核心元数据

- 实现 Header、Table、Column、PrimaryKey、Index、UniqueKey、ForeignKey、AutoInc、TableComment、ColComment、View、Function、VirtualColumn 的内部模型。
- Go `int` 与磁盘 i64 转换必须检查溢出。
- 保留每个列表的原始顺序。
- 复合主键/外键逐列记录，不聚合丢失 KeySeq 或对应关系。
- `DataDefault`、`ViewText`、`FuncText` 原文往返一致。

### 7.3 TypedTuple

- 实现 Null Bitmap。
- 实现全部定宽整数、浮点数、String、Bytes、Date、Time、DateTime、Decimal。
- Decimal 实现唯一规范编码。
- 实现 Schema 派生接口：根据 Header 方言和 Column 属性得到逻辑 Type。
- 类型派生由接口抽象，核心提供显式 Schema 路径；各数据库方言映射作为可注册组件补齐。
- Fuzz 行 Decoder 和 Metadata Decoder。

### 7.4 完成标准

- 每个核心元数据字段均有非零、零值、空值和 Unicode round-trip 测试。
- 字符串字段的空字符串、带空白原文往返一致。
- 所有 TypedTuple 类型具有边界值测试。
- 未知扩展记录读入再写出后字节完全一致。

## 8. M3：Block、压缩与基础文件 I/O

### 8.1 任务

- 实现 Rows Block Builder 和 Metadata Block Builder。
- 按 256 KiB 目标原始大小自动 flush。
- 实现超过目标大小的单行独立 Block。
- 实现 None 和 Zstd Compressor。
- 实现 RawSize、StoredSize、RawCRC 和 RowCRC 校验。
- 实现 Block Reader：ReadAt、限额解压、Directory 定位。
- 实现追加写 abstraction，自行维护 writeOffset，不使用共享 Seek 游标。
- 实现文件创建的双文件回滚策略。

### 8.2 完成标准

- 同一输入在同一格式选项下生成确定性未压缩 Payload。
- None/Zstd 解码得到相同行和元数据。
- 压缩炸弹、错误 RawSize 和超限记录被安全拒绝。
- 超大单行测试通过。

## 9. M4：索引事务与不可变视图

### 9.1 任务

- 实现每 Snapshot 一个 IndexTxn 的写入。
- 实现 Snapshot、Metadata、Block、Row IndexEntry。
- 实现事务 Body CRC 和数据 Footer 交叉校验。
- 实现 `.rpi` 顺序重放。
- 构建不可变 `indexView`：snapshot、block、metadata、row 和派生 schema 索引。
- 使用排序切片或紧凑 map 保存增量 Row Index。
- 统计 IndexMemoryBytes。
- 实现父链校验、深度限制和环检测。

### 9.2 完成标准

- 任意截断位置的 IndexTxn 不被部分重放。
- 数据 Footer 不匹配时整个事务无效。
- 发布后的 view 无原地 map/slice 修改。
- 通过索引可定位任意 Metadata Record、Block 和 Row Directory Entry。

## 10. M5：FULL 快照端到端闭环

### 10.1 任务

- 实现 `DefaultOptions`、Create、Open、Close。
- 实现 SnapshotWriter 状态机。
- 实现 DefineSchema、PutMetadata、Insert、Apply、Commit、Abort。
- 实现 SyncCommit 和 AsyncCommit。
- 实现 CommitError.Unknown。
- 实现 Get、Exists、Snapshot、LatestSnapshot、ListSnapshots。
- 实现只读 Open。
- 提交时原子发布新 indexView。

### 10.2 垂直验收场景

1. 创建 Store。
2. 写 Header、Table、Column、PK 元数据。
3. 写至少两个 Rows Block。
4. Commit FULL。
5. Close/Reopen。
6. 读取元数据和随机行并逐值比对。

### 10.3 完成标准

- 满足需求 AC-001、AC-003、AC-006、AC-011、AC-012 的 FULL 部分。
- Commit 前数据不可见，Commit 后整体可见。
- SyncCommit 返回成功后 kill/reopen 不丢失。

## 11. M6：DELTA、历史读取与 Scan

### 11.1 任务

- 实现 DELTA Begin 和 Parent 校验。
- 实现 Update、Delete Tombstone。
- Strict 模式校验父视图存在性。
- 实现 Get 沿父链解析。
- 实现元数据 Revision 和 DELETE 沿父链解析。
- 实现 Scan Iterator。
- 使用父链排序增量索引 k-way merge，过滤覆盖和 Tombstone。
- 实现 StartRowID/EndRowID 范围。
- 实现空 DELTA。

### 11.2 完成标准

- 满足 AC-002 和 AC-007 的逻辑部分。
- 任意历史 Snapshot 读取不受后续提交影响。
- Scan RowID 严格升序，不返回旧版本和 Tombstone。
- Scan 额外内存不与逻辑表总行数线性增长。

## 12. M7：Cache、并发与生命周期

### 12.1 任务

- 实现按字节容量限制的线程安全 LRU。
- 实现并发 miss 合并。
- 大于 Cache 容量的 Block 可读但不缓存。
- CRC 失败内容不得进入 Cache。
- Store 使用 atomic immutable view。
- 实现活动操作登记、Close 等待和拒绝新操作。
- 实现同进程及跨进程单 writer lock。
- 补齐 Stats 原子计数。

### 12.2 完成标准

- `go test -race ./...` 通过。
- 至少 32 个 goroutine 并发 Get/Scan，同时提交 Snapshot，无死锁和部分可见。
- 同一 Block 并发冷读只触发一次受控加载。
- Cache 淘汰不破坏正在进行的读取。
- 满足 AC-004、AC-005、AC-012。

## 13. M8：崩溃恢复、校验与重建

### 13.1 故障注入点

至少在以下位置注入确定性错误或模拟退出：

1. Data SnapshotHeader 前后。
2. BlockHeader、Payload、Padding 写入中。
3. SnapshotFooter 写入前后。
4. `.rpk` sync 前后。
5. IndexTxnHeader、Body、Footer 写入中。
6. `.rpi` sync 前后。
7. 内存 view 发布前后。

### 13.2 任务

- 实现快速打开校验。
- 实现数据领先索引时的尾部扫描和索引补写。
- 实现索引领先/损坏尾部的忽略或截断。
- 实现数据无 Footer 尾部的忽略或截断。
- 区分尾部截断和中间损坏。
- 实现 VerifyQuick、VerifyFull。
- 实现 RebuildIndex：临时文件、sync、原子 rename。
- 实现 RecoveryStats。

### 13.3 完成标准

- 每个故障点恢复结果确定且幂等。
- 只读模式从不修改文件。
- 中间损坏明确失败，不跳过。
- 满足 AC-007、AC-008、AC-009、AC-010。

## 14. M9：元数据适配层（引擎之外，不冻结）

> 2026-09-06 决策修订（ADR-002）：引擎不内建强类型元数据模型、不猜方言类型。
> 元数据通过通用 TLV 记录通道作为普通数据存储；13 类强类型模型与方言映射
> 属于上层数据库适配层，按真实 fixture 设计，不冻结进引擎核心 API。

### 14.1 任务（如实施，放在引擎之外的独立包）

- 按 `meta.Store` 列表结构组织 13 类强类型模型（字段编号参考 METADATA_FORMAT_V1.md §6/§7）。
- 实现数据库方言到 TypedTuple Type 的映射（引擎不内建；行解码只认 `DefineSchema` 的规范类型字符串）。
- 字符串字段保存原文并保持列表顺序，复合 PK/FK 逐列 KeySeq。
- 使用真实 MySQL、Oracle、SQL Server、PostgreSQL、DM 元数据 fixture 往返测试。

### 14.2 引擎已完成的部分（M5/M6）

- `DefineSchema`：引擎行解码的最小 Schema 契约，写入自产自销的规范类型字符串。
- 通用 `PutMetadata` / `Metadata`：无损存储/透传任意元数据记录，引擎不解释语义。

## 15. M10：性能、兼容性与发布

### 15.1 Benchmark

建立可重复基准：

- FULL 顺序写吞吐和压缩比。
- DELTA 小批量/大批量提交。
- Get 冷读、热读、同 Block 局部读取。
- 1、8、32、64 goroutine 并发 Get。
- Scan FULL、短链 DELTA、长链 DELTA。
- Open 索引重放时间和内存占用。
- RebuildIndex 吞吐。

记录 CPU、内存、磁盘、Go 版本、Zstd 版本、数据分布和 BlockSize，避免只保存无环境的数字。

### 15.2 发布任务

- 完成全部 golden files 和损坏样本。
- Linux/macOS/Windows amd64/arm64 兼容测试。
- 运行长时间 Fuzz 和 race。
- API 文档、格式文档、README、示例和迁移说明。
- 为每个导出标识符补 GoDoc。
- 建立 v1 格式兼容性测试，防止后续提交修改磁盘编码。
- 打 tag 前审查所有 TODO、panic、临时枚举和未处理错误。

### 15.3 发布门槛

- `go test ./...` 通过。
- `go test -race ./...` 通过。
- Fuzz 核心 Decoder 累计运行至少 8 小时无崩溃。
- 需求文档 AC-001 至 AC-012 全部有自动化测试。
- 所有 golden files 在干净环境逐字节一致。
- 无已知数据损坏、提交原子性或越界分配问题。

## 16. 工作包与依赖关系

```text
M0
 └─ M1 fileformat
     ├─ M2 metadata/codec
     └─ M3 block/io
         └─ M4 index
             └─ M5 FULL vertical slice
                 ├─ M6 DELTA/Scan
                 └─ M7 Cache/concurrency
                     └─ M8 recovery/verify
                         └─ M10 release
```

可并行项：

- M2 的 Metadata 与 TypedTuple 可由不同开发者并行，但共享编号和错误规范。
- M7 Cache 可在 M4 后独立开发。
- M9 的 fixture 和映射表可在 M2 后开始，最终集成依赖 M5。
- README、示例和 inspect 工具可在 M5 后持续进行。

## 17. Issue 拆分模板

每个开发 Issue 至少包含：

```text
标题：明确模块和行为
关联规范：文档章节
输入/输出：公开或内部接口
磁盘影响：无 / 读取 / 写入 / 修改格式
并发语义：是否可并发
失败语义：错误类型和恢复行为
安全限制：长度、数量、递归、内存
测试：unit / fuzz / race / golden / integration
完成条件：可自动验证的结果
```

所有标记“修改格式”的 PR 必须由格式负责人审核，并更新 golden compatibility test。

## 18. 首批建议 Issues

1. 初始化 Go module、CI 和包目录。
2. 固定格式常量与结构大小测试。
3. 实现 CRC32C、align8 和安全整数运算。
4. 实现两个 FileHeader。
5. 实现 Snapshot/Block Header/Footer。
6. 实现 IndexTxn 固定结构。
7. 实现 Metadata Field TLV。
8. 实现通用 Metadata 记录（TLV）API。
9. 实现 TypedTuple 定宽类型与 Null Bitmap。
10. 实现 String/Bytes/DateTime/Decimal。
11. 实现 None/Zstd Block。
12. 实现空 Store 和单 FULL golden file。
13. 实现 IndexTxn 重放与 immutable view。
14. 实现 Create/Open/Close。
15. 实现 SnapshotWriter FULL 和 Get。

首个可演示版本定义为 Issue 1–15 完成：能够写入一个含 Header/Table/Column 元数据的 Zstd FULL 快照，关闭、重开并随机读取行。

## 19. 主要风险与缓解

| 风险 | 影响 | 缓解措施 |
| --- | --- | --- |
| 格式字段后期改变 | golden 和兼容性失效 | M1 冻结、结构大小测试、格式负责人审核 |
| 两文件提交窗口 | 调用者不知道是否成功 | `.rpk` 权威、CommitError.Unknown、重开查询 ID |
| DELTA 链过长 | Get/Scan 退化 | 深度限制、指标；后续 checkpoint/compact |
| 索引全量载入内存 | 大文件内存高 | 统计和 benchmark；v1 后引入 mmap/分页索引 |
| 元数据方言差异 | 行类型映射不正确 | 保存原字段、显式派生接口、真实 DB fixtures |
| Property 字段未来变化 | 元信息丢失 | 有序 FieldSet、未知 TLV 无损透传 |
| Go int 平台差异 | 32 位溢出 | 磁盘 i64、读回边界检查 |
| 压缩炸弹/损坏长度 | OOM 或崩溃 | 分配前验证、解压目标上限、Fuzz |
| Cache 并发生命周期 | 数据竞争或悬空引用 | 不暴露内部 buffer、singleflight、race test |
| Windows fsync/rename/lock 差异 | 恢复语义不一致 | 平台抽象和跨平台故障测试 |

## 20. v1.0 完成定义

只有同时满足以下条件才能宣布 v1.0 完成：

1. 需求、二进制、元数据和 API 文档与实现一致。
2. 所有公开 API 有测试和 GoDoc。
3. 所有磁盘 Decoder 通过 Fuzz，所有并发路径通过 race。
4. FULL/DELTA、历史读取、Scan、Cache 和恢复达到验收标准。
5. `meta.Store` 13 类核心数据逐字段无损往返。
6. 未知非 Critical 元数据能无损透传，Critical 元数据能安全拒绝。
7. v1 golden files 已锁定并纳入 CI。
8. 发布包不含未受控格式 TODO、调试输出或可触发 panic 的不可信输入路径。
