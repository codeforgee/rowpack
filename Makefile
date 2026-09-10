GO ?= go

.PHONY: all build test race vet lint fmt clean golden bench bench-quick bench-batch bench-10m bench-profile

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

BENCHTIME ?= 1s
BENCHCOUNT ?= 1
bench:
	mkdir -p bench
	$(GO) test -run '^$$' \
		-bench 'Benchmark(Env|MainMatrix|Latency|WriteFull|GetHot|GetCold|GetColdUnpooled|Scan|ReadBatch1000|GetLoop1000|OpenReplay|OpenMemory|DeepChainGet|EncryptedWrite|EncryptedGetHot)$$' \
		-benchtime=$(BENCHTIME) -benchmem -count=$(BENCHCOUNT) . \
		2>&1 | tee bench/results.txt

# 10M 行 Open 内存档（idxB/row 与峰值 RSS）。
bench-10m:
	mkdir -p bench
	ROWPACK_BENCH_ROWS10M=10000000 $(GO) test -run '^$$' -bench 'BenchmarkOpenMemory10M$$' \
		-benchmem -benchtime=3x -count=1 . 2>&1 | tee bench/results-10m.txt

# CPU/heap profile 采集（读路径用 GetCold 覆盖冷读放大，Scan 覆盖流式分配）。
# 产物为 *.out（gitignore），用 go tool pprof 查看；命令随仓库维护，机器相关。
bench-profile:
	$(GO) test -run '^$$' -bench 'BenchmarkGetCold$$' -benchtime=1000x -cpuprofile bench/getcold-cpu.out -memprofile bench/getcold-mem.out .
	$(GO) test -run '^$$' -bench 'BenchmarkScan$$' -benchtime=50x -cpuprofile bench/scan-cpu.out -memprofile bench/scan-mem.out .

# 快速档：小数据集 + 固定迭代数，~30s 跑完全矩阵；只看结构/量级，不与 100k 基线比数值。
bench-quick:
	mkdir -p bench
	ROWPACK_BENCH_ROWS=20000 ROWPACK_BENCH_ROWS1M=200000 \
	$(GO) test -run '^$$' \
		-bench 'Benchmark(Env|MainMatrix|Latency|WriteFull|GetHot|GetCold|Scan|ReadBatch1000|GetLoop1000|OpenReplay|DeepChainGet|EncryptedWrite|EncryptedGetHot)$$' \
		-benchtime=3x -benchmem -count=1 . \
		2>&1 | tee bench/results-quick.txt

bench-batch:
	$(GO) test -run '^$$' -bench 'Benchmark(GetLoop1000|ReadBatch1000)$$' \
		-benchmem -benchtime=10s -count=1 .

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
	rm -rf *.test coverage.out
	rm -rf testdata/tmpdb
	find . -type d -path '*/testdata/tmpdb' -exec rm -rf {} + 2>/dev/null || true
