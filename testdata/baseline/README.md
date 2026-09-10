# testdata/baseline

RowPack 性能基线归档目录，由 `make baseline`（`scripts/baseline.sh`）生成。

## 命名

```
<YYYY-MM-DD>.txt          当日首次完整基线
<YYYY-MM-DD>-<label>.txt  带 BASELINE_LABEL 标签（如 -m1pro）
<YYYY-MM-DD>-<n>.txt      同名已存在时的自增后缀，不覆盖历史
```

## 内容

每个文件 = 统一标注头 + 对应档位的完整 `go test -bench` 输出：

```
# date / mode / git / go / os-arch / cpu / mem / zstd / format / config
# benchtime / count / rows / rows1m / command
# json: {"ts":...,"mode":...,"git":...,"go":...,"os":...,"arch":...,"cpu":...,
#        "mem":...,"zstd":...,"format":"v1","magic":"ROWPACK1",
#        "benchtime":...,"count":...,"rows":...,"rows1m":...}
<go test -bench 原始输出>
```

- `# json:` 行为机器可读元数据（单行），供 `scripts/baseline-diff.sh` 解析；
- 模式：`full`（`make baseline`）、`quick`（20k 行冒烟，数值不与基线比）、
  `1m`（`make baseline MODE=1m`）；
- 头部 `mode:` 字段标明档位；对照两个基线前先比对头部 git/Go/CPU/config 是否一致；
- 这些文件纳入版本库，作为可比证据；临时输出（`bench/results*.txt`）不入库。

## 对比

```sh
make baseline-diff OLD=2026-09-10 NEW=2026-09-11            # 阈值 10%
THRESHOLD=5 VERBOSE=1 make baseline-diff OLD=... NEW=...    # 阈值 5% + 全量
```

按指标方向计算变化率，列出超阈值回退/改进并校验元数据一致性；有超阈值回退时退出码为 1。

完整基线与解读见 [docs/PERFORMANCE_BASELINE_V1.md](../../docs/PERFORMANCE_BASELINE_V1.md)。
