# a2p2 developer tasks. `make check` is what CI runs.
GO      ?= go
BIN     := bin/a2p2
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: trace build test check fmt vet lint vuln fuzz bench e2e review clean release

build:            ## build bin/a2p2
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/a2p2

test:             ## unit, property and integration tests with the race detector
	$(GO) test -race -count=1 ./...

fmt:              ## fail if any file is not gofmt-clean
	@out=$$(gofmt -l .); [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

vet:
	$(GO) vet ./...

lint:             ## staticcheck (run without installing)
	$(GO) run honnef.co/go/tools/cmd/staticcheck@latest ./...

vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

trace:            ## every requirement has a row in docs/TRACE.md, and every test it names exists
	scripts/trace.sh

check: fmt vet trace test ## everything CI runs except lint/vuln (which need network)

fuzz:             ## short fuzz smoke run; FUZZTIME=10m for a long one
	$(GO) test ./internal/core -run '^$$' -fuzz FuzzDecodeEnvelope -fuzztime $${FUZZTIME:-20s}

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./...

e2e: build        ## real harnesses, spends model tokens: make e2e HARNESS=claude
	scripts/e2e/run.sh $${HARNESS:-all}

review:           ## independent review of uncommitted changes: make review WHO=sol
	scripts/review.sh $${WHO:-sol}

release:          ## cross-compile static binaries into dist/
	@mkdir -p dist; for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
	    -o dist/a2p2-$$os-$$arch ./cmd/a2p2 || exit 1; done; \
	cd dist && shasum -a 256 a2p2-* > SHA256SUMS

clean:
	rm -rf bin dist
