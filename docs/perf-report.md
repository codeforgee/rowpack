# RowPack 性能测试报告

> 文档状态：**统一基线（v2 单文件格式 Tier 0，矩阵恢复版）**
> 日期：2026-09-09（当前基线）
> 环境：Go 1.27.0 / darwin arm64 (Apple Silicon M1 Pro) / klauspost/compress v1.20.0 (zstd)
> 说明：本报告 §1–§2 由统一套件 `make bench` 生成，完整输出落在 `docs/bench-results.txt`
> （gitignore，机器相关，可随时复现）。历史 v1 对照批次见 §3。
>
> **格式重构基线已冻结**：重构验收对照物见 `docs/baseline/`（双口径定义、方差档、
> 固定数据集几何）。重构 KPI 起点：冷读临时分配 328,645 B/op（无池口径）、冷读
> rawB/op 327,095（整块解压）、Eager 索引 24.03 B/row。验收门槛见
> `FILE_FORMAT_REFACTOR_PLAN.md` §3；执行排期见 `REFACTOR_EXECUTION_PLAN.md`。

## 0. 重构 KPI 基线（S0 冻结，2026-09-09）

| KPI | 冻结值 | 验收门槛（§3.1） |
| --- | --- | --- |
| 冷读临时分配（无池） | 328,645 B/op | ≤ 64 KiB/op（期望 ≤ 40 KiB） |
| 冷读 rawB/op（读取放大） | 327,095 B | 冷读延迟 ≥ 4x（期望 6–10x） |
| Eager 索引常驻 | 24.03 B/row | ≤ 16 B/row（期望 ~12） |
| Open 1M 峰值 | 68.7 MB/op 临时分配 | 最终索引 + 2 个 Index Page |
| Scan 100k | 134 allocs（arena 已生效） | 0 alloc/row（已达标，保持） |
| FULL 顺序写 | ~96.0 万行/s | ≥ 基线 90% |
| 热点读 | 277 ns/op | ≤ 基线 125%（期望 110%） |
| 冷读 CPU 构成 | zstd 51.5% + CRC 18.2% | —（佐证整块成本） |

方差档 ≤ ±3.7%（`docs/baseline/bench-s0-variance.txt`），分配类指标恒定。

## 0.5 S2 页容器重构实测（2026-09-09，20k 行直读档，同机同口径）

S2 把 Rows Block 切为页容器（逻辑块 + 独立压缩 32 KiB Page），页级 I/O + 页内
O(1) 记录索引。实测（`BenchmarkGetHot / GetCold / GetColdUnpooled / WriteFull / Scan`）：

| 场景 | S0 基线 | S2 实测 | 门槛 | 结论 |
| --- | --- | --- | --- | --- |
| 冷读临时分配（无池） | 328,645 B/op | **57,932 B/op**（≈56.6 KiB） | ≤ 64 KiB | ✅ |
| 冷读 readB/op | 62,843 B | **5,845 B** | —（页级 I/O） | 只拉一页 |
| 冷读 rawB/op（读取放大） | 327,095 B | **32,778 B** | — | 只解压一页 |
| 冷点读延迟（无池） | ~349 µs | **~48 µs** | ≥ 4x | ✅ ≈7.3x |
| 热点读 | 277 ns | **246 ns** | ≤ 125% | ✅ ≈89% |
| FULL 顺序写 | 104.2 ms | **87.3 ms** | ≥ 90% | ✅（更快） |
| Scan 100k | 13.6 ms · 134 allocs | **13.5 ms · 64 allocs** | 0 alloc/row | ✅ |
| 文件大小（golden 样本） | — | 变小（页格式去掉逐行冗余） | ≤ +10% | ✅ |

> 说明：冷读临时分配由整块解压（~256 KiB raw）降为一页（~32 KiB raw）+ 目录 + 页索引
> ≈ 57.9 KiB，高于 40 KiB 期望但仍低于 64 KiB 硬门槛。

**加密档（逐页 nonce，S2 item⑤ 已实现，增量成本单独报告）**

