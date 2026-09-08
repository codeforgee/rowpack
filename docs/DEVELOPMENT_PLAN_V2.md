# RowPack v2 单文件开发计划

> 状态：执行草案
> 日期：2026-09-08
> 格式设计：[BINARY_FORMAT_V2.md](BINARY_FORMAT_V2.md)
> API 设计：[GO_API_DESIGN_V2.md](GO_API_DESIGN_V2.md)
> 架构决策：[ADR-003](adr/ADR-003.md)
> 风险清单：[V2_DESIGN_RISKS.md](V2_DESIGN_RISKS.md)（各里程碑已标注对应 R 项）

## 1. 开发原则

1. v2 直接替代现有文件格式，不保留旧 reader、golden、双格式分派或迁移路径。
2. 先打通单文件 FULL，再实现 DELTA、批量读取和加密。
3. v2 复用现有 Row/Block/Metadata/Snapshot Index，索引事务内嵌到单文件；不另造 Page 索引。
4. SnapshotFooter 仍是提交权威；任何优化不得绕过 Footer 可见性。
5. 每个阶段同时包含正确性、恢复、fuzz、race 和性能基准。
6. 格式固定结构在实现和测试通过前保持 Draft，不提前宣布冻结。

## 2. 里程碑

| 里程碑 | 结果 | 状态 |
| --- | --- | --- |
| V2-M0 | 精确布局、ADR、测试矩阵冻结 | ✅ 已冻结（Footer 144B / ROWPACK2 / 备份单文件语义） |
| V2-M1 | 单文件 Header、Block、IndexTxn、Footer 编解码 | ✅ v2-single-file `25d9a5e` |
| V2-M2 | FULL Snapshot 写入、打开、Get/Scan | ✅ v2-single-file `ee9a6d8` |
| V2-M3 | DELTA、历史读取、尾部恢复 | ✅ v2-single-file `e5e4151` |
| V2-M4 | 批量 Block planner 与范围读取 | ✅ v2-single-file `ddd89ca`（冷缓存 1000 行 ~417× vs Get 循环） |
| V2-M5 | Block Cache、批量规划和缓存预算 | ✅ v2-single-file `1384067`（Parallelism 并行解码 + 淘汰一致性） |
| V2-M6 | AES-256-GCM、故障注入和发布基准 | ✅ v2-single-file `303b738`（IndexTxn 域加密 + 篡改恢复）；发布基准见 perf-report-v2 |

## 3. V2-M0：冻结设计输入

- 确定 `.rpk` 单文件扩展名和 Magic；
- 确定 File/Snapshot/Block/Footer 精确尺寸与字段 offset；
- 确定 `PreviousFooterOffset` 和文件尾搜索算法；
- 确定 MinRowID/MaxRowIDExclusive 对空 Block、Metadata Block 和 uint64 最大值的表达；
- 确定 v2 Feature Bits；
- 确定 Batch API 的顺序、重复 ID、空洞和资源限制语义；
- 生成空 Store、单 FULL、FULL+DELTA 的设计向量。

完成标准：所有待冻结项有 ADR 或格式表，`go vet` 可检查固定结构常量测试。

## 4. V2-M1：格式编解码

- 新增 v2 FileHeader；
- 新增带前驱 Footer offset 的 SnapshotHeader/Footer；
- 扩展 Block Header 的 RowID 包围范围；
- 复用现有 Block/IndexTxn 编码，仅增加单文件 offset 校验；
- 保持 Rows/Metadata Payload 可复用时优先复用；
- 所有 Unmarshal 先检查长度、溢出、CRC 和边界；
- 增加 round-trip、短输入、未知 Feature、CRC、offset 和 fuzz 测试。

完成标准：纯内存格式测试全部通过，不读写真实 Store。

## 5. V2-M2：FULL 单文件闭环

- Create 默认创建 v2；
- 单 Appender、单 mmap 生命周期；
- FULL 写入 Header→Blocks→Footer；
- SyncCommit 只调用一次 Sync；
- Open 从尾部定位 Footer 并建立 Snapshot/Block 目录；
- Get 通过现有 Row Index 定位 Block 和 ItemOrdinal；
- Close/Reopen、只读、空文件和超大行测试。

**本里程碑显式包含 R1/R5：** 恢复扫描器必须识别 IndexTxn 四类结构（BINARY_FORMAT_V2
§10.1），正常写入的文件 Open 不得报 mid-file corruption；重放按 IndexTxn 区段读，**不得
整文件 ReadAll**，并加 Open 峰值内存基准（不随数据量放大）。

性能门槛：FULL 写入以当前测试环境重新建立 v2 基线；一次提交只发生一次 fsync。

## 6. V2-M3：DELTA 与恢复

- FULL/DELTA 父链；
- INSERT/UPDATE/DELETE 和 Tombstone；
- 历史 Snapshot 读取；
- Footer 反向链；
- 未提交尾部忽略/截断；
- Header、Block、Footer 各位置故障注入；
- 中间损坏与尾部不完整区分；
- `CommitError{Unknown:true}` 语义验证。

