# RowPack v2 设计风险点清单（评审记录）

> 状态：评审记录
> 日期：2026-09-08
> 关联规范：[BINARY_FORMAT_V2.md](BINARY_FORMAT_V2.md)、[GO_API_DESIGN_V2.md](GO_API_DESIGN_V2.md)、
> [DEVELOPMENT_PLAN_V2.md](DEVELOPMENT_PLAN_V2.md)、[ADR-003](adr/ADR-003.md)
> 方法：对 v1 现有实现（writer.go、recovery.go、loader.go、internal/index、internal/fileformat、
> internal/seal、internal/iofile）逐路径核对 v2 设计假设，找出与现有代码语义冲突、未定义的
> 边界、以及重构时的隐藏依赖。

严重度：

- **P0**：按现有文档/代码实现必然出错（正常写入的文件无法打开、可损坏数据或身份认证被破坏）；
- **P1**：语义矛盾或会导致特定故障场景下错误行为，必须在实现前钉死；
- **P2**：资源/性能/兼容性隐患，需要显式决策或测试覆盖。

---

## 风险总表

| ID | 位置 | 严重度 | 一句话风险 |
| --- | --- | --- | -- |
| R1 | 格式 §10.1 / recovery.go | P0 | 恢复扫描器不识别 IndexTxn，正常 v2 文件被误判 mid-file corruption |
| R2 | 格式 §9 / replay.go | P0 | v1 重放是"首个坏 txn 即停"；v2 中间 IndexTxn 损坏必须"逐 snapshot 重建并继续" |
| R3 | 格式 §7 vs §10.2 | P0 | "Footer 完整=已提交"与"Footer 有效但 IndexTxn 坏=已提交"两处口径自相矛盾 |
| R4 | 格式 §10.4 | P1 | 尾部 Footer 反向搜索窗口无法覆盖大未提交尾部，退化策略需 fuzz |
| R5 | 格式 §12 / recovery.go | P0 | Open 若沿用 ReadAll 读整文件，单文件含数据导致内存放大百倍 |
| R6 | 格式 §10.1 / iofile | P2 | 截断尾部时机与 mmap 映射的生命周期（含 Windows）需进矩阵 |
| R7 | API §10 / writer.go | P0 | v1 只允许唯一 FULL（id 恒为 1）；"FULL checkpoint"在现代码不可产生 |
| R8 | 格式 §6 / recovery.go | P1 | v1 的 IndexTxn offset 字段是 informational；v2 变权威校验字段，出错即无法打开 |
| R9 | 格式 §8 / writer.go | P2 | 单次 fsync 与 fault 注入点位需要按 v2 提交顺序重排（不再沿用 v1 点位） |
| R10 | 格式 §8 | P2 | AsyncCommit 无 fsync 时 footer 搜索对写序的依赖需写明（可丢、不可撕裂） |
| R11 | 格式 §13 / seal.go | P0 | GCM nonce 若用 TxnSequence 与 BlockID 同域，数字相等即碰撞：必须显式域分离 |
| R12 | 格式 §13 | P1 | 加密时 Footer 的 IndexTxnCRC32C 必须覆盖落盘密文（无密钥可检撕裂），并保证扫描不需密钥 |
| R13 | 格式 §10.2 | P2 | 内存重建索引需要密钥，加密 store 的 Open 契约要写清（已 ErrKeyRequired，无需改） |
| R14 | 格式 §5 / 计划 M1 | P1 | Block RowID envelope 若只进磁盘头，批量 planner 无内存过滤收益；建议由 RowIndexEntry 派生 |
| R15 | 格式 §4 | P2 | v2 Feature Bits / CheckVersion 0x0F 掩码需随 v2 能力重定义 |
| R16 | 格式 §17 | P2 | 所有 v2 固定结构必须保持 8 字节对齐，footer 搜索按对齐扫描才成立 |
| R17 | API §4/§11 / read_batch.go | P1 | v1 ReadBatch 是"整批报错+重复读取"；v2 改成"跳过+去重"，两代际语义漂移未声明 BREAKING |
| R18 | API §4 | P2 | BatchOrderRowID 并行解压后有界重排缓冲上界未定义 |
| R19 | API §6 | P2 | 并行预读 + ctx 取消的协程泄漏、MaxBytes 语义、Stats 并发需定义 |
| R20 | API §10 | P2 | Rewrite 目标已存在、源仍打开、加密重算等行为未定 |
| R21 | API §2 | P2 | Create/Open 对 `.rpk` 后缀的行为必须定死（防 foo.rpk.rpk） |

