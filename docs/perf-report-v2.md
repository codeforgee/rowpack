# RowPack v2 单文件性能报告（v1 基线对比）

> 状态：V2-M6 发布基线
> 日期：2026-09-08
> 环境：Go 1.27 / darwin/arm64 (8 核)，klauspost zstd v1.20，BlockSize 256 KiB /
> Zstd / SyncCommit，数据集 100k 行 × 7 列（基准）与 2000 行（文件大小样本）。
> 方法：`-benchtime=10x -count=1` 双分支同机顺序执行（v1 = main `bb17f98`，
> v2 = v2-single-file `303b738`）；样本量小，数值看趋势不看绝对值。

## 1. 核心基准对比

| 基准 | v1（双文件） | v2（单文件） | 差异 |
| --- | --- | --- | --- |
| FULL 顺序写 | 118.5 ms/op · 84.4 krows/s | 103.8 ms/op · 96.4 krows/s | **写吞吐 +14%**（两次 fsync → 一次） |
| Get 热读 | 6.18 µs/op · 4 allocs | 5.90 µs/op · 4 allocs | 持平（+5%，噪声内） |
| Get 冷读 | 267.9 µs/op · 9 allocs | 269.6 µs/op · 9 allocs | 持平 |
| Scan 100k | 16.0 ms/op | 14.8 ms/op | +8% |
| Open 索引重放 | 7.34 ms/op | 5.72 ms/op | **+28%**（按 IndexTxn 区段读 vs 整 .rpi ReadAll） |

结论：单行热/冷读与 Scan 与 v1 持平（验收标准"单行 Get 回退 ≤10%"满足）；
写路径与 Open 由单文件事务显著受益。

## 2. 落盘布局（2000 行样本）

| | v1 | v2 |
| --- | --- | --- |
| 文件数 | 2（.rpk 28,057 B + .rpi 80,616 B） | **1（.rpk 108,593 B）** |
| 总字节 | 108,673 | 108,593 |

总落盘量持平（-80 B，footer 扩展与 header 合并相抵）。备份/迁移/复制从
"两文件保持配对"变为"拷贝一个文件"。

## 3. 批量读取（v2 新增能力）

`BenchmarkBatchVsGetLoop`（冷缓存 CacheBytes=0，连续 1000 行，50000 行 store）：

| 路径 | 结果 |
| --- | --- |
| 逐行 Get 循环 | 260.2 ms/op（每行重复解压整个块） |
| ReadRowsByIDs 迭代器 | 0.62 ms/op（每块只解压一次） |

约 **417×**，远超 V2-M4 门槛（≥3×）。并行块解码（`Parallelism>1`）输出与
顺序路径逐字节一致（race 检测通过）。

## 4. 与验收标准的对照

| 验收项（BINARY_FORMAT_V2 §16） | 状态 |
| --- | --- |
| Store 只有一个持久化文件 | ✅ |
| 数据、IndexTxn、Footer 一次 fsync 原子提交 | ✅（fault 点位 `commit.sync.*`） |
| Get/Scan/Schema/FULL/DELTA 语义保持 | ✅ 全量测试 + golden |
| 单行 Get 回退 ≤10% | ✅ 持平 |
| IndexTxn 损坏可由本事务 Block 重建内存索引 | ✅（含加密密文篡改场景） |
| 任一写入边界崩溃只见提交前/后完整快照 | ✅（crash fault 点位矩阵，含子进程 os.Exit） |
| 批量读取同 Block 只解压一次 | ✅（stats 断言） |
| 17 类型写/关/重开/Get/Scan 往返 | ✅（全类型 golden + vertical 测试） |
| golden、fuzz、race、故障注入、性能测试 | ✅ 全绿 |

## 5. 后续独立优化（不在 v2 首版范围）

- 单行冷读整块解压放大（Page 级索引，ADR-003 备选）
- Row Index 常驻内存的紧凑表示（1M 行 ~24 MB）
- 批量 `MaxBytes` 工作集统计的精化（当前按候选块 raw 字节计）
