# 数据分块加密：分步开发计划

> 状态：S1–S8 已完成；决策 C 已定（随机冷点读记为已知基线，优化列为快速跟进）
> 日期：2026-09-09
> 前提：格式尚未发布，加密作为创建时可选能力直接进入 v1 格式，
> 无老格式兼容约束（DATA_BLOCK_ENCRYPTION_FEASIBILITY.md §10，2026-09-09 修订）
> 目标版本：v1.3（与 v1.2 ReadBatch 相同代码行，磁盘结构扩展）

## 0. 已完成的配套决策

- 方案：Block 级 AES-256-GCM，先压缩后加密；导航字段（Header/Footer/索引）明文。
- nonce：NonceScheme=1，96-bit = KeyEpoch(4B) ‖ BlockID(8B)，确定性、零每块 KDF。
- AAD：BlockHeader 全部字段 + StoreUUID；不绑 SchemaVersion / ParentID。
- 缓存：默认缓存明文（复用现有 LRU），"仅缓存密文"为可选策略不做。
- 打开契约：加密 Store 的 Open / Verify / RebuildIndex 必须提供 KeyProvider，
  否则 ErrKeyRequired；不做"半可用"状态。
- 错误：ErrKeyRequired / ErrKeyUnavailable / ErrKeyIDNotFound / ErrAuthFailed
  （认证失败挂 CorruptionError）。

## 1. 实施步骤概览

| 步骤 | 内容 | 产物 |
| --- | --- | --- |
| S1 ✅ | 格式层：FileHeader/BlockHeader 加密字段 + 常量 + 单测 | internal/fileformat |
| S2 ✅ | 加密封装：AES-GCM 块加密/解密、nonce 派生、AAD 构造 | internal/seal |
| S3 ✅ | API 与写路径：Options.Encryption、Create 写 header、commit 加密 payload | options.go / rowpack.go / writer.go |
| S4 ✅ | 读路径：Reader 注入 Decrypter，解密在解压前 | internal/block/reader.go / loader.go |
| S5 ✅ | Open/恢复/重建/校验接入：ErrKeyRequired、recovery、RebuildIndex、Verify | recovery.go / rebuild.go / verify.go |
| S6 ✅ | 集成测试：加密 Store 全功能 + 错误路径 + 篡改 + 崩溃恢复 + 并发 | 测试 |
| S7 ✅ | 格式文档同步：BINARY_FORMAT_V1.md、GO_API_DESIGN.md | docs |
| S8 ✅ | 性能基准与对比（加密 vs 未加密），报告 | benchmark + docs/perf-report-encryption.md |

每一步独立提交；S1–S7 以 `go test ./...` + `-race` 全绿为门槛。

## 2. S1 格式层

- `internal/fileformat`：
  - const：`EncNone=0`、`EncAES256GCM=1`（EncryptionAlgorithm）；
    `NonceNone=0`、`NonceCounterV1=1`；`KeyIDMaxLen=32`；`AESGCMTagLen=16`。
  - `FileHeader` 增加字段：`EncryptionAlgorithm`、`NonceScheme`、`KeyID []byte`（定长 32），
    marshal 写入 offset 64…（原 Reserved 区）；CRC 布局不变。
  - `BlockHeader` 增加字段：`KeyEpoch uint32`（Flags 无变化，add `BlockFlagEncrypted`? 不——
    BlockHeader.Flags 目前由结构体无字段暴露；需加 `Encrypted bool` 映射 offset 14 bit0）、
    无其他块头变化。
  - marshal/unmarshal round-trip 单测 + golden 影响评估（未加密全零，既有 golden 不变或更新）。

## 3. S2 加密封装 internal/seal

- `type AEADKey [32]byte`；`EncryptBlock(key, epoch, blockID, aad, plain) ([]byte, error)`；
  `DecryptBlock(...) ([]byte, error)`；`BuildAAD(uuid, header) []byte` 确定性序列化；
  `Nonce(epoch, blockID) [12]byte`。
- 若速度不足（每块 cipher.NewGCM 初始化），提供 per-key `*gcmManager` 缓存
  cipher.AEAD（单写者复用 / 读路径按 store 缓存）。
