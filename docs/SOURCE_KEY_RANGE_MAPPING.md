# 源库 Key Range 到 RowPack 批量读取的映射

> 状态：设计建议；§9 决策记录（v1.2）已固化实现边界（2026-09-09）
> 适用场景：源数据库按 `[lo, hi)` 查询，RowPack 按快照批量分块读取

## 1. 问题定义

源数据库中的范围查询通常表示业务 Key 范围：

```sql
WHERE primary_key >= :lo AND primary_key < :hi
```

而 RowPack 当前的 `RowID` 是表内稳定的逻辑行标识，与源数据库主键相互独立。因此不能默认把：

```text
source key ∈ [lo, hi)
```

直接解释为：

```text
RowID ∈ [lo, hi)
```

只有在导入时明确保证 RowID 与源 Key 同序并建立对应关系时，二者才可以作为一种优化路径互相转换。

## 2. 推荐分层

```text
Source Key Range [lo, hi)
        │
        ▼
Key → RowID 映射 / Range Index
        │
        ▼
RowID 集合或 RowID 区间
        │
        ▼
RowID → BlockID 定位
        │
        ▼
Block 去重、排序、批量读取、解压
```

各层职责如下：

- 源数据库适配层理解主键、复合键、排序规则和 NULL 语义；
- RowPack 保存 Canonical Schema、行数据和稳定 RowID；
- Key Index 负责把源 Key 映射到 RowID；
- RowPack 批量读取路径负责按 Block 聚合 I/O、解压和校验。

RowPack 核心不应假设某个外部数据库的主键或排序语义。

## 3. 推荐实现

导入时同时保存：

```text
RowID       → RowPack 内部稳定身份
source_key  → 源数据库主键，作为普通 Canonical Schema 列
key_index   → source_key 到 RowID 的旁路映射
```

一次范围读取可以执行：

1. 对 `[lo, hi)` 使用源 Key 的比较规则查询 Key Index；
2. 得到匹配的 RowID 集合；
3. 对 RowID 去重并按 RowID 或 BlockID 排序；
4. 根据 Row Index 找到涉及的 Block；
5. 每个 Block 只读取、解压和校验一次；
6. 返回结果时恢复调用方需要的 Key 顺序或 RowID 顺序。

例如：

```text
source_key ∈ [1000, 2000)
        ↓
RowID = [7, 9, 13, 28, ...]
        ↓
BlockID = [2, 3, 5]
        ↓
读取 Block 2、3、5，并批量解码目标行
```

## 4. RowID 保持源 Key 顺序的优化路径

如果导入总是按照源 Key 升序执行，可以让 RowID 大致保持源 Key 顺序，并记录范围映射：

```text
source key range → RowID range
```

这可以作为快速路径，但不能成为通用语义，因为：

- 后续插入可能位于已有 Key 中间；
- 删除会产生空洞；
- 主键修改需要更新映射；
- FULL 与 DELTA 导入可能不再保持全局顺序；
- 复合键的字典序需要额外定义。

因此 RowID 仍必须保持独立身份，范围映射只能作为可失效、可重建的辅助索引。

## 5. 复合主键

多主键的 `[lo, hi)` 应被解释为主键元组的半开区间，而不是每一列分别独立限制：

```text
lo = (10, 20)
hi = (20, 30)

(10, 20) <= (k1, k2) < (20, 30)
```

对应的字典序规则是：

```text
(a1, a2) < (b1, b2)
当且仅当：
  a1 < b1
  或 a1 == b1 且 a2 < b2
```

因此它等价于：

```sql
(k1 > :lo1 OR (k1 = :lo1 AND k2 >= :lo2))
AND
(k1 < :hi1 OR (k1 = :hi1 AND k2 < :hi2))
```

不能错误地转换成：

```sql
k1 >= :lo1 AND k1 < :hi1
AND k2 >= :lo2 AND k2 < :hi2
```

后者表示矩形区域，语义不同，会漏掉或错误包含部分 Key。

对于三列主键，规则继续递归：

```text
(a, b, c) < (x, y, z)
当 a < x，或 a == x 且 b < y，
或 a == x 且 b == y 且 c < z
```

RowPack 的 Range Index 应保存按规范元组 Key 排序的条目：

```text
EncodedTupleKey → RowID
```

查询时执行：

```text
Encode(lo) <= EncodedTupleKey < Encode(hi)
```

但只有在 EncodedTupleKey 的二进制字节序与元组字典序一致时，才能直接使用字节范围比较。否则必须使用解码后的逐列比较器。

复合主键应使用明确的字典序比较：

```text
(k1, k2) < (k1', k2')
```

但在建立 Range Index 前必须冻结以下规则：

- 每一列的 ASC/DESC 方向；
- NULL 是最小值还是最大值；
- 字符串按 UTF-8 字节序还是数据库排序规则；
- 数值的规范二进制编码；
- 日期和时间的时区处理；
- Decimal 的 Scale 与规范化方式；
- 前缀比较和长度比较规则。

如果无法完整复制源库的排序规则，应由源数据库适配器执行范围筛选，RowPack 只负责读取已经确定的 RowID 集合。

## 6. 快照一致性

Key Index 必须与数据属于同一个逻辑 Snapshot 版本：

