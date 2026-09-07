# RowPack 数据分块加密可行性

> 状态：可行性设计，非 v1 实现承诺
> 日期：2026-09-07

## 1. 结论

数据分块加密与 RowPack 当前的 append-only、快照、压缩 Block 和随机读取模型相容，推荐作为 v1 之后的可选能力实现。

推荐顺序为：先压缩，再对每个 Block 使用 AEAD 加密；`.rpk` 的 SnapshotFooter、Block Header 和必要的索引导航信息保持可解析，Block 的压缩负载和敏感元数据内容加密。首选 AES-256-GCM，平台无 AES 硬件时可提供 ChaCha20-Poly1305。

## 2. 为什么采用 Block 级别

整文件加密会破坏随机读取、增量追加和崩溃恢复；逐行加密则会增加 nonce、认证标签和调用开销，并降低压缩效果。Block 级加密可以：

- 保持一个 Block 解压、解密、校验后即可读取其中任意行；
- 保留 Block 索引和 `ReadAt` 随机访问路径；
- 新快照只追加新的密文 Block；
- 让认证失败定位到 Snapshot、Table 和 Block；
- 支持按 Block 缓存明文，避免整库解密。

处理顺序必须固定为：

```text
Rows/Metadata payload → compress → encrypt(AEAD) → write
```

解读顺序相反。不能先加密再压缩，否则密文不可压缩。

## 3. 建议的格式变化

加密应通过格式主版本或 Feature Bit 明确启用，不应改变已有 v1 未加密 Block 的解释方式。建议在 Store Header 增加：

- EncryptionAlgorithm：None、AES-256-GCM、ChaCha20-Poly1305；
- KeyID：外部密钥管理系统中的逻辑标识，不保存密钥本身；
- NonceScheme：固定版本的 nonce 生成规则；
- KeyEpoch：密钥轮换代次。

Block Header 增加或复用字段表示：

- 是否加密；
- CiphertextSize；
- AuthenticationTagSize；
- KeyEpoch；
- Nonce 或可确定性 nonce 所需的参数。

推荐 nonce 不直接随机生成并单独存储，而是由唯一的 Store ID、KeyEpoch、SnapshotID、BlockID 和固定域分隔编码后，通过安全 KDF 或严格分配器生成。无论采用哪种方案，都必须保证同一密钥下 nonce 永不复用。BlockID 单调递增且不回退，是实现这一约束的重要基础。

AEAD 的 Additional Authenticated Data（AAD）应绑定不可变的结构字段，例如格式版本、Store 标识、SnapshotID、ParentID、BlockID、TableID、BlockKind、SchemaVersion、压缩算法和明文长度。这样可以防止密文被挪到另一个 Block 或表后仍被接受。

## 4. 哪些内容需要保护

建议分三个等级：

| 内容 | 完整性 | 机密性 | 说明 |
| --- | --- | --- | --- |
| Row payload | 必须 | 必须 | 主要保护对象 |
| Metadata payload | 必须 | 默认必须 | 表名、列名和源库设计可能泄露信息 |
| Block/索引导航字段 | 必须 | 可选 | 随机读取和恢复需要读取部分字段 |
| SnapshotFooter | 必须 | 可选 | `.rpk` 提交权威，必须可验证 |
| `.rpi` 索引 | 必须 | 可选/建议 | 若包含 RowID、表名映射或统计信息，则应隐藏或单独加密 |

最低可行方案是加密 `.rpk` 中 Block 的压缩负载，保留导航字段并对 Header 做 AAD。更完整的方案还应对 `.rpi` 的敏感字段加密，但必须保留恢复所需的最小导航信息，或提供重建索引所需的解密路径。

## 5. 快照、提交与恢复

加密不改变 `.rpk` 是提交权威的决策：

1. 生成并加密 Block；
2. 写入并同步密文 Block；
3. 写入 SnapshotFooter；
4. 同步 Footer；
5. 更新 `.rpi`；
6. 发布内存视图。

恢复时必须先验证 AEAD tag，再把 Block 交给解压和行 CRC 校验。没有有效 Footer 的加密 Block 仍视为未提交尾部。认证失败分为两类：尾部不完整可按现有规则忽略或截断；已提交范围内的认证失败属于中间损坏，必须报错，不能跳过。

## 6. 密钥管理与轮换

RowPack 不应负责保存、生成或托管用户主密钥。API 应接收一个外部提供的 KeyProvider：

```go
type KeyProvider interface {
    Key(ctx context.Context, keyID string, epoch uint32) ([]byte, error)
}
```

密钥轮换不应重写整个 `.rpk`。新快照使用新的 KeyEpoch，旧 Block 继续使用旧密钥；读取历史快照时按 Block 的 KeyEpoch 请求对应密钥。长期保留历史快照意味着旧密钥也必须长期可获取，否则数据仍在但不可解密。

如果需要撤销旧密钥，必须明确这是“使历史数据不可读”的管理操作，而不是普通 compaction。未来 compaction 可以把存活数据重写为新快照并使用新密钥，但这属于新的快照/归档流程。

## 7. 性能与工程风险

主要成本是每个 Block 一次 AEAD 初始化、认证标签和解密 CPU。由于当前默认 Block 目标为 256 KiB，开销通常远低于逐行加密；压缩后再加密也保留了现有压缩比。需要重点验证：

- 加密 Block 的随机 Get 和顺序 Scan 延迟；
- Block Cache 缓存密文还是明文（推荐可配置，默认不长期缓存明文）；
- 并发读取时 KeyProvider 是否会成为瓶颈；
- 密钥不可用、KeyID 不存在和认证失败的错误分类；
- RebuildIndex、Verify 和只读 Open 在没有密钥时的行为；
- nonce 唯一性、AAD 覆盖范围和故障注入恢复。

## 8. 分阶段建议

1. 设计阶段：冻结 Encryption Feature Bit、Block 加密字段、nonce 方案和错误类型。
2. 原型阶段：只加密 Row/Metadata Block 负载，保留索引明文，完成 round-trip、随机读和恢复测试。
3. 完整阶段：增加加密索引或敏感索引字段、KeyProvider、KeyEpoch 和密钥轮换测试。
4. 发布前：增加篡改测试、nonce 重复检测、密钥丢失测试、性能基准和格式兼容测试。

## 9. 暂不建议

- 不建议把密码直接写入 Store 文件；
- 不建议使用 ECB 或自行拼接“加密 + CRC”替代 AEAD；
- 不建议每行独立加密；
- 不建议为了隐藏 Block 大小而在 v1 引入复杂填充，除非威胁模型明确要求；
- 不建议让 RowPack 核心理解 KMS、云厂商密钥服务或数据库账户体系。