---

## A. 打开 / 恢复路径（recovery.go 系）

### R1（P0）恢复扫描器不识别 IndexTxn

**现状**：`scanDataFile`（recovery.go:151）在快照内部按"BlockHeader + StoredSize"步进找
Footer，遇到不认识的结构时：
`bh.Unmarshal` 失败 → 检查是否是 SnapshotHeader → 都不是则调
`hasLaterValidSnapshot`（recovery.go:241）判断后面有没有完整快照，有就报
"mid-file corruption"。

**后果**：v2 中最后一个 Block 与 SnapshotFooter 之间夹着 IndexTxn。扫描器在 IndexTxnHeader
（magic `RPITXNBH`，不是 `RPKBLOCK`/`RPKSNAPH`/`RPKSNAPF`）处三种 magic 都不匹配，
而 SnapshotFooter 确实在后面 → `hasLaterValidSnapshot` 返回 true → **正常、无崩溃写入的 v2
文件在 Open 时报 mid-file corruption，直接挡住 V2-M2 闭环**。空 DELTA（0 个 Block）时
IndexTxnHeader 紧跟 SnapshotHeader，同样命中。

**决议（写进格式 §10.1）**：扫描步进协议必须识别四类结构：

```text
SnapshotHeader (RPKSNAPH)  → 开始新 snapshot
BlockHeader    (RPKBLOCK)  → 按 StoredSize 跳过 payload
IndexTxnHeader (RPITXNBH)  → 按 BodyBytes 跳过，随后必须出现 IndexTxnFooter (RPITXNEF)
SnapshotFooter (RPKSNAPF)  → 结束当前 snapshot
```

未知结构出现在最后一个有效 Footer 之前 → 中间损坏硬错；之后 → 未提交尾部。

### R2（P0）重放架构：从"尾部截断"变成"逐 snapshot 重建并继续"

**现状（v1 已移除）**：v1 的 `index.Replay`（internal/index/replay.go，已随 v2 删除）一旦某个 txn 校验失败，就
`TailIgnored = len(remaining)` 停止，把其后的所有 txn 视为尾部丢弃。这在 v1 成立，因为
`.rpi` 是纯派生文件，坏一点即可整体丢弃重建。

**后果**：v2 中 IndexTxn 与数据同文件。中间某个 snapshot 的 IndexTxn 位腐（bitrot）后，
其后**已提交**的 snapshot 必须照常打开（§10.2 已承认这一点），也就是：

- 重放不能再"顺序读 + 遇坏即停"；
- 必须按 committed Footer 链逐 snapshot 取其 IndexTxn 区段校验；坏者仅对该 snapshot
  用 Blocks 内存重建，然后**继续处理下一个**；
- 因此 §9 的"重放到现有 index.View"与 v1 `Replay()` 不是一个算法，需要新实现（不可复用作
  原样函数），并有配套崩溃/位腐注入测试。

**落地（v2）**：上述新实现即 `Store.recover`（recovery.go）的逐 snapshot 重建/继续逻辑
与 `View.Apply`；v1 的 `index.Replay`/`DataFooterReader` 实现已删除，不再存在。

**边界强制（防跨 txn 消费垃圾）**：对每个 IndexTxn 必须成立：

```text
IndexTxnEndOffset == IndexTxnStartOffset + IndexTxnHeaderSize + BodyBytes + IndexTxnFooterSize
SnapshotEndOffset  == IndexTxnEndOffset + SnapshotFooterSize        （无填充时）
```

任一不满足即视为该 txn 损坏（走重建），绝不从错误偏移继续解析。BodyBytes 有 Header/Footer
双重来源（IndexTxnHeader.BodyBytes 与 IndexTxnFooter 内的区间），两者必须一致。

### R3（P0）提交权威口径自相矛盾

§7 说 "Footer 完整且 CRC 正确，表示数据与索引已作为一个事务提交"；§10.2 又说 "若 Footer
和 Blocks 有效但 IndexTxn 校验失败：数据提交事实仍由 Footer 决定"。落地时必须二选一并钉死。
建议条款（写进 §7）：

> **提交判定只看两件事：** ① SnapshotFooter 自身 FooterCRC32C 有效；② 其记录的所有 offset
> （Start/End/Previous）自洽且在文件范围内。其余一切字段（BlocksCRC32C、IndexTxnCRC32C）
> 是**绑定提示**而非提交条件：绑定失败只改变索引可用性（触发内存重建），不改变快照是否提交。
> Block 数据损坏（块头/密文认证/解压长度/Raw CRC）在任何情况下都是硬错误，不得因索引可重建而
> 跳过。

