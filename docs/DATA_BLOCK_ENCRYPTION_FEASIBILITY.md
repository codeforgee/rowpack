# RowPack 数据分块加密可行性

> 状态：已实现并冻结（加密作为创建时可选能力直接进入 v1 格式，无老格式兼容约束）
> 决策见 §10
> 日期：2026-09-07（§10 决策记录 2026-09-09，2026-09-09 修订解除兼容约束）
> 性能：加密开销仅随机冷读回退 ~13%（无缓存解压+解密，记为已知基线；
> 热读/扫描/批量均 ≤1%，详见 docs/perf-report.md 基准矩阵），后续可按需优化

## 1. 结论

数据分块加密与 RowPack 当前的 append-only、快照、压缩 Block 和随机读取模型相容，已决定作为 v1 格式的可选能力实现（格式尚未发布，不承担老格式兼容约束）。

推荐顺序为：先压缩，再对每个 Block 使用 AES-256-GCM 加密；`.rpk` 的 SnapshotFooter、Block Header 和必要的索引导航信息保持可解析，Block 的压缩负载和敏感元数据内容加密。

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

- EncryptionAlgorithm：None、AES-256-GCM；
- KeyID：外部密钥管理系统中的逻辑标识，不保存密钥本身；
- NonceScheme：固定版本的 nonce 生成规则；
- KeyEpoch：密钥轮换代次。

具体落位（v1 格式线）：`RequiredFeatures` bit 4 声明本 Store 启用加密；
上述字段写入 FileHeader Reserved 区（offset 64–120），KeyID 定长 32 字节
ASCII。FileHeader 创建后不更新，因此**加密必须在 Store 创建时启用**，
不存在"给已存在的 .rpk 事后补加密"；布局、已知位掩码与兼容性见 §10.1。

Block Header 增加或复用字段表示：

- 是否加密：Flags bit 0；
- CiphertextSize：复用 StoredSize（语义扩展为密文长度）；
- AuthenticationTagSize：由算法冻结（AES-256-GCM = 16 字节），不落盘；
- KeyEpoch：写入 Reserved 8 字节；
- Nonce：确定性生成（NonceScheme=1，见下），不落盘。

推荐 nonce 不直接随机生成并单独存储，而是由唯一的 Store ID、KeyEpoch、SnapshotID、BlockID 和固定域分隔编码后，通过安全 KDF 或严格分配器生成。无论采用哪种方案，都必须保证同一密钥下 nonce 永不复用。BlockID 单调递增且不回退，是实现这一约束的重要基础。v1 采纳"严格分配器"（NonceScheme=1：96-bit = KeyEpoch 4B ‖ BlockID 8B），KDF 派生留作 NonceScheme=2，见 §10.1。

AEAD 的 Additional Authenticated Data（AAD）只能绑定**解密时代码已可得的字段**，防止密文被挪到另一个 Block 或表后仍被接受。v1 冻结集合 = 格式版本、StoreUUID、BlockKind、Compression、BlockID、SnapshotID、TableID、ItemCount、RawSize、StoredSize（即 BlockHeader 全部字段 + StoreUUID）；不绑定 SchemaVersion（它在加密 payload 内部，解密时不可得）；ParentID 可选且 v1 不进（同 Store 挪块低危，跨 Store 已被 StoreUUID 防住），见 §10.2。

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
- Block Cache 缓存密文还是明文：v1 默认缓存明文、"仅缓存密文"为可选策略，见 §10.5；
- 并发读取时 KeyProvider 是否会成为瓶颈；
- 密钥不可用、KeyID 不存在和认证失败的错误分类；
- RebuildIndex、Verify 和只读 Open 在没有密钥时的行为；
- nonce 唯一性、AAD 覆盖范围和故障注入恢复。

## 8. 分阶段建议

1. 设计阶段：冻结 Encryption Feature Bit、Block 加密字段、nonce 方案和错误类型（已冻结，见 §10）。
2. 原型阶段：只加密 Row/Metadata Block 负载，保留索引明文，完成 round-trip、随机读和恢复测试。
3. 完整阶段：增加加密索引或敏感索引字段、KeyProvider、KeyEpoch 和密钥轮换测试。
4. 发布前：增加篡改测试、nonce 重复检测、密钥丢失测试、性能基准和格式兼容测试。

## 9. 暂不建议

- 不建议把密码直接写入 Store 文件；
- 不建议使用 ECB 或自行拼接“加密 + CRC”替代 AEAD；
- 不建议每行独立加密；
- 不建议为了隐藏 Block 大小而在 v1 引入复杂填充，除非威胁模型明确要求；
- 不建议让 RowPack 核心理解 KMS、云厂商密钥服务或数据库账户体系。

## 10. 决策记录（v1.3 规划）

> 日期：2026-09-09
> 关联：BINARY_FORMAT_V2.md、ADR-001（.rpk 提交权威）、ADR-003（v2 单文件）。
> 规划期原引用 BINARY_FORMAT_V1.md §2/§3、GO_API_DESIGN.md §5、plan-v12.md 已随 v1 历史文档一并移除。

### 10.1 格式落位（修正 §3）

