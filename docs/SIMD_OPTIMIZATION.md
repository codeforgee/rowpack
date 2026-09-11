# RowPack SIMD 优化指南

## 目标与边界

RowPack v1 是行式、变长、强校验格式。SIMD 优化必须保持磁盘字节、错误语义和跨平台行为不变。
项目采用两层证据：`make bench-simd` 衡量候选内核，`make baseline` 验证端到端 Get、Scan、
ReadBatch、写入和恢复没有回退。只在相同 CPU、Go 版本、数据集和配置之间比较结果。

## 已有硬件加速

- `hash/crc32` 的 CRC32C 会在受支持的 arm64/amd64 上使用硬件 CRC 指令；
- `klauspost/compress/zstd` 自带 arm64/amd64 优化，RowPack 不重复实现压缩内核；
- Go 标准库和 runtime 已优化 AES-GCM、`copy`、`memmove` 和 `memclr`。

这些路径应通过依赖升级和端到端基准维护，而不是在 RowPack 内复制汇编实现。

## 候选路径

### Rows Page 元数据

`ParseRowsPage` 解码 RowID delta、tuple end-offset delta 和 schema RLE。Varint 边界与前缀和造成
串行依赖，因此不能直接把主循环向量化。当前安全策略是先做 portable SWAR：例如每字节同时
检查四个 2-bit change type，并把 RowID min/max 合入首次解码。

未来可以为“连续单字节 Varint”增加批量 fast path，但必须保留通用解码和完整损坏检测。若要
使用 Stream VByte、Group Varint 或 bit packing，必须作为磁盘格式 v2 设计，不能静默改变 v1。

### Tuple 与 Scan

逐行 `DecodeInto` 包含 NULL、类型 switch、长度检查和变长字段，手写 NEON/AVX 收益有限。
获得显著 Scan 提升的正确方向是新增批量接口：按相同 schema 聚合一组 record，把固定宽度列
解码到连续 typed vectors。该工作会改变上层 API、内存所有权和过滤执行方式，应单独设计并以
fixed-width、string-heavy、NULL-heavy 三种数据集验证。

schemaIndex 构建时会为每个 schema version 编译唯一的 schema-bound `codec.Decoder`，把 schema
上限检查、bitmap geometry 和内核选择全部移出读取路径。Get、Scan、ReadBatch 和 Verify 只消费
不可变执行计划，不再保留临时准备 decoder 或直接按 schema 解码的兼容路径。

schema 全部为非 NULL fixed-width 类型时，prepared decoder 还会启用专用路径：在行级一次检查
总长度和 NULL bitmap，随后直接加载连续 payload，省去逐列 bounds check、宽度计算和通用函数
调度。异常 bitmap、bool、time 或长度仍回退完整校验路径，因此损坏检测语义不变。

`Decoder.DecodeBatchInto` 接收同 schema 的多个 body，并把结果写入连续 `Value` slab。`ReadBatch`
在每个 page 内按 schema version 和最多 128 行组块，使用栈上 body/output 索引数组提交批次；这既
保持返回顺序，也不增加 heap allocation。固定宽度批次复用紧凑内核，变长或 nullable 批次仍逐行
执行完整校验和 Sink materialization。

### 不建议独立优化

- 单行热 Get：当前主要是索引、缓存和函数调度，数据规模不足以摊薄 SIMD；
- NULL bitmap：常用 schema 列数太少；
- CRC、Zstd、AES 和内存复制：底层已经选择硬件实现。

## 基准使用

```sh
make bench-simd BENCHTIME=2s BENCHCOUNT=5
make baseline MODE=full BENCHTIME=2s BENCHCOUNT=5
make baseline-diff OLD=<旧归档> NEW=<新归档> THRESHOLD=5
```

`bench-simd` 当前包含：

- `BenchmarkRowsPageParse32K`：CRC、change stream 校验和三个元数据流展开；
- `BenchmarkRowsPageRecords32K`：顺序遍历 page；
- `BenchmarkValidateChangeBits`：适合 SWAR/SIMD 的 packed 2-bit 校验内核。
- `BenchmarkValidateChangeBitsScalar`：优化前逐记录算法，仅作为同进程性能对照。
- `BenchmarkPreparedDecodeBody`：schema-bound 单行固定宽度内核。
- `BenchmarkPreparedDecodeBatch128`：128 行连续 slab 的多行内核吞吐与分配。

微基准改善不代表用户路径改善。合入标准是端到端目标场景有可重复收益，且其他核心指标、
分配数、损坏检测和跨架构测试无显著回退。

## 实施路线

1. v1 无格式变化：portable SWAR、循环融合、减少 pass 和分支；
2. 可选架构 fast path：只有 profile 证明内核占比足够高时才增加 arm64/amd64 汇编；
3. 批量 Decode API：以 Scan/ReadBatch 为主要受益方；
4. v2 研究：列式 mini-page、bit-packed delta 或 Stream VByte。
