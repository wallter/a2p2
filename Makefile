# a2p2 developer tasks. `make check` is what CI runs.
GO      ?= go
BIN     := bin/a2p2
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: trace build test check fmt vet windows licences lint vuln fuzz bench e2e review clean release

build:            ## build bin/a2p2
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/a2p2

test:             ## unit, property and integration tests with the race detector
	$(GO) test -race -count=1 ./...

fmt:              ## fail if any file is not gofmt-clean
	@out=$$(gofmt -l cmd internal); [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

vet:
	$(GO) vet ./...

windows:          ## a2p2 does not run on Windows yet, but it must build and vet there
	GOOS=windows GOARCH=amd64 $(GO) build ./...
	GOOS=windows GOARCH=amd64 $(GO) vet ./...

licences:         ## requirement I4: only allow-listed direct dependencies; prints their licence files
	scripts/licences.sh

lint:             ## staticcheck (run without installing; needs network)
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

vuln:             ## govulncheck (needs network)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.10.0 ./...

trace:            ## every requirement has a row in docs/TRACE.md, and every test it names exists
	scripts/trace.sh

.PHONY: recipes
recipes:          ## requirement I3: the recipes and the skill name only tools, arguments and commands that exist
	scripts/recipes.sh

check: fmt vet windows licences trace test recipes ## everything CI runs except lint/vuln (which need network)

fuzz:             ## short fuzz smoke run of the record and entry decoders; FUZZTIME=10m for a long one
	$(GO) test ./internal/core -run '^$$' -fuzz '^FuzzParseRecord$$' -fuzztime $${FUZZTIME:-20s}
	$(GO) test ./internal/core -run '^$$' -fuzz '^FuzzParseEntry$$' -fuzztime $${FUZZTIME:-20s}

bench:
	$(GO) test -run '^$$' -bench . -benchmem ./...

# The deep random differential tests at other seeds, one after another
# (about two minutes a seed). A failure logs its operations; reduce them
# with A2P2_MODEL_REDUCE=file A2P2_MODEL_MATCH=text go test -run
# TestReduceAFailingSchedule ./internal/node. Not part of check.
.PHONY: model-sweep
model-sweep:      ## make model-sweep [SEEDS="1 2 3"]: deep random differential runs per seed; exits 1 if any fails
	@mkdir -p .model-sweep; fail=0; for s in $${SEEDS:-1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20}; do \
	  if A2P2_MODEL_DEEP=1 A2P2_MODEL_SEED=$$s A2P2_MODEL_DUMP=1 $(GO) test -count=1 -timeout 60m \
	    -run '^(TestRealExchangeAgreesWithTheDeliveryModelOnRandomSchedules|TestImplementationAgreesWithTheDeliveryModelOnRandomSchedules)$$' \
	    ./internal/node > .model-sweep/seed-$$s.log 2>&1; then echo "seed $$s ok"; rm -f .model-sweep/seed-$$s.log; \
	  else echo "seed $$s FAILED (.model-sweep/seed-$$s.log)"; fail=1; fi; done; exit $$fail

e2e: build        ## real harnesses, spends model tokens: make e2e HARNESS=claude
	scripts/e2e/run.sh $${HARNESS:-all}

.PHONY: e2e-local
e2e-local: build  ## OpenCode, Hermes, Pi on local Ollama models: make e2e-local HARNESS=opencode MODELS="gemma4:e4b"
	scripts/e2e/local.sh $${HARNESS:-all} $${MODELS:-}

review:           ## independent review of uncommitted changes: make review WHO=sol
	scripts/review.sh $${WHO:-sol}

release:          ## static binaries a2p2_<os>_<arch> and SHA256SUMS in dist/ (VERSION=v1.2.3 to name it)
	@rm -rf dist && mkdir dist && for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
	    -o dist/a2p2_$${os}_$$arch ./cmd/a2p2 || exit 1; done; \
	cd dist && if command -v sha256sum >/dev/null; then sha256sum a2p2_*; else shasum -a 256 a2p2_*; fi > SHA256SUMS

clean:
	rm -rf bin dist

# Team launcher (scripts/team): a layer on top of a2p2, tested with a fake harness.
.PHONY: team-test
check: team-test
team-test: build  ## the team launcher against a fake harness and a throwaway home (about 5 seconds)
	python3 scripts/team/test_team.py

# Chaos: real daemons in Docker containers, killed, paused, disconnected and
# partitioned from a seed (scripts/chaos). Needs Docker and minutes; not part
# of check. Logs and the summary go to .chaos/ (git-ignored).
.PHONY: chaos
chaos:            ## make chaos [NODES=3] [SEED=1] [DURATION=90]; exit 1 violation, 2 run or cleanup failed, 3 inconclusive
	python3 scripts/chaos/chaos.py run --nodes $${NODES:-3} --seed $${SEED:-1} --duration $${DURATION:-90}