即术语上区分：**提交事实**（footer 自身完整性）与**索引有效性**（IndexTxn 绑定）是两个
独立维度，交叉组合都要有定义。

### R4（P1）Footer 尾部反向搜索窗口

§10.4 的"从文件尾按固定对齐向前搜索 Footer Magic，有限窗口内找不到时扩大搜索或退化为顺序
扫描"有两点风险：

1. **未提交尾部可能非常大**：一整个 snapshot 的 Blocks（块数无上界）之后断电，尾部尺寸 =
   全部未提交 Block 数据。任何固定窗口都可能覆盖不到最后一个有效 Footer；
2. 反向逐 8 字节搜索大文件很贵；"退化"成本摊销需要明确。

由于 v2 Open 反正要逐 snapshot 重放全部 IndexTxn（R2），**正向物理结构扫描（v1 已有实现，
改造后正好复用 R1 的协议）几乎免费**。建议：Open 主路径用正向扫描定位全部 committed
Snapshot；反向搜索仅作为可选项，且必须对"撕裂 Footer、伪 magic、超大尾部"做 fuzz。

### R5（P0）Open 内存回归：不得 ReadAll 单文件

**现状**：recovery.go:69 `idxData, err := s.index.ReadAll()` 把整个 `.rpi` 读入内存再重放
（1M 行约 22 MiB）。

**后果**：v2 若沿用 `s.data.ReadAll()` 重放，读入的是**包含全部数据 Block 的单文件**
（1M 行可达数百 MiB），Open 峰值内存暴涨。§12 "Open Replay 基本持平" 在该前提下不成立。

**决议（写进 §9/§12）**：打开时只按 Footer 链 / 物理扫描取得的每个 IndexTxn 区段做
ReadAt（或依赖 mmap 按需分页，常驻内存仍只含索引），**禁止整文件 ReadAll**；并在 V2-M2
加"Open 峰值内存"基准门槛。

### R6（P2）截断尾部与 mmap 生命周期

`iofile.Truncate` 在 `f.Truncate(n)` 之后才 `mapper.unmap()`：Unix 上映射覆盖越过新 EOF
不报错（访问才 SIGBUS），unmap 随后发生所以安全；但 Windows（mmap_other 退化路径）以及
"读写 Open 边截断边有并发读"的组合需要进 M3 故障矩阵（文档测试矩阵已有平台维度，补上
"尾部截断 + 活动中 mmap/读"）。

---

## B. 提交 / 一致性（writer.go 系）

### R7（P0）v1 只允许唯一 FULL，"FULL checkpoint"无法产生

**现状**：writer.go:161-163

```go
id := s.lastSnapshotID.Add(1)
if typ == SnapshotFull {
    id = 1 // first snapshot is FULL and its ID is 1
    s.lastSnapshotID.Store(1)
}
```

任何 `BeginSnapshot(SnapshotFull)` 都强制拿 id=1；若此前已有快照，重放时
"snapshot 1 already committed" → 提交失败。**v1 实际上只有一个 FULL（首个快照）**。

**后果**：GO_API_DESIGN_V2 §10 的 "FULL checkpoint：上层读取完整可见状态后写入一个新的
FULL Snapshot" 与 BINARY_FORMAT_V2 §7 "FULL 的 ParentSnapshotID=0" 在现有代码下根本无法
发生；若不改 writer，v2 的 checkpoint/压实能力是空中楼阁；若改，则 `view.Apply` 的 FULL
判定（view.go 只拒绝"FULL 带 parent"，没有拒绝重复 FULL 的 ID 递增）、Footer 链
"SnapshotID 单调" 检查均需适配（FULL 之后 ID 仍递增，单调性自然成立）。

**决议（写进 §7）**：v2 显式允许任意时刻提交 ParentSnapshotID=0 的新 FULL，SnapshotID 取
全局递增计数器（废弃 v1 的 id=1 强制），Depth 重置为 1，可见性不再沿祖先链解析。该语义
同时影响 §10.4 "SnapshotID 单调"校验与 §9 重放——需要在 M3 加"后续 FULL + 之后跟 DELTA"
的往返与崩溃测试。

### R8（P1）IndexTxn offset 字段从 informational 变权威

