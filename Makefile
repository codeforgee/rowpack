GO ?= go

# 基准参数：BENCHTIME/BENCHCOUNT 供 bench 与 baseline 使用；MODE 选择 baseline 档位。
BENCHTIME ?= 1s
BENCHCOUNT ?= 1
MODE ?= full

# bench / bench-quick / baseline 共用的标准基准集合与公共参数。
BENCH_PATTERN = Benchmark(Env|MainMatrix|Latency|WriteFull|GetHot|GetCold|GetColdUnpooled|Scan|ReadBatch1000|GetLoop1000|OpenReplay|DeepChainGet|EncryptedWrite|EncryptedGetHot)
BENCH_ARGS = -run '^$$' -bench '$(BENCH_PATTERN)$$' -benchmem

.PHONY: all build test race vet lint staticcheck fmt clean golden \
        bench bench-quick bench-1m bench-batch bench-profile baseline baseline-diff

all: fmt vet test

build:
	$(GO) build ./...

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

lint:
	golangci-lint run ./...

staticcheck:
	staticcheck ./...

fmt:
	gofmt -l -w .

# ---- 基准（机器相关输出落 bench/，gitignore）----

bench:
	mkdir -p bench
	$(GO) test $(BENCH_ARGS) -benchtime=$(BENCHTIME) -count=$(BENCHCOUNT) . 2>&1 | tee bench/results.txt

# 快速档：20k 行冒烟，只看结构/量级，不与 100k 基线比数值。
bench-quick:
	mkdir -p bench
	ROWPACK_BENCH_ROWS=20000 ROWPACK_BENCH_ROWS1M=200000 \
		$(GO) test $(BENCH_ARGS) -benchtime=3x -count=1 . 2>&1 | tee bench/results-quick.txt

# 1M 行聚焦档（scan1m/getrand1m），与 make bench 同口径。
bench-1m:
	mkdir -p bench
	ROWPACK_BENCH_ROWS1M=1000000 \
		$(GO) test -run '^$$' -bench 'BenchmarkMainMatrix/(scan1m|getrand1m)' -benchmem \
		-benchtime=$(BENCHTIME) -count=$(BENCHCOUNT) . 2>&1 | tee bench/results-1m.txt

# 批量读对比：逐行 Get 基线 vs ReadBatch。
bench-batch:
	$(GO) test -run '^$$' -bench 'Benchmark(GetLoop1000|ReadBatch1000)$$' -benchmem -benchtime=10s -count=1 .

# CPU/heap profile → bench/*.out，用 go tool pprof 查看。
bench-profile:
	mkdir -p bench
	$(GO) test -run '^$$' -bench 'BenchmarkGetCold$$' -benchtime=1000x -cpuprofile bench/getcold-cpu.out -memprofile bench/getcold-mem.out .
	$(GO) test -run '^$$' -bench 'BenchmarkScan$$' -benchtime=50x -cpuprofile bench/scan-cpu.out -memprofile bench/scan-mem.out .

# ---- 性能基线归档（统一标注，写入 testdata/baseline/，入版本库）----

# make baseline [MODE=full|quick|1m] [BASELINE_LABEL=...] [BENCHTIME=...] [BENCHCOUNT=...]
baseline:
	scripts/baseline.sh $(MODE)

# make baseline-diff OLD=<date> NEW=<date> [THRESHOLD=10]，有超阈值回退时退出码 1。
baseline-diff:
	scripts/baseline-diff.sh "$(OLD)" "$(NEW)" $(THRESHOLD)

golden:
	$(GO) test . -run 'TestGolden' -args -update-golden
	@echo "== golden 样本 SHA-256（须与 testdata/golden/README.md 清单一致）=="
	@shasum -a 256 testdata/golden/*.rpk testdata/golden/*.bin
	@if git diff --quiet -- testdata/golden; then \
		echo "== git: 无字节变化（格式未漂移，重写为相同字节）=="; \
	else \
		echo "== 警告：golden 字节变化，视为格式变更，须人工 diff 审查 + 提升版本 =="; \
		git diff --stat -- testdata/golden; \
	fi

clean:
	rm -rf *.test coverage.out testdata/tmpdb
	find . -type d -path '*/testdata/tmpdb' -exec rm -rf {} + 2>/dev/null || true
