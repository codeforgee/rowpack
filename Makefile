GO ?= go

.PHONY: all build test race vet lint fmt fuzz-short clean golden bench bench-batch

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

# Smoke fuzz runs (5s each) for every fuzz target; use FUZZ_TIME for longer runs.
fuzz-short:
	@for t in $$($(GO) list -f '{{range .FuzzTargets}}{{.}} {{end}}' ./...); do \
		base=$${t##*/}; pkg=$${t%/*}; \
		echo "fuzzing $$pkg :: $$base"; \
		$(GO) test $$pkg -run '^$$' -fuzz '^'$$base'$$' -fuzztime=5s || exit 1; \
	done

# Unified v1.2 benchmark matrix: one command reproduces the whole baseline
# (env + matrix + latency). Output saved to docs/bench-results.txt; use
# BENCHTIME to override iterations (default 3x) and BENCHCOUNT for runs.
BENCHTIME ?= 3x
BENCHCOUNT ?= 1
bench:
	mkdir -p docs
	$(GO) test -run '^$$' \
		-bench 'Benchmark(Env|MainMatrix|Latency)' \
		-benchtime=$(BENCHTIME) -benchmem -count=$(BENCHCOUNT) -v . \
		2>&1 | tee docs/bench-results.txt

# Tier 1 batch-read comparison (baseline Get vs ReadBatch), 10s per scenario.
# Larger stores (1M rows) make this slower than the main matrix on purpose.
bench-batch:
	$(GO) test -run '^$$' -bench 'Benchmark(BatchBaselineGet|ReadBatch)$$' \
		-benchmem -benchtime=10s -count=1 .

# Regenerate every golden file from the current implementation.
# Golden files must be reviewed in the same change as the format change.
golden:
	$(GO) test . -run 'TestGolden' -args -update-golden

clean:
	rm -rf *.test coverage.out
