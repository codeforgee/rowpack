# RowPack 数据块加密（v1）

> 状态：已实现并冻结（加密作为创建时可选能力直接进入 v1 格式，无老格式兼容约束）
> 相关：[BINARY_FORMAT_V1.md](BINARY_FORMAT_V1.md) §4/§5/§13 · [INDEX_TXN_FORMAT_V1.md](INDEX_TXN_FORMAT_V1.md) §6

## 1. 结论

- 只使用 **AES-256-GCM**，顺序固定为**先压缩、后加密**；
- 加密是**创建时的可选能力**：`EncryptionAlgorithm != EncNone` 即加密 store，在 Create 时确定，
  FileHeader 创建后不更新，不存在「给已存在 `.rpk` 事后补加密」；
- SnapshotFooter、BlockHeader 和必要的索引导航信息保持可解析；
- Rows Block **逐页密封**，Metadata Block **整容器密封**；IndexTxn body 按 chunk/页密封。

整文件加密会破坏随机读取、增量追加和崩溃恢复；逐行加密则增加 nonce、认证标签和调用开销并降低
压缩效果。Block 级加密可保持「一个 Block 解密校验后即可读其中任意行」，保留 Block 索引与
`ReadAt` 随机访问，新快照只追加新密文 Block，认证失败可定位到 Snapshot/Table/Block，并按
Block/页缓存明文避免整库解密。

## 2. 加密单位与顺序

```text
Rows/Metadata payload → compress → encrypt(AEAD) → write
```

读取顺序相反，且必须在解压前完成认证：

```text
read → Open(认证解密) → decompress → validate(CRC/长度/行解码)
```

- **Rows Block**：页目录明文，每个 stored page 独立密封（`AEAD(压缩页) ‖ tag`），
  `RowsPageDirEntry.StoredSize = 压缩页长 + 16B`；未加密页不承担 tag 开销；
- **Metadata Block**：整个 Metadata Payload 容器密封；
- **IndexTxn**：body 按 chunk / Row Index Page 独立密封，Header/Footer 保持明文。

认证失败分两类：尾部不完整按未提交尾部规则忽略或截断；已提交范围内的认证失败属于中间损坏，必须
报错，不能跳过。

## 3. FileHeader 落位

加密字段位于 FileHeader Reserved 区（offset 64..120），明文 store 该区全零，与非加密布局字节
一致：

```text
offset  size  field
64      1     EncryptionAlgorithm    // 0=None, 1=AES-256-GCM
65      1     NonceScheme            // 0=None, 1=NonceCounterV1
66      1     KeyID length (0..31)
67      31    KeyID（ASCII，定长区，未用部分为 0）
```

加密通过 `EncryptionAlgorithm` 声明，不占用 `RequiredFeatures` 位。`HeaderCRC32C` 覆盖包含这些
字段的整个 128 字节头。

## 4. Block / Page 字段

- **BlockHeader**：`Flags` bit0 = 本块加密；`StoredSize` 语义为密文长度（含 tag）；认证标签长度
  由算法冻结（16B），不落盘；`KeyEpoch` 写在 offset 56..60（Reserved 前 4 字节）；
- **RowsPageDirEntry**：`StoredSize` 为密封后页长；页目录保持明文，供读取器无需解密即可定位页。

## 5. Nonce

nonce 唯一性不依赖 AAD，必须在 nonce 字段内部显式分区：

```text
Block nonce : KeyEpoch(4B, LE) ‖ BlockID(8B, LE)
Index nonce : (KeyEpoch | 0x80000000)(4B, LE) ‖ TxnSequence(8B, LE)   // bit31 = 域标志
```

- Block nonce 的 epoch 恒低于 2^31，index nonce 置 epoch 字最高位，因此两者永不碰撞；唯一性由
  BlockID 单调递增、`.rpk` 只追加不重写、epoch 计入 nonce 保证；
- **Rows Page nonce**：`Trunc12(HMAC-SHA256(pageNonceKey, prefix ‖ UUID ‖ SnapshotID ‖ BlockID ‖
  PageOrdinal ‖ KeyEpoch))`，`pageNonceKey = HMAC-SHA256(dataKey, "RowPack rows page nonce key
  v1")`；
- **Index chunk / Row Index Page nonce**：`Trunc12(HMAC-SHA256(chunkNonceKey, BE(TxnSequence) ‖
  BE(ChunkSequence)))`，Row Index Page 接在 chunk 序号之后继续编号，与 chunk 两两不同。

已否决：截断 TxnSequence 腾空间（超长生命周期有碰撞风险）、nonce 低位 XOR ChunkSequence
（`1⊕2 == 3⊕0` 会复用 nonce）、每块 HKDF（NonceScheme=2 之前不做）。禁止：同密钥下复用 nonce；
Block / Index / Page nonce 跨域混用；只绑 ChunkSequence 而不绑 TxnSequence；重写或重试复用旧
事务 nonce。

## 6. AAD

AAD 只能绑定**解密时已可得的字段**，防止密文被挪到另一个 Block、表或 store 后仍被接受。