recovery.go:403 注释明确 v1 的 `TxnStartOffset/TxnEndOffset` 是 informational（重放靠
walk 推导）；writer 提交时甚至传 `txnEnd+int64(0)`。v2 中 Footer 的 IndexTxnStart/End 是
**校验字段**（R2 的边界强制依赖它），一处偏移计算错（对齐、padding、StoredSize 口径）就
会让整库无法打开——而同样的错在 v1 只是留了个没人读的数。

**决议**：单一 offset 计算函数（header→blocks→indextxn→footer→snapEnd 顺序推进），并在
每个提交点断言 `next.SnapshotStart == prev.SnapshotEnd`；同时钉住语义：IndexTxn 区段
`[IndexTxnStartOffset, IndexTxnEndOffset)` 落在 BlocksEnd 与 Footer 之间，`DataEnd` 沿用
v1 定义为 SnapshotFooter 之后（使 `dataFooterVerifier` 的
`footer = dataEnd - SnapshotFooterSize` 无需改动）。

### R9（P2）fault 注入点位需按 v2 顺序重排

v1 点位命名沿双文件提交（`commit.data-header.before`、`commit.data-sync.*`、
`commit.index.before`、`commit.index-sync.*`、`commit.publish.*`）。v2 提交变成
header→blocks→indextxn→footer→单次 sync→publish，点位要重排并新增
`commit.indextxn.append.*` / `commit.snapshot-footer.*`，且 M3 故障矩阵必须用 v2 点位
重做（沿用 v1 点位清单会漏掉 index append 撕裂场景）。

### R10（P2）AsyncCommit 的写序依赖

单文件多次 WriteAt 后不 fsync：同一文件系统缓存通常按提交顺序回写，但这是实现行为不是
契约。文档应写明：AsyncCommit 允许掉电丢失最近一次提交（尾部截断），但绝不产生
"数据已落、IndexTxn 未落"的可读中间态——因为两者在同一事务内、且无 Footer 时整体不可见；
Footer 可见而数据缺失等同数据损坏（硬错）。搜索策略不应假设"无 fsync 也顺序落盘"。

---

## C. 加密（seal.go 系）

### R11（P0）GCM nonce 域分离必须落在 nonce 位上，不能只靠 AAD

**现状**：`seal.Nonce(epoch, blockID) = KeyEpoch(4B) ‖ BlockID(8B)`（seal.go），全 store
单 BlockID 计数器，KeyEpoch 恒为 0。

**后果**：若 IndexTxn nonce 采用 `KeyEpoch ‖ TxnSequence` 且 TxnSequence 与某个 BlockID
数字相等 → nonce 完全复用 → AES-GCM 灾难性失败（可伪造）。AAD 不同不免疫 nonce 复用。

**决议（写进 §13）**：索引域与块域在 **nonce 字段内部**分离，例如：

```text
Block nonce : KeyEpoch(31bit) ‖ 0 ‖ BlockID(8B)
Index nonce : KeyEpoch(31bit) ‖ 1 ‖ TxnSequence(8B)   // bit31 = 域标志
```

AAD 继续绑定 StoreUUID、SnapshotID、区段 offset、长度，但 nonce 唯一性不依赖 AAD。

### R12（P1）Footer 的 IndexTxnCRC32C 覆盖密文（落盘字节）

若对明文计算 CRC，则撕裂/位腐的密文要等解密时才能检出，而 Footer 校验本身应在无密钥
路径完成（Header/Footer 保持明文导航字段）。决议：IndexTxnCRC32C 一律对**落盘字节**计算
（加密 store 即密文），使"Footer 完整但 IndexTxn 密文撕裂"在无密钥时即可判定 → 走 R2 的
内存重建。同时保证 R1 扫描协议只需读固定结构 + BodyBytes 跳过，**永不需要密钥**。

### R13（P2）内存重建需要密钥

§10.2 重建要读并解压 Blocks（`buildIndexTxnFromData` 路径）→ 加密 store 必须持有
decrypter。现状已强制 `ErrKeyRequired`（Open 无 KeyProvider 直接失败），无需新约定；文档
补一句"重建走只读解密路径（decrypter），不依赖写路径 encCipher"即可。

---

## D. 格式细节

### R14（P1）Block RowID envelope：进内存索引，不进磁盘块头

计划 M1 "扩展 Block Header 的 RowID 包围范围"有两个问题：

1. 批量 planner 要在**不读盘**的情况下按 envelope 过滤候选 Block（格式 §11 批量读取流程
   第 5 步），envelope 必须来自内存索引（view.BlockLoc），磁盘块头只有读盘后才看得到——
   放进磁盘块头对批量规划没有帮助；