- 单元测试：round-trip、AAD 任一位篡改 → ErrAuthFailed、密文篡改 → ErrAuthFailed、
  nonce 变化解密失败、空 payload。

## 4. S3 API 与写路径

- 公开 API（GO_API_DESIGN 同步）：
  - `type KeyProvider interface { Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error) }`
  - `type EncryptionConfig struct { KeyProvider KeyProvider; KeyID string }`
  - `Options.Encryption *EncryptionConfig`（nil = 不加密，零值安全）。
- Create：若 Encryption!=nil → resolve 主密钥（`Key(keyID, 0)`）校验可行 → 写 header 加密字段。
- writer.commitLocked：写盘循环内——分配 BlockID 后，若加密：用 store 的 encryptor
  加密 blk.payload（更新 header.Encrypted、KeyEpoch=0、StoredSize）；先写 SnapshotHeader
  不受影响。注意加密发生在 FirstBlockID 确定之后（已是）。
- Store 持有写路径 encryptor（单写者无并发）。

## 5. S4 读路径

- `internal/block.Reader` 增加可选 `Decrypter`（注入，nil=明文路径）：
  - `type Decrypter interface { Decrypt(header fileformat.BlockHeader, stored []byte) ([]byte, error) }`
- `readAtBlockView` / `readAtBlockCopy`：ch head 后若 `header.Encrypted` → 先 Decrypt 再 decompress。
  解密输出新 buf（天然解决 None 压缩的别名问题）。
- loader 不变（缓存明文块）。

## 6. S5 Open/恢复/重建/校验

- Open：读 header 后若 `EncryptionAlgorithm != None`：
  - Options.Encryption 或 KeyProvider 缺失 → `ErrKeyRequired`；
  - 否则构建读路径 decryptor，注入 reader（含 epoch→key 的缓存）。
- recovery.buildIndexTxnFromData / verify.Verify / RebuildIndex：
  - 需要解密的路径统一走 reader（已注入 decryptor）；函数签名扩展接受 KeyProvider。
- 错误分类：`ErrAuthFailed` 挂 CorruptionError（Snapshot/Table/Block 定位）。

## 7. S6 集成测试

- 加密 Store：Create → FULL/DELTA → Get/Scan/ReadBatch/Exists → Close → Open 读一致；
- 错误：无 KeyProvider Open → ErrKeyRequired；Key 不存在 → ErrKeyUnavailable/ErrKeyIDNotFound；
  KeyID 不匹配 → ErrKeyRequired/ErrKeyUnavailable；
- 篡改：位翻转 .rpk 中某 Block 密文/AAD 字段 → 读该块 ErrAuthFailed（定位正确）；
- 崩溃恢复：fault 注入各 commit 点 + 加密店 → 恢复与 rebuild 索引后数据一致；
- 并发：加密 Store 多 goroutine 读（-race）；
- 与未加密 Store 全量对拍（golden + 语义等价）。

## 8. S7 文档同步

- BINARY_FORMAT_V1.md：FileHeader/BlockHeader 加密字段表；
- GO_API_DESIGN.md：KeyProvider、EncryptionConfig、Options.Encryption、错误列表；
- DATA_BLOCK_ENCRYPTION_FEASIBILITY.md：保持 §10 决策与实现一致。

## 9. S8 性能基准与对比

- 在统一基准矩阵（plan-v12 §2 BenchmarkMainMatrix）上增加 encryption 维度：
  场景（rand/seq × cold/hot）× BlockSize × encryption on/off；
- 关键指标：随机 Get 吞吐、顺序 Scan 吞吐、ReadBatch 吞吐、p95 延迟、
  缓存命中路径（热读）回退；
- 验收目标：加密 vs 未加密：除随机冷点读外吞吐回退 ≤ 10%；热路径（缓存命中）不回退（决策 C，2026-09-09）；随机冷点读 -12.8% 记为结构性解密放大基线
- 产出 docs/perf-report-encryption.md 对比表；
- 后续性能调整（缓存策略、并行解密、GCM 复用、BlockSize 矩阵）由使用者参与决策，
  不在本计划自动执行。

## 10. 明确不做（v1）

.rpi 敏感字段加密；每块 HKDF（NonceScheme=2）；padding 隐藏块大小；
KMS 集成（KeyProvider 抽象即可）；事后补加密；密文缓存策略实现。