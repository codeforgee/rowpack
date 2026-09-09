# RowPack 测试规划（最小必要集）

> 背景：全量单元测试已删除（保留 golden），当前处于「格式与 API 未公开、面向重构」阶段。
> 原则：**只保留能锁住格式、能挡住故障、能快速评估性能的最小集**；不做全面覆盖。

## 分层策略

| 层 | 测试 | 职责 | 取舍理由 |
| --- | --- | --- | --- |
| 格式锁定 | `TestGoldenManifest` / `TestGoldenStoreSamples`（已有）<br>`internal/fileformat/structs_test.go`（新增） | 字节级 golden 哈希 + 固定结构大小/魔数/枚举回读 + 所有固定结构的 Marshal/Unmarshal round-trip + CRC 已知答案 | 文件结构固定是硬约束，任何字节级漂移必须失败 |
| 故障恢复（到位） | `recovery_test.go`（新增，主包） | 8 个 commit 故障点逐个 crash；torn tail 截断（rw/ro）；IndexTxn 损坏内存重建；mid-file 结构损坏必报错；块负载损坏读路径 `ErrCorruptData` + `Verify` 结构化检出；加密 store 同类场景 | 崩溃一致性是单文件引擎的命根子，故障测试必须到位 |
| 核心语义 | `store_test.go` / `values_test.go`（新增） | 生命周期、FULL/DELTA 可见性、错误哨兵、表名寻址（含 ReadBatch/Schema）、全部 17 种类型 + NULL + Decimal 边界 + 行复用 + rowIDSet | 无全面覆盖，只锁公共契约与编码正确性 |
| 加密 | `encryption_test.go`（新增） | round-trip、无 key `ErrKeyRequired`、错 key 认证失败、key 提供者错误、密文篡改 AEAD 检出、加密 golden 重开 | 加密是独立安全域，必须单独锁 |
| 性能 | `bench_test.go`（新增） | 单配置快速套件：写 / 热冷读 / 扫描 / 批量 vs 逐行 / Open 重放 / 深链 / 加密写读 | 不做矩阵，秒级完成即可评估相对性能 |
| 省略 | LRU、seal 单测、block builder/reader 单测、index view 单测、metadata TLV 单测 | — | 均由 golden + e2e + recovery 全路径覆盖；按「不全面」原则不设独立测试 |

## 关键设计决策

1. **故障注入点**：`internal/fault` 已有 8 个命名点（header/block/txn/footer/sync/publish）。
   测试用 panic 模拟进程死亡（`crashCommit` 包装 recover），验证每个点之后文件状态的
   可恢复性——`recovery_test.go` 明确区分「footer 前崩溃 = 尾部截断」vs「footer 后崩溃 =
   文件级已提交、调用方必须查询快照 ID」。

2. **读路径损坏哨兵**：`blockLoader.blockReadError` 把所有块读取/解码失败归一为
   `CorruptionError`（保持 `ErrAuthFailed` 等 cause 链），使 `Get/Scan/ReadBatch/Verify`
   都能 `errors.Is(ErrCorruptData)`——这是从故障测试中反推补齐的契约缺口。

3. **DELETE 严格校验补漏**：`put()` 原先只在非 DELETE 分支跑 `checkStrictParent`，
   Strict 模式下删除不存在的行被静默接受；现为 DELETE 也执行父视图存在性检查
   （`writer.go`），与 `checkStrictParent` 的 `ChangeDelete` 分支意图一致。

4. **表名寻址收敛**：`ReadBatch`、`Schema` 由 `TableID` 改为表名字符串，与
   `Get/Scan/Blocks/ScanBlocks/Exists` 统一（读路径全部按名寻址，内部 ID 不外泄）。

## 性能套件（`make bench`，~10-20s）

数据集统一 100k 行 × 7 列（256 KiB 块、Zstd、SyncCommit 默认档）：

- `BenchmarkWriteFull` — 顺序写吞吐（krows/s）
- `BenchmarkGetHot` / `BenchmarkGetCold` — 热（缓存命中，dst 复用）/ 冷（CacheBytes=0）
- `BenchmarkScan` — 全表扫描 100k
- `BenchmarkReadBatch1000` vs `BenchmarkGetLoop1000` — 批量聚合倍数
- `BenchmarkOpenReplay` — Open 索引重放
- `BenchmarkDeepChainGet` — 32 层 DELTA 链随机读
- `BenchmarkEncryptedWrite` / `BenchmarkEncryptedGetHot` — 加密档相对成本

## 页容器格式测试（S2 新增）

`internal/block/rows_container_test.go` 锁住页容器格式：

- 多页容器 round-trip（300 条混合变更 + 乱序 RowID，4 KiB 页），`ForEach` 顺序与
  `RecordAtScratch` 随机访问逐条对照；
- Zstd 压缩页 round-trip 与 `StoredSize < RawSize` 断言；
- 超大连行页（Flags bit0）独立成页；
- 损坏矩阵：截断（各区域边界）、单 bit 翻转（头/目录区）、伪造 ItemCount，
  均不得 panic 或无界分配，`ParseRowsContainer` 必须报错。

搭配 `internal/block/rows_page_test.go`（页流编码）+ `internal/fileformat/rows_block_test.go`
（块头/目录 marshaling），覆盖页容器层的字节级不变式。

## Row Index Page + Fence 矩阵（S3-⑦ 落盘② 新增，§13.2）

行索引从 chunk delta 切换为排序 Row Index Page + Fence Directory 后，新增以下损坏测试：

- **页内损坏**（`internal/index/row_index_page_test.go`）：`encodeRowIndexPage` /
  `decodeRowIndexPage` 的截断（头/各流边界）、单 bit 翻转（magic/version/长度/锚点/CRC/
  流区）、伪造 size（TableRunBytes/ChangeBitsBytes）、伪造 EntryCount（=MaxUint32，
  分配前报错）、非法 changeType（2bit=3 保留）、CRC 错误、排序破坏——均报错且绝不
  panic/无界分配；RowID 边界（0 / MaxUint64 / 跨 delta 溢出）、多表 run、ordinal 负 delta。
- **Fence 目录层**（`internal/index/row_index_page_corrupt_test.go`，直接针对
  `parseRowIndexPages`）：伪造 `RowIndexPageCount`（巨值，分配前拒绝；0 且空区）、Fence
  越界 `StoredOffset`、零 `StoredSize`、错误 `SnapshotID` 归属、重叠/重复页、页压缩负载
  损坏——均报错且无 panic/无界分配。
- **Header `RowIndexPageCount` 字**（`internal/fileformat/indextxn_test.go`）：offset
  12..16 与 76..80（KeyEpoch）互不冲突的 round-trip 与 `PatchIndexTxnHeaderForStorage`
  保留测试。
- **fuzz**：`FuzzParseRowIndexPages`（seed 正反例；`go test -fuzz=` 启用）喂任意页区，
  必须不 panic、不向 sink 递交畸形批。

## 运行

```sh
make test        # 全量（golden + 结构 + 核心 + 故障 + 加密）
make race        # 并发数据面冒烟
make bench       # 快速性能评估
make bench-batch # 批量读聚焦对比
make golden      # 仅当有意的格式变更（人工 diff 审查）
```