| 场景 | 明文 | 加密 | 增量 |
| --- | --- | --- | --- |
| 热点读 | ~255 ns/op | ~283 ns/op | ~+11% |
| 顺序写 | ~87 ms | ~180 ms | ~2x（每页 AES-GCM） |
| 敏感读（每个 GET 只 OPEN 一页） | 页级 I/O | 页级 I/O（整页 OPEN） | 无整容器放大 |

加密块冷读由于逐页 OPEN，同样只读/解密/解压所访问的那一页，不引入整容器读取放大
（metadata 块仍整容器密封）。逐页 nonce 域分离与篡改/跨上下文拒绝测试见
`internal/seal/seal_page_test.go`。

**S3-⑧ Eager 索引内存压缩（SoA/block-run）**：Open 1M 行 `idxB/row` 24.02 → **13.02**
（idxMB 22.91 → 12.42 MiB），满足 Eager ≤16 B/row；Open 1M 峰值分配 68.7 → 65.9 MB。
热点读 256ns 不变，Scan 100k 65 allocs（0 alloc/row）。详细决策见
`REFACTOR_EXECUTION_PLAN.md` §9.1.2。

**S3-⑦ 落盘②：IndexTxn 正文行索引切换到排序 Row Index Page + Fence**（2026-09-09，
同机同口径，1M 行 Open 档）。本提交把行索引从 chunk delta 二进制改为排序页+Fence，并把
Eager 读路径改为从 Index Page 解码直建 SoA/block-run shard（S3-⑧ 的 13.02 B/row 不变）。

| 场景 | S3-⑧ | 本提交（S3-⑦ 落盘②） | 门槛 | 结论 |
| --- | --- | --- | --- | --- |
| Open 1M `idxB/row` | 13.02 | **13.02**（页+Fence 解码直写 shard） | ≤16 | ✅ |
| 热点读 ns/op | 256 | **250.3** | ≤125% (~260ns) | ✅ |
| 冷读无池 `readB/op` | — | **5,870** | ≤64 KiB | ✅ |
| FULL 写 MB/s | — | **70.9**（块写路径未变） | ≥基线 90% | ✅ |
| 文件大小 golden | — | `full-delta` 4601→**4588 B** / `encrypted` 2047→**2035 B** | ≤+10% | ✅ 变小 |

关键点：Open `idxB/row` 未回归，因为 Eager 读路径仍直建同一 SoA/block-run shard，只是
数据源从 chunk delta 换成 Index Page 解码（`decodeRowIndexPage` + 流式 `TxnSink`，不物化
`[]RowIndexEntry`）。块数据面写/读路径未动，热点/冷读/写吞吐不变；设计编码比 chunk delta
更紧凑（ADR-005：seq/delta 省 ~43%、rand 省 ~13%），golden 文件变小。加密 store 的
Index Page 按 R11 走 Index 域 chunk-nonce/AAD 密封，冷读/热点加密档与 S2/S3-⑧ 一致。

**S3-⑨ 峰值优化 + S4 Lazy 模式**（2026-09-09，同机同口径，1M 行 Open 档）。

| 场景 | S3-⑧ → S3-⑦ 落盘② | **S3-⑨（P1）** | **S4 Lazy（P2）** | 门槛 | 结论 |
| --- | --- | --- | --- | --- | --- |
| Open 1M `idxB/row`（Eager） | 13.02 | **13.02** | 13.02（Eager 不变） | ≤16 | ✅ |
| Open 1M `B/op`（Eager） | 65.9 MB | **15.6 MB** | 15.6 MB | ≤65.9 MB | ✅ 大幅下降 |
| Open 1M `fenceB/row`（Lazy 常驻） | — | — | **0.0127** | ≤0.25（期望≤0.1） | ✅ |
| Open 1M `B/op`（Lazy） | — | — | **0.23**（无页物化/shard 构建） | — | ✅ |
| 热点读（Eager） | 250 ns | 不变 | **250 ns**（Lazy 分支不劣化 Eager） | ≤125% | ✅ |

S3-⑨（P1）把 Open 的 `[]RowIndexEntry` 页物化 + `[]RowKeyLoc` 全量中间层去掉：
`walkRowIndexPage` 流式吐条目，直喂 `rowShardBuilder`（SoA/block-run），Eager 常驻 13.02
B/row 不变，但 Open 瞬态分配 65.9 → 15.6 MB。

