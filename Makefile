GO ?= go

.PHONY: all build test race vet lint fmt fuzz-short clean golden

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

# Regenerate every golden file from the current implementation.
# Golden files must be reviewed in the same change as the format change.
golden:
	$(GO) test ./... -run 'TestGolden' -args -update-golden

clean:
	rm -rf *.test coverage.out