2. 64B 块头中加密已占用保留区（KeyEpoch@56），再加 2×uint64 需涨到 80B，所有 golden
   重录。

**建议结论**：envelope 由 `view.Apply` 阶段按 RowIndexEntry 集合派生
per-(Snapshot, Table, Block) 的 MinRowID/MaxRowIDExclusive，磁盘块头不加字段；metadata
block 无 envelope；MaxRowIDExclusive 在 MaxRowID=MaxUint64 时无法 +1（溢出），固定用 0
表示"无上界"（RowID 0 永不合法，writer 已拒绝），空 Block 不产生。此项对应 §17 决策点，
给出结论后 M4 才能冻结。

### R15（P2）v2 Feature Bits 与 CheckVersion 掩码

当前 `CheckVersion` 用 `^(0x0F)` 掩码拒绝未知 RequiredFeatures（headers.go）。v2 需重新
定义位图（单文件索引是内置能力而非 feature），否则旧掩码与 v2 位分配冲突。属 M0 冻结项，
列出即可。

### R16（P2）8 字节对齐是 Footer 搜索的前提

§10.4 按固定对齐向前/向后搜索依赖所有固定结构为 8 的倍数；新增 Footer 字段、IndexTxn
字段时必须保持对齐（当前 entry 尺寸 40/48/56/72 均满足）。用常量测试守护（M0 已有，扩展
到 v2 结构）。

---

## E. 批量读取 API

### R17（P1）两代际批量语义漂移必须声明 BREAKING

v1 `ReadBatch`（read_batch.go:24-28）：重复 id **读两次**、任一 id missing/deleted
**整批报错**。GO_API_DESIGN_V2 §4/§11 建议："输入先去重，每个可见 Row 只返回一次" +
"默认跳过不可见 RowID"。这是行为级不兼容：

- 同一仓库两个代际对同一调用有不同结果，升级方无感知；
- "去重 + BatchOrderInput"破坏输入下标 → 输出行的 1:1 映射（[5,5,7] 去重后第 2 行是 5
  还是 7？）。

**决议**：v2 批量 API 明确声明与 v1 ReadBatch 的差异；建议默认**保留重复输入重复返回**
（与 v1 一致、映射 1:1），去重由调用方预处理或另设选项；"跳过不可见行"作为显式选项或仅
在 Stats 中体现差异（RequestedIDs vs RowsReturned），并禁止静默行为切换。

### R18（P2）BatchOrderRowID 的重排缓冲上界

批量并行 decode 后按 RowID 排序输出需要有界缓冲；上界未定义 → 整批 100k 行随机读时重排
缓冲可能 == 全请求量（内存失控）。建议钉死：缓冲行数 ≤ MaxRows 或
PrefetchBlocks×平均 ItemCount，超限报 `ErrBatchLimit`。

### R19（P2）取消、资源、统计语义

- `Parallelism>0` 时 ctx 取消必须 drain worker 并释放预读缓冲，需 goroutine-leak 测试；
- `MaxBytes` 语义在 §12 决策点三选一（返回值/工作集/两者）；
- `BatchIterator.Stats()` 若与 Next 并发调用需声明不支持（或原子累计）。

---

## F. 其他

### R20（P2）Rewrite 边界

- target 已存在：必须 ErrAlreadyExists（禁止覆盖），避免误删数据；
- source 处于 Open 状态：文档说明 Rewrite 内部以只读方式打开 source（复用 v2 Open），
  与调用方自身持有的句柄互不干扰；成功后不删除源（文档已定）；
- 加密 store：重压缩 + 重加密，AAD 重新绑定，KeyProvider 生命周期要写清。

### R21（P2）`.rpk` 后缀行为定死

v1 拒绝带扩展名的 basePath；v2 单文件若 Create("foo.rpk") 生成 "foo.rpk.rpk" 是经典 bug。
M0 决策点 #6 给结论：建议 Create/Open 接受 `.rpk` 后缀的完整路径（is the file），不接受时
必须报清晰错误而不是拼接。

---

## 实施顺序建议（并入 DEVELOPMENT_PLAN_V2）

```text
R1/R5 → V2-M1（扫描协议 + 重放区段化）  必须先于 M2 闭环
R2/R3/R8 → V2-M2/M3                       提交权威与逐快照重放是 M3 恢复的骨架
R7 → V2-M3                                “后续 FULL”语义进入恢复测试
R11/R12 → V2-M6                           nonce 域分离与密文 CRC 是加密里程碑的验收项
R14/R17/R18 → V2-M4                       envelope 来源与批量语义在 M4 前冻结
```