- 加密作为**创建时的可选能力**直接进入 v1 格式：FileHeader Reserved 区
  （offset 64–120，56 字节）写入 EncryptionAlgorithm(1)、NonceScheme(1)、
  KeyID 定长 32 字节 ASCII（1B 前缀长度 + 31B 值）；.rpk/.rpi 共享同一
  FileHeader 布局。
- 格式尚未发布 → 不引入 feature bit / minor 双轨；RequiredFeatures 保持
  现状；未加密 Store 上述字节全零，布局与既有实现一致，数据文件变更
  可接受。
- 创建时冻结：FileHeader 创建后不更新，加密状态在 Create 时确定，
  不存在"给已存在 .rpk 事后补加密"。
- Block Header：Flags bit 0=本块加密；StoredSize 语义扩展为密文长度；
  tag 大小由算法冻结（16 B），不落盘；Reserved 前 4 字节放 KeyEpoch(4)。
- NonceScheme=1（直接编码）：96-bit nonce = KeyEpoch(4B) ‖ BlockID(8B)，
  零每块 KDF 开销，唯一性由 BlockID 单调不回退 + .rpk append-only 保证；
  HKDF 派生留作 NonceScheme=2。

### 10.2 AAD 边界（修正 §3）

AAD 只能绑定解密时代码已可得的字段：

- BlockHeader 全部字段（BlockKind、Compression、BlockID、SnapshotID、
  TableID、ItemCount、RawSize、StoredSize）+ StoreUUID；
- 不绑定 SchemaVersion：它位于加密 payload 内部的 RowDirectoryEntry，
  解密时不可得；
- ParentID（SnapshotHeader）v1 不进 AAD：需要给 Load/ReadAtBlock 传
  上下文，同 Store 内挪块攻击面低，跨 Store 已被 StoreUUID 防住；
  标注为低危未覆盖项。

### 10.3 打开与工具契约

- 加密 Store 的 Open 必须提供 KeyProvider，否则直接返回 `ErrKeyRequired`
  失败；不得进入"Open 成功但所有读失败"的半可用状态。
- Verify / RebuildIndex 同样要求密钥，无密钥时返回 `ErrKeyRequired`。
- 结构审计（快照时间线、块边界、大小，不解密）作为独立只读工具路径，
  不默认提供。

### 10.4 ADR-001 修订

".rpi 可完全从 .rpk 重放重建"在加密 Store 下限缩为"有密钥时可重建"：
RebuildIndex、恢复补索引、VerifyFull 均需解密 payload。
**密钥丢失 =（.rpk/.rpi 俱在但）数据不可恢复**，是管理风险而非普通故障；
§6 "旧密钥必须长期可获取"升级为强制约束。重建/恢复路径无密钥时返回
`ErrKeyRequired`，不静默跳过。

### 10.5 缓存策略

默认缓存明文（复用现有 LRU 语义：key=BlockID、value=校验后明文块）：
BlockID→内容不可变，无 KeyEpoch 冲突；行解码后明文本就在进程内，
威胁模型与"备份文件本身是明文"同量级；热读不回退。
"仅缓存密文"为可选策略（高安全部署），保留配置位，v1 不默认。

### 10.6 错误分类

新增错误并入现有错误模型；认证失败同时挂到 `CorruptionError`（可定位
Snapshot/Table/Block）：

- `ErrKeyRequired`：Store 声明加密但未提供 KeyProvider；
- `ErrKeyUnavailable` / `ErrKeyIDNotFound`：KeyProvider 取不到密钥；
- `ErrAuthFailed`：AEAD 认证失败（AAD/tag/密文被篡改）。

仍沿用 ADR-001：尾部不完整按现规则忽略/截断；已提交范围内认证失败为
中间损坏，必须报错。

### 10.7 版本定位与实施顺序

- 直接进入 v1 格式（尚未发布，无兼容约束），VersionMinor 保持 0；
  不需要 feature bit 或 minor 双轨。
- P0 原型（最小可行）：FileHeader/BlockHeader 字段 + NonceScheme=1 + AAD
  最小集（BlockHeader 全字段 + StoreUUID）+ Rows/Metadata Block 负载
  加密 + Open 需 KeyProvider + Get/Scan/ReadBatch/恢复/Verify/
  RebuildIndex 全链路 + 与未加密 Store 对拍测试。
- P1 完整：KeyEpoch 轮换测试、AAD 任一位篡改必须认证失败、密钥丢失/
  轮换测试、.rpi 敏感字段加密评估（v1 不实现）。
- 分步实现计划（S1–S8）已执行完毕，过程文档已移除。

### 10.8 验收门槛

256 KiB 块 + AES-NI：

- 随机 Get / 顺序 Scan 加密 vs 未加密吞吐回退 ≤ 10%；
- Cache 命中路径吞吐不回退；
- nonce 重复检测（测试桩注入重复 nonce 必须报错）；
- 加密与未加密 Store 全语义等价（golden + 对拍）；
- 无密钥 Open/Verify/Rebuild 行为全部符合 §10.3/§10.4。

### 10.9 明确不做（追加 §9）

- .rpi 敏感字段加密（v1 阶段，P1 才评估）；
- 每块 HKDF（NonceScheme=2 之前不做）；
- padding 隐藏块大小；
- KMS / 云密钥服务集成进核心（KeyProvider 保持抽象接口）；
- 对已存在 .rpk 事后补加密。
