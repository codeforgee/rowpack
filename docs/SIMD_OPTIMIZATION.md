# RowPack SIMD 优化指南

## 目标与边界

RowPack v1 是行式、变长、强校验格式。SIMD 优化必须保持磁盘字节、错误语义与跨平台行为不变。
两层证据：`make bench-simd` 衡量候选内核，`make baseline` 验证端到端 Get / Scan / ReadBatch /
写入 / 恢复无回退。只在相同 CPU、Go 版本、数据集和配置之间比较。

## 已有硬件加速

`hash/crc32` 的 CRC32C 在受支持 arm64/amd64 上使用硬件 CRC 指令；`klauspost/compress/zstd` 自带
arm64/amd64 优化，RowPack 不重复实现压缩内核；Go 标准库与 runtime 已优化 AES-GCM、`copy`、
`memmove`、`memclr`。这些路径通过依赖升级与端到端基准维护，不在 RowPack 内复制汇编实现。

## 候选路径

### Rows Page 元数据

`ParseRowsPage` 解码 RowID delta、tuple end-offset delta 与 schema RLE。Varint 边界与前缀和造成
串行依赖，主循环不能直接向量化。当前安全策略是先做 portable SWAR：每字节同时检查四个 2-bit
change type，并把 RowID min/max 合入首次解码。

未来可为「连续单字节 Varint」增加批量 fast path，但必须保留通用解码与完整损坏检测。Stream
VByte / Group Varint / bit packing 必须作为磁盘格式 v2 设计，不能静默改变 v1。

### Tuple 与 Scan

逐行 `DecodeInto` 含 NULL、类型 switch、长度检查和变长字段，手写 NEON/AVX 收益有限。显著 Scan
提升的正确方向是新增批量接口：按相同 schema 聚合一组 record，把固定宽度列解码到连续 typed
vectors。这会改变上层 API、内存所有权与过滤执行方式，应单独设计，并以 fixed-width、string-
heavy、NULL-heavy 三种数据集验证。

schemaIndex 构建时为每个 schema version 编译唯一的 schema-bound `codec.Decoder`，把 schema
上限检查、bitmap geometry 与内核选择移出读取路径。Get / Scan / ReadBatch / Verify 只消费不可变
执行计划，不再保留临时准备 decoder 或按 schema 直接解码的兼容路径。

schema 全为非 NULL fixed-width 时，prepared decoder 启用专用路径：行级一次检查总长度与 NULL
bitmap，随后直接加载连续 payload，省去逐列 bounds check、宽度计算与通用函数调度。异常 bitmap、
bool、time 或长度仍回退完整校验路径，损坏检测语义不变。

其余 schema 走 `decode_plan.go` 的**列计划**：编译期为每列预解 opcode（dense jump table）、
payload 宽度、bitmap 字节与掩码，行解码循环因此无 `col.Type` switch、`fixedWidth` 查表与
`need()` 闭包；NULL 位、截断、bool/time/UTF-8 异常与 limits 一致则直接接受，否则本行回退通用
校验路径产出原有错误。实测混合 schema（含 String/Bytes）单行解码 -58%，固定宽度 -5%；端到端
Get 热读 -34%、Scan -50%、ReadBatch -22%（`make bench-simd` 收录 `PreparedDecodeMixed`）。

`Decoder.DecodeBatchInto` 接收同 schema 的多个 body，结果写入连续 `Value` slab。`ReadBatch` 在
每个 page 内按 schema version 和最多 128 行组块，用栈上 body/output 索引数组提交批次：保持返回
顺序且不增加 heap allocation。固定宽度批次复用紧凑内核，变长或 nullable 批次仍逐行执行完整校验
与 Sink materialization。

### 不建议独立优化

单行热 Get（主要是索引、缓存与函数调度，数据规模不足以摊薄 SIMD）；NULL bitmap（常用 schema
列数太少）；CRC、Zstd、AES 与内存复制（底层已选硬件实现）。

## 基准使用

```sh
make bench-simd BENCHTIME=2s BENCHCOUNT=5
make baseline MODE=full BENCHTIME=2s BENCHCOUNT=5
make baseline-diff OLD=<旧归档> NEW=<新归档> THRESHOLD=5
```

`bench-simd` 当前包含：`BenchmarkRowsPageParse32K`（CRC、change stream 校验与三个元数据流
展开）；`BenchmarkRowsPageRecords32K`（顺序遍历 page）；`BenchmarkValidateChangeBits`（适合
SWAR/SIMD 的 packed 2-bit 校验内核）；`BenchmarkValidateChangeBitsScalar`（优化前逐记录算法，
同进程性能对照）；`BenchmarkPreparedDecodeBody`（schema-bound 单行固定宽度内核）；
`BenchmarkPreparedDecodeBatch128`（128 行连续 slab 的多行内核吞吐与分配）。

微基准改善不代表用户路径改善。合入标准：端到端目标场景有可重复收益，且其他核心指标、分配数、
损坏检测与跨架构测试无显著回退。

## 实施路线

1. v1 无格式变化：portable SWAR、循环融合、减少 pass 和分支；
2. 可选架构 fast path：仅当 profile 证明内核占比足够高时才加 arm64/amd64 汇编；
3. 批量 Decode API：以 Scan/ReadBatch 为主要受益方；
4. v2 研究：列式 mini-page、bit-packed delta 或 Stream VByte。
