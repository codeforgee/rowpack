# testdata/baseline

RowPack 性能基线归档目录，由 `make baseline`（`scripts/baseline.sh`）生成。文件按
`<YYYY-MM-DD>.txt` 命名；带 `BASELINE_LABEL` 时加 `-<label>`（如 `-m1pro`），同名已存在则自动
加 `-2/-3…`，不覆盖历史。

每个文件 = 统一标注头 + 对应档位的完整 `go test -bench` 输出。头部字段为
`date/mode/git/go/os-arch/cpu/mem/zstd/format/config/benchtime/count/rows/rows1m/command`，末行是
机器可读的 `# json: {...}`（单行，供 `scripts/baseline-diff.sh` 解析）。模式：`full`
（`make baseline`）、`quick`（20k 行冒烟，数值不与基线比）、`1m`（`make baseline MODE=1m`），头部
`mode:` 字段标明档位。这些文件纳入版本库作为可比证据；临时输出（`bench/results*.txt`）不入库。

## 对比

```sh
make baseline-diff OLD=2026-09-10 NEW=2026-09-11            # 阈值 10%
THRESHOLD=5 VERBOSE=1 make baseline-diff OLD=... NEW=...    # 阈值 5% + 全量
```

按指标方向计算变化率，列出超阈值回退/改进并校验元数据一致性；有超阈值回退时退出码为 1。完整
基线与解读见 [docs/PERFORMANCE_BASELINE_V1.md](../../docs/PERFORMANCE_BASELINE_V1.md)。