### Block AAD（68B）

```text
aadMagic("RowPackBlockV1") · StoreUUID · BlockKind · Compression
· BlockID · SnapshotID · TableID · ItemCount · RawSize · StoredSize
```

不绑定 `KeyEpoch`（已在 nonce 中）与 `Encrypted` flag（它正是选择该路径的字段）；不绑定
`SchemaVersion`（位于加密 payload 内部，解密时不可得）；`ParentSnapshotID` 不进 AAD（同 store
挪块攻击面低，跨 store 已被 StoreUUID 防住，标注为低危未覆盖项）。

### Rows Page AAD（96B）

绑定 StoreUUID、SnapshotID/BlockID/TableID/Compression，以及页目录的
PageOrdinal/FirstRecordOrdinal/RecordCount/StoredSize(密封后)/RawSize/MinRowID/MaxRowID，再加
KeyEpoch。

### Index chunk AAD（72B）

绑定 StoreUUID、TxnSequence、SnapshotID、ChunkSequence、FirstEntryOrdinal、EntryKind、RawBytes、
StoredBytes、KeyEpoch（实现为 `seal.ChunkContext.AAD`）。**不绑文件偏移**：StoredBytes 依赖压缩
结果，而 Footer 偏移依赖全部 stored 长度之和，绑定会形成循环；偏移防挪用由
`SnapshotFooter.IndexTxnCRC32C` 与精确字节范围承担。整条
IndexTxn 一个 AAD 的布局（`seal.BuildAADIndex` / `AADIndexSize`）作为冻结原语保留，当前读写
路径不调用，仅由 seal 包测试守住布局。

## 7. 密钥管理与轮换

RowPack 不保存、生成或托管主密钥，只通过外部 `KeyProvider`（接口签名见
[GO_API_DESIGN_V1.md](GO_API_DESIGN_V1.md) §2）按 `keyID`+`epoch` 取 32 字节 AES-256 密钥，
epoch 0 为初始代次。密钥轮换不重写整个 `.rpk`：新快照用新 KeyEpoch，旧 Block 继续用旧密钥，
读取历史快照时按 Block 的 KeyEpoch 请求对应密钥；长期保留历史快照意味着旧密钥必须长期可获取，
否则数据在但不可解密。撤销旧密钥是「使历史数据不可读」的管理操作，不是普通 compaction。
**密钥丢失 = 数据不可恢复**（管理风险，非普通故障）。

## 8. 缓存策略

默认缓存**明文**（沿用现有 LRU 语义：key=BlockID、value=校验后明文块）：BlockID→内容不可变，
无 KeyEpoch 冲突；行解码后明文本就在进程内，威胁模型与「备份文件本身是明文」同量级；热读不回
退。「仅缓存密文」为可选策略（高安全部署），保留配置位，默认不启用。

## 9. 打开与工具契约

- 加密 store 的 Open 必须提供 KeyProvider，否则直接返回 `ErrKeyRequired`，不得进入「Open 成功
  但所有读失败」的半可用状态；
- `Verify`/索引重建同样要求密钥，无密钥返回 `ErrKeyRequired`；
- 结构审计（快照时间线、块边界、大小，不解密）作为独立只读工具路径，不默认提供；
- `.rpk` 的提交权威不受加密影响：尾部不完整按现规则忽略/截断；已提交范围内认证失败为中间损坏，
  必须报错。

## 10. 错误分类

认证失败同时挂到 `CorruptionError`（可定位 Snapshot/Table/Block）：

```text
ErrKeyRequired     Store 声明加密但未提供 KeyProvider
ErrKeyUnavailable  KeyProvider 取不到密钥
ErrKeyIDNotFound   KeyID 不存在
ErrAuthFailed      AEAD 认证失败（AAD/tag/密文被篡改）
```

## 11. 性能与验收门槛

加密开销主要是每块/每页一次 AEAD 初始化、标签与解密 CPU；压缩后再加密保留压缩比。256 KiB 块 +
AES-NI 下：

- 随机 Get / 顺序 Scan 加密 vs 未加密吞吐回退 ≤ 10%；
- 随机冷读回退已知基线约 ~13%（无缓存解压+解密），热读/扫描/批量 ≤ 1%；
- Cache 命中路径吞吐不回退；
- nonce 重复检测（测试桩注入重复 nonce 必须报错）；
- 加密与未加密 Store 全语义等价（golden + 对拍）；
- 无密钥 Open/Verify/重建行为全部符合 §9。

## 12. 明确不做

- 不建议把密码直接写入 Store 文件；
- 不使用 ECB 或自拼「加密 + CRC」替代 AEAD；
- 不每行独立加密；
- 不在 v1 引入 padding 隐藏块大小；
- 不做每块 HKDF（NonceScheme=2 之前）；
- 不把 KMS / 云密钥服务集成进核心（KeyProvider 保持抽象接口）；
- 不提供对已存在 `.rpk` 事后补加密；
- 不在 v1 加密 `.rpi` 敏感字段（已无独立 `.rpi`；IndexTxn 按 chunk/页密封已覆盖导航信息）。
