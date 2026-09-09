GO ?= go

.PHONY: all build test race vet lint fmt clean golden bench bench-batch

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
	mkdir -p docs
	$(GO) test -run '^$$' \
		-bench 'Benchmark(WriteFull|GetHot|GetCold|Scan|ReadBatch1000|GetLoop1000|OpenReplay|DeepChainGet|EncryptedWrite|EncryptedGetHot)$$' \
		-benchtime=$(BENCHTIME) -benchmem -count=$(BENCHCOUNT) . \
		2>&1 | tee docs/bench-results.txt

bench-batch:
	$(GO) test -run '^$$' -bench 'Benchmark(GetLoop1000|ReadBatch1000)$$' \
		-benchmem -benchtime=10s -count=1 .

golden:
	$(GO) test . -run 'TestGolden' -args -update-golden

clean:
	rm -rf *.test coverage.out
	rm -rf testdata/tmpdb
	find . -type d -path '*/testdata/tmpdb' -exec rm -rf {} + 2>/dev/null || true