```text
Snapshot S1
├── Canonical Schema
├── Rows
└── Key Index revision
```

Delta Snapshot 只保存 Key Index 的变化：

- 新增 Key：UPSERT；
- Key 修改：删除旧映射并 UPSERT 新映射；
- 行删除：DELETE；
- 未变化 Key：沿父快照继承。

读取 Snapshot S 时，Key Index 和 Row Index 必须从同一个 Snapshot 父链解析，不能使用最新 Key Index 去查询历史数据。

## 7. 与批量分块读取的关系

批量读取 API 更适合接收两类输入：

```go
ReadRowsByIDs(ctx, snapshot, table, rowIDs)
ReadRowsByKeyRange(ctx, snapshot, table, lo, hi, opts)
```

其中：

- `ReadRowsByIDs` 可以直接执行 RowID 去重和 Block 聚合；
- `ReadRowsByKeyRange` 需要上层 Key Index 或适配器提供 Key→RowID 解析；
- 两者最终都应汇聚到同一个 Block 批量读取内核。

Block 读取内核应做到：

- 同一个 Block 在一次请求中只读取一次；
- 同一个 Block 只解压一次；
- 同一个 Block 只做一次完整校验；
- 返回顺序由 API 选项决定，不影响物理读取顺序；
- 大范围读取可以退化为按 RowID 有序 Scan。

## 8. 当前版本边界

当前 RowPack 已有稳定 RowID、Row Index 和按 RowID 升序的 Scan，但尚未提供通用 Source Key Index 或 `ReadRowsByKeyRange` API。

因此当前推荐由源库适配层完成：

```text
源库 Key Range
→ 源库/适配器得到匹配主键
→ 转换为 RowID 集合
→ 使用 RowPack 的 Get/Scan 路径读取
```

后续 v1.2 若确认范围读取是主要访问模式，应优先实现按 RowID 集合聚合 Block 的内部能力，再决定是否提供通用 Key Index API。

> 注：按 RowID 集合聚合 Block 的能力已随 `Store.ReadBatch` 于 v1.2 完成
> （plan-v12.md §3.1）；是否提供通用 Key Index API 的最终决策见 §9。

## 9. 决策记录（v1.2）

> 日期：2026-09-09
> 关联：ADR-002（引擎解耦方言语义）、plan-v12.md §3.1（批量分块读取已完成）、
> GO_API_DESIGN.md §8（ReadBatch 公开契约）

### 9.1 采用外层适配方案（§8 路线），不内建 Key Index

选定实现路径：源库适配层负责 Key→RowID 解析，RowPack 只提供按 RowID 集合的批量读取。

- 语义正确性由源库自身的 PK 索引和排序规则保证，引擎不冻结任何方言排序规则；
- §5 所列冻结清单（ASC/DESC、NULL 位次、字符串排序、数值编码、时区、Decimal scale 等）
  全部归适配层，引擎不承担；
- 符合 ADR-002「引擎与『数据库是什么』解耦」的方向；
- v1.2 已完成 `Store.ReadBatch`（按块聚合、去重、单次读/解压/校验），所需引擎能力已齐备，
  本方案的引擎改动量为零。

### 9.2 API 边界：公开 `ReadBatch`，不公开 `ReadRowsByKeyRange`

- §7 的 `ReadRowsByIDs` 由**公开 API** `Store.ReadBatch(ctx, snapshot, table, ids []RowID)`
  承担（输入 Snapshot/Table/RowID 等逻辑对象、输出逻辑行、不暴露磁盘细节；
  外部适配器在模块外，`internal/` 对其不可见，因此批量读取必须是公开入口）；
- §7 的 `ReadRowsByKeyRange` **不进入引擎公开 API**：它携带 Key 与排序语义，属于
  适配器接口形态（对外暴露 `KeyRange → []Row`），内部用源库查询 + `ReadBatch` 实现；
- 按块 Load/CRC/解压/目录复用的读取内核保持在 `internal/block`、`internal/cache`，
  `Store.ReadBatch` 只是门面。

### 9.3 RowID 保序只作为适配层的数据布局优化

§4 的「RowID 保持源 Key 顺序」不作为引擎语义，但可作为适配层导入策略：

- FULL 导入按源 Key 升序写入、RowID 顺序分配，使 Block 布局与 Key 序对齐；
- 适配器在导入循环内记录每块的 min/max key，形成私有稀疏映射（可重建、可失效），
  把 `[lo, hi)` 压成候选 RowID 区间后再交给 `ReadBatch`；
- 该映射不进 `.rpk` 格式，也不进引擎。

### 9.4 升级到通用 Key Index 的门槛

以下两个条件**同时**满足才启动引擎内建 Key Index（方案 A），否则维持 9.1：

1. 真实场景基准证明按 Key 范围过滤是主导访问模式，且收益大于全量 Decode 成本；
2. 目标源库收敛到单一/少数方言，且能完整复制其排序规则（复合键、NULL、字符串排序）。

在此之前，`ReadRowsByKeyRange` 定义为适配器接口；需要批量读取性能优化时，
引擎侧优先补 `ReadBatchInto`（行缓冲复用，消除 plan-v12 §3.1 seq/hot 档回退），
而不是引入 Key Index。