S4 Lazy（P2）在保留 Eager（零值默认）前提下新增 `IndexLazy`：Open 只加载 Row Index
Fence（52 B/页），真实页按需 OPEN+decompress+decode 并在 bounded `IndexPageCache` 缓存。
常驻 `fenceB/row` 0.0127（远低于 0.25），`CacheBytes = DataPageCache + ScanWindow +
IndexPageCache` 守恒。深链 Get 的 I/O 放大可接受（Fence `[Min,Max]` 边界过滤 + 页缓存
吸收），**不引入**负查询缓存或 page bloom filter（详见 ADR-006 决策点 #4）。

## 1. 基准套件与指标口径

`make bench` 一条命令复现全部基线（默认 `-benchtime=1s -count=1`，可用 `BENCHTIME`/
`BENCHCOUNT` 覆盖），包含四组入口：

- **`BenchmarkEnv`**：运行环境自描述 + 标准数据集几何（100k 行 × 7 列 @ 256 KiB：
  单文件 2.20 MB、压缩比 0.190、单行落盘 23.0 B、38 个块、索引常驻内存 2.29 MB）。
- **`BenchmarkMainMatrix`**：64 个子测试覆盖 写/读/扫描/深链/打开/索引重建 场景 ×
  BlockSize (64K/256K/1M) × 缓存(hot/cold) × 持久化(sync/async) × I/O
  (mmap/readat)，读场景带预热。剪枝规则见 `bench_matrix_test.go` 文件头。
- **`BenchmarkLatency`**：固定 4096 样本的点读延迟分位数（热 / 全范围热 / 冷 / 深链）。
- **直读档**：`BenchmarkWriteFull / GetHot / GetCold / Scan / ReadBatch1000 /
  GetLoop1000 / OpenReplay / DeepChainGet / EncryptedWrite / EncryptedGetHot`，
  与历史 `bench-results.txt` 同口径，逐日可比。

### 指标中文对照（矩阵自定义指标）

| 输出指标 | 中文含义 |
| --- | --- |
| krows/s | **行吞吐量**（每秒写入或扫描的行数） |
| kget/s | **点读吞吐量**（每秒随机单行读取次数） |
| dataMB | `.rpk` 单文件落盘大小（v2 含内嵌 IndexTxn） |
| ratio | 压缩比（落盘字节 / 原始字节，越小越好） |
| bytePerRow | 单行落盘字节 |
| idxMB | 索引常驻内存 |
| hitpct / scanhitpct | 块缓存 / 扫描窗口命中率（%） |
| rssdMB | 进程峰值 RSS 增量（本子测试归属，近似） |
| p50ns / p95ns / p99ns | 点读延迟分位数（纳秒） |

快速档 `make bench-quick`（20k 行 / 200k 大场景 + 固定 3 次迭代，约 15 秒）用于
结构与覆盖冒烟，ns/op 不与 100k 基线可比，量级参考见 §4。

## 2. 当前基线要点（256 KiB / mmap / sync 档，2026-09-09）

吞吐量统一用中文口径表述（行吞吐量 = 万行/秒，点读吞吐量 = 万次/秒）：