**本里程碑显式包含 R2/R3/R7/R8/R9：**

- 重放改为逐 snapshot 独立校验：中间 IndexTxn 位腐 → 仅该 snapshot 用 Blocks 内存重建并
  Apply，**继续处理后续已提交 snapshot**（不可沿用 v1 “首个坏 txn 即停”语义，R2）；
- 提交判定两维分离测试：Footer 自身 CRC 有效 + 索引绑定失败 = 已提交数据 + 内存重建索引；
  Footer 缺失 = 未提交；Block 数据损坏 = 硬错（R3）；
- 后续 FULL checkpoint（R7）：FULL→DELTA 链、FULL 后 Depth 重置、Footer 链 SnapshotID
  单调在 FULL 后的往返与崩溃注入；
- IndexTxn 区间边界强制（R8）：`BodyBytes` 与 Header/Footer 区间一致性、跨 txn 消费
  防护；fault 点位按 v2 提交顺序重排并新增 indel txn/footer 撕裂点（R9）。

完成标准：每个追加边界执行崩溃注入，重开后只能看到提交前或提交后的完整状态。

## 7. V2-M4：批量分块读取

- RowID 集合排序、去重；
- 多范围规范化和合并；
- 按 Snapshot 层解析覆盖与 Tombstone；
- 用 Block RowID 包围范围筛选；
- BlockID/offset 去重和物理排序；
- 相邻读取合并与有界预读；
- 同 Block 只解压、校验一次；
- BatchIterator、取消、Close 和统计；
- 连续范围自动复用 Scan 内核。

基准矩阵：

```text
Batch = 1 / 10 / 100 / 1k / 10k / 100k rows
分布 = 连续 / 均匀随机 / 集中热点 / 多范围 / 含空洞
链深 = FULL / 8 / 32 / 64 DELTA
缓存 = cold / warm
```

性能门槛：同 Block 不重复解压；批量路径较循环 Get 提升至少 3 倍。

**前置（R14）**：M4 开工前冻结 Block RowID envelope 的来源——由 `view.Apply` 按
RowIndexEntry 集合派生 per-(Snapshot, Table, Block) 的 Min/MaxRowIDExclusive（磁盘块头不
加字段），并钉死 MaxUint64 边界表达（MaxRowIDExclusive=0 表示无上界）；同时冻结批量重复
ID/不可见行语义（R17）与重排缓冲上界（R18）。

## 8. V2-M5：Block Cache 与批量规划

- 为解压 Block 建立按字节预算的 LRU；
- 批量请求按 BlockID 分组、去重和物理排序；
- 评估可选 Bloom Filter，但不改变 Row Index 正确性；
- Scan 使用有界解压窗口，不污染热点缓存；
- 增加 peak RSS、GC、缓存命中和首次访问延迟基准。

性能门槛：1M Scan 峰值内存较当前约 156 MiB/op 下降至少 30%；缓存淘汰前后结果一致。

## 9. V2-M6：加密与发布

- 只实现 AES-256-GCM；
- Rows/Metadata Block 先压缩后加密；
- AAD 绑定 Store/Snapshot/Block/Header；
- 索引 nonce 位内域分离（R11）：`KeyEpoch(31bit) ‖ 域标志(1bit) ‖ 计数器(8B)`，块域与
  索引域互斥，nonce 唯一性不依赖 AAD；
- Footer 的 IndexTxnCRC32C 覆盖落盘密文字节（R12）：撕裂/位腐在无密钥路径可检；
- nonce 唯一性、KeyEpoch、错误密钥和密钥丢失测试；
- 加密 Batch/Scan 不重复调用 KeyProvider；
- 加密恢复和篡改故障注入（含加密 IndexTxn 的尾巴/中间撕裂）；
- 完整性能报告。

完成标准：认证失败绝不返回明文；加密路径无 nonce 复用；文档和 golden 冻结后发布。

## 11. 测试矩阵

必须覆盖：

- None/Zstd；Sync/Async；加密/未加密；
- FULL、DELTA、空 DELTA、长链；
- 全部 17 种 Value 类型；
- 单行、超大行、空字符串/Bytes、NULL、Decimal；
- 单表、多表、空表；
- 顺序/乱序 RowID；
- 批量连续、随机、重复、空洞、多范围；
- Linux/macOS/Windows；
- v2 Create/Open/Rewrite；
- 读写并发、remap、Close 和 context cancellation。

## 12. 提交拆分

建议每个里程碑独立提交，不混合格式、API 和性能机械优化：

```text
v2 format structs
v2 single-file full path
v2 delta and recovery
v2 batch planner
v2 embedded IndexTxn/block cache
v2 encryption
v2 docs/golden freeze
```

每个提交必须通过 `go test ./...`；格式和恢复提交额外通过 race、fuzz smoke 和故障注入。
