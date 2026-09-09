# RowPack 核心 API 草案（流式写入版 · 定稿候选）

> 状态：**已实现**（2026-09-08；`BlockInfo` 按评审意见定名为 `Block`）
> 日期：2026-09-08（第七稿）
> 使用场景：
> ① 全量快照：源库二维表逐行保存；
> ② 源库 ↔ RowPack diff：Blocks 块级主键高低位切批，RowPack 侧 Scan 范围读、源库侧 PK Range 查询；
> ③ diff 逐条产出、逐条写入增量快照（调用方零累计，内存 O(块大小)），Commit 原子生效。
> 约束：不改变磁盘字节布局；无历史包袱。

```go
package rowpack

// ---------- 生命周期 ----------

func Create(basePath string, opts Options) (*Store, error)
func Open(basePath string, opts Options) (*Store, error)
func (s *Store) Close() error

func (s *Store) BeginFull(ctx context.Context) (*Writer, error)                     // 全量
func (s *Store) BeginDelta(ctx context.Context, parent SnapshotID) (*Writer, error) // 增量
func (w *Writer) Commit(ctx context.Context) (SnapshotID, error)                    // 全部变更原子生效
func (w *Writer) Abort() error

// ---------- 表 ----------

// 表名即身份；TableID / schema 版本由引擎内部分配
func (w *Writer) CreateTable(name string, columns []Column) error

// ---------- 保存数据：逐条流式写入（RowID = 业务主键） ----------
//
// diff 产生一条调一条，Writer 内部按块缓冲（256 KiB 目标，写满即落盘），
// 调用方内存占用 O(块大小)，与 diff 总量无关；Commit 时原子生效。

func (w *Writer) Insert(ctx context.Context, table string, rowID RowID, row Row) error
func (w *Writer) Update(ctx context.Context, table string, rowID RowID, row Row) error
func (w *Writer) Delete(ctx context.Context, table string, rowID RowID) error

// ---------- 读取 ----------

func (s *Store) Get(ctx context.Context, snap SnapshotID, table string,
	rowID RowID, dst Row) (Row, error)

// 主键范围扫描 [Start, End)：父链合并后的可见行，RowID 升序
func (s *Store) Scan(ctx context.Context, snap SnapshotID, table string,
	opts ScanOptions) (*Iterator, error)

// ---------- 分块（对比时的批量规划依据） ----------

// 该快照自身写入的块，物理顺序；MinRowID/MaxRowID 即每块主键高低位
func (s *Store) Blocks(ctx context.Context, snap SnapshotID, table string) ([]BlockInfo, error)

// 按块号区间 [lo, hi) 顺序扫描原始变更流（备份/增量导出用）
func (s *Store) ScanBlocks(ctx context.Context, snap SnapshotID, table string,
	lo, hi uint64) (*Iterator, error)

// ---------- 迭代器 ----------

func (it *Iterator) Next() (Row, bool)      // 返回的 Row 到下一次 Next 前有效
func (it *Iterator) RowID() RowID
func (it *Iterator) ChangeType() ChangeType // Scan 恒非 DELETE；ScanBlocks 为原始变更
func (it *Iterator) Err() error
func (it *Iterator) Close() error

// ---------- 类型 ----------

type (
	SnapshotID uint64
	RowID      uint64 // 即业务主键
	ChangeType uint8  // 1=Insert 2=Update 3=Delete
)

type ScanOptions struct {
	Start RowID // 含
	End   RowID // 不含；0 = 无上界
}

type BlockInfo struct {
	BlockID     uint64
	ItemCount   uint32
	MinRowID    RowID // 含 —— 主键低位
	MaxRowID    RowID // 不含 —— 主键高位
	RawBytes    uint32
	StoredBytes uint32
}

type Column struct {
	Name     string
	Type     Type  // TypeBool…TypeDecimal 共 17 种
	Nullable bool
	Scale    int32 // 仅 TypeDecimal
}

type Row []Value // rowpack.Int64(1), rowpack.String("a"), rowpack.Null()…
```

## 场景对照

1. **全量**：`BeginFull → CreateTable → 逐行 Insert → Commit`
2. **对比**：`Blocks` 拿每块 `[MinRowID, MaxRowID)` 切批 → RowPack 侧 `Scan(Start,End)`
   ↔ 源库侧 `WHERE pk >= ? AND pk < ?` → diff 逐条产出
3. **差异应用**：`BeginDelta(parent)` → 逐条 `Insert/Update/Delete`（不累计）→
   `Commit`（原子生效，失败 `Abort` 无残留）

## 实现替换对照（内部改造点）

| 现有 | 改造为 |
| --- | --- |
| `BeginSnapshot(ctx, typ, SnapshotOptions)` | `BeginFull` / `BeginDelta` |
| `DefineSchema(Schema{TableID, Version, …})` | `CreateTable(name, cols)`，ID/版本引擎分配 |
| `Insert/Update(ctx, table, rowID, schemaVersion, row)` | 去掉 schemaVersion，`table` 改表名（内部 name→TableID 解析缓存） |
| `Commit(ctx) (SnapshotInfo, error)` | `Commit(ctx) (SnapshotID, error)` |
| `Get/Scan` | 保留；`ScanOptions` 字段定名 Start/End |
| 新增 `Blocks` / `ScanBlocks` | 基于 index.View 块定位 + R14 内存派生行范围 |
| 批量读/写入口（`ReadBatch`、`Apply` 等） | 降为内部路径，公开面待后续单议 |