| 场景 | 吞吐量 / 延迟 |
| --- | --- |
| FULL 顺序写**行吞吐量** | ~101.6 万行/秒（sync · 98.4 ms/次提交）· ~104.5 万行/秒（async）|
| 同构行写**行吞吐量** | ~186.3 万行/秒（sync）· ~203.7 万行/秒（async）· 压缩比 0.044 |
| Get 热**点读吞吐量**（100 键工作集，复用 dst） | ~238.1 万次/秒 · 420 ns/op · 命中率 100% · 2 allocs · 16 B/op |
| Get 热**点读吞吐量**（全键域 + dst 复用） | ~233.3 万次/秒 · 429 ns/op · 2 allocs · 20 B/op |
| Get 冷**点读吞吐量** | ~2,958 次/秒 · 338 µs/op · 6 allocs · 160 B/op（池化瞬态解压）|
| 并发 Get **点读吞吐量**（64 goroutine） | ~156.8 万次/秒 · 638 ns/op · 3 allocs |
| Scan 100k **行吞吐量**（热 / 冷） | ~738.0 / ~365.4 万行/秒（13.55 / 27.37 ms）· 全表 135 / 249 allocs |
| Scan 1M **行吞吐量** | ~421.6 万行/秒（237.2 ms）· 全表 2,227 allocs |
| 随机点读**吞吐量**（1M 键域） | ~6,204 次/秒 · 命中率 53.8%（64 MiB 缓存有界）|
| 深链（32 层 DELTA）Get **点读吞吐量** | ~45.1 万次/秒 · 2.22 µs/op · 3 allocs |
| 深链 Scan **行吞吐量**（13.2 万行） | ~483.3 万行/秒 · 27.3 ms · 268 allocs |
| Open 索引重放 | 1.66 ms |
| IndexTxn 损坏重开（内存重建，文件不改写） | 19.7 ms |
| 点读延迟 p50/p95/p99（热） | 417 / 500 / 667 ns |
| 点读延迟 p50/p95/p99（冷） | 336 / 400 / 591 µs |
| 点读延迟 p50/p95/p99（深链） | 2.08 / 4.88 / 6.38 µs |
| 加密档（AES-256-GCM）顺序写**行吞吐量** | ~46.6 万行/秒（214.4 ms/次提交）· 热读 2 allocs · 335 万次/秒（直读档 20k 口径）|

### BlockSize 梯度（sync / mmap 档）

| 维度 | 64K | 256K | 1M |
| --- | --- | --- | --- |
| 顺序写**行吞吐量** | 98.8 万行/秒 | 99.8 万行/秒 | 101.1 万行/秒 |
| 冷点读延迟 | 85.6 µs | 323.5 µs | 1.35 ms |
| 扫描**行吞吐量**（热） | 728.3 万行/秒 | 728.9 万行/秒 | 729.8 万行/秒 |
| 单文件落盘 | 2.25 MB | 2.20 MB | 2.22 MB |

### 维度结论

- **BlockSize**：写吞吐量与扫描吞吐量对块大小不敏感；块越小冷点读越快
  （64K 冷读 85.6 µs vs 1M 冷读 1.35 ms，整块解压成本随块大小线性），但小块
  增加索引条目与元数据开销（64K 单文件 2.25 MB 略大）。
- **I/O 路径**（mmap vs readat）：热读与扫描差异在噪声内（±1%）；冷读 readat
  略慢 3–5%。
- **持久化**（sync vs async）：async 写吞吐量 +4~9%（省一次 fsync）。
- **并发**：读路径多核扩展良好，64 goroutine 点读吞吐量 156.8 万次/秒；
  无锁竞争悬崖。

### 分配与内存优化记录（2026-09-09 第二轮）

以 pprof（alloc_space / alloc_objects / cpu）对写/热读/冷读/扫描四条路径定位
热点并修复，全部改动经 `go test -race` 与统一矩阵复测验证：

| 改动 | 位置 | 效果（256K/mmap 档实测） |
| --- | --- | --- |
| Scan 的 Bytes 列改走迭代器 arena 零复制（原每行复制一次 payload） | codec.Sink 泛化（String+Bytes 双物料化器）+ strArena.materializeBytes | Scan 100k 全表分配 100,121 → **135**（-99.9%）；Scan 1M 1,002,100 → **2,227**；扫描吞吐量 +1.3% |
| 禁用缓存（CacheBytes<0）的冷读改走池化瞬态解压（原每读一块即丢弃一个 ~330KB 缓冲） | readRowInto / ReadBatch 路由到 loader.LoadScan | 冷读 328,149 → **160 B/op**（-99.95%）· 7 → 6 allocs；5 轮中位 329 µs（噪声带内 +1.3%）|
| ParseRowAt 返回 RowRef 值（原指针堆逃逸，热读每行一次） | block.ParseRowAt / decodeRowInto | 热读 3 → **2 allocs/op** · 96 → **16 B/op**（-83%），速度持平（270 ns）|
| 深链 Get / 加密热读同步受益 | — | 深链 Get 4 → 3 allocs · 800 → 720 B/op；加密热读 310.8 → **297.9 ns**（-4.2%）且 3 → 2 allocs |
| ReadBatch 的 String/Bytes 列改走每次调用独立的 arena 视图（行仍互不别名、不 pin 块缓冲） | read_batch.go + decodeRowInto 增设 sink 参数 | ReadBatch1000 分配 2,018 → **19**（-99%）· 229 µs（逐行 Get 基线 281 µs · 2,000 allocs）；B/op 持平（大头为返回行 Value slab，不可避免）|

**写路径调研结论**（`-memprofilerate=1` 精确计数）：BenchmarkWriteFull 报告的
~4 allocs/行几乎全部来自基准负载自身的行构造（fmt.Sprintf + 复合字面量），
引擎 Insert 路径靠缓冲构建器与编码器池化已是 ~0 allocs/行；剩余 ~1,000
allocs/op 为 commit 批量分配（zstd/索引缓冲，均已池化复用），无可观收益点，
故本轮不动写路径。Get 路径的 String/Bytes 复制保留（Get 是并发入口，无法
安全 alias 有界 LRU 缓存的块缓冲）。CPU 侧最大单项 runtime.madvise（~21%，
将瞬态页归还 OS）随冷读/扫描垃圾量下降已显著缓解；剩余写 CPU 主体为 zstd
压缩本体与 fsync。

## 3. 与 v1（双文件）基线的对照（历史记录，v1 已废弃）

同机双分支顺序执行（v1 = main，v2 = 本分支，`-benchtime=10x`，小样本看趋势）：

| 维度 | v1 | v2 | 变化 |
| --- | --- | --- | --- |
| FULL 顺序写 | 84.4 krows/s（两次 fsync） | 96.4 krows/s（一次 fsync） | **+14%** |
| Open 索引重放 | 7.34 ms（.rpi 整读） | 5.72 ms（按区段读） | **+28%** |
| Scan 100k | 16.0 ms | 14.8 ms | +8% |
| Get 热/冷读 | 持平 | 持平 | ±5% 噪声内 |
| 落盘总量（2000 行样本） | 108,673 B / 2 文件 | 108,593 B / **1 文件** | 备份 = 拷贝单文件 |
| 批量冷读（1000 连续行） | 260 ms（Get 循环） | 0.62 ms（迭代器） | **~417×** |

## 4. 快速档（bench-quick）量级参考

`make bench-quick`（20k 行 / 200k 大场景 / 3 次迭代 / ~15 秒）2026-09-09 快照，
仅验证套件结构与吞吐量量级，不与上表基线比数值：

- 顺序写**行吞吐量** ~76–83 万行/秒；同构行写 ~134–197 万行/秒（压缩比 0.044）。
- 扫描**行吞吐量**（热/冷）~700 / ~360 万行/秒；深链点读 3.2–4.4 µs。
- 延迟分位数（4096 固定样本，可信）：热 p50 = 417 ns；冷 p50 = 334 µs；深链 p50 = 1.8 µs。
- 注意：直读档（`BenchmarkGetHot` 等）在 3 次迭代下无预热收敛，ns/op 噪声大，
  以矩阵格子和延迟分位数为准。

## 5. 阅读注意

- 矩阵读场景在计时前有预热/烧入（热档 100 次、冷档 10 次），首迭代一次性开销
  （GC、页缓存）已剔除；`-benchtime=1s` 下 ns/op 可比。
- `dataMB`/`bytePerRow` 是**单文件口径**（v2 含内嵌 IndexTxn），与 v1 的
  data/index 分列不可直接相加比较。
- `rssdMB` 为 getrusage 峰值 RSS 的子测试增量，属近似归因；mmap 页不计入 RSS
  属预期。
- `index_rebuild` 是 v2 语义：IndexTxn 位腐后重开的内存重建（文件永不改写），
  仅影响该快照、只进内存；与 v1 的 RebuildIndex 不同。
- `BenchmarkGetHot / GetCold / DeepChainGet / Encrypted*` 直读档使用 20k 行数据集
  （历史口径），与矩阵 100k 档的绝对值不可直接互比；矩阵与直读档各自纵向可比。
- `make bench-batch`（10s 每场景）单独量化批量读聚合效应：冷缓存连续 1000 行
  场景较逐行 Get 提升数百倍。
