# a2p2 developer tasks. `make check` is what CI runs.
GO      ?= go
BIN     := bin/a2p2
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: trace build test check fmt vet windows licences lint vuln fuzz bench e2e review clean release

build:            ## build bin/a2p2
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/a2p2

# The race tests with -count=1 -timeout 30m. Packages whose tests take 20
# seconds or more by scripts/test-timings.tsv run as SHARDS processes of
# one compiled test binary (scripts/gotest.sh; docs/TESTING.md). SHARDS
# defaults to the number of CPUs, at most 12.
.PHONY: test-timings
test:             ## unit, property and integration tests with the race detector, slow packages sharded [SHARDS=n]
	GO=$(GO) scripts/gotest.sh

test-timings:     ## run the tests and rewrite scripts/test-timings.tsv, by which the shards are balanced
	GO=$(GO) TIMINGS_OUT=scripts/test-timings.tsv scripts/gotest.sh

fmt:              ## fail if any file is not gofmt-clean
	@out=$$(gofmt -l cmd internal); [ -z "$$out" ] || { echo "gofmt needed:"; echo "$$out"; exit 1; }

vet:
	$(GO) vet ./...

windows:          ## build and vet (test files included) for Windows on amd64 and arm64; running there is the dispatch-only CI job
	GOOS=windows GOARCH=amd64 $(GO) build ./...
	GOOS=windows GOARCH=amd64 $(GO) vet ./...
	GOOS=windows GOARCH=arm64 $(GO) build ./...
	GOOS=windows GOARCH=arm64 $(GO) vet ./...

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

# The steps of check run at the same time (scripts/gate.sh): one line per
# step with its time, then the full log of any step that failed.
CHECK_STEPS := fmt vet windows licences trace recipes test team-test
check:            ## everything CI runs except lint/vuln (which need network), steps at the same time
	@scripts/gate.sh $(foreach s,$(CHECK_STEPS),$(s)='$(MAKE) --no-print-directory $(s)')

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

release:          ## static binaries a2p2_<os>_<arch> (.exe on Windows) and SHA256SUMS in dist/ (VERSION=v1.2.3 to name it)
	@rm -rf dist && mkdir dist && for t in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/arm64 windows/amd64; do \
	  os=$${t%/*}; arch=$${t#*/}; ext=""; [ "$$os" = windows ] && ext=.exe; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
	    -o dist/a2p2_$${os}_$$arch$$ext ./cmd/a2p2 || exit 1; done; \
	cd dist && if command -v sha256sum >/dev/null; then sha256sum a2p2_*; else shasum -a 256 a2p2_*; fi > SHA256SUMS

# The same tests on Linux, in a container, without leaving this machine:
# the hosted CI is kept small on purpose (docs/RELEASING.md). The module and
# build caches live in named volumes so a second run is quick.
.PHONY: check-linux prerelease
GO_IMAGE ?= golang:$(shell awk '/^go /{print $$2}' go.mod)
LINUX_SHARDS ?= 8
check-linux:      ## go vet and the race tests on Linux in a container, sharded as make test (needs Docker)
	docker run --rm -v "$(CURDIR)":/src:ro -v a2p2-gomod:/go/pkg/mod -v a2p2-gocache:/root/.cache/go-build \
	  -e SHARDS=$(LINUX_SHARDS) -w /work $(GO_IMAGE) sh -c 'cp -a /src/. /work/ && rm -rf /work/bin /work/dist /work/.chaos /work/.prerelease /work/.a2p2-dev /work/.claude && go vet ./... && scripts/gotest.sh'

# check, lint, vuln, check-linux and chaos at the same time, each to its
# own log in .prerelease/; on a pass on a clean tree it writes
# .prerelease/stamp, which make tag requires (scripts/prerelease.sh).
prerelease:       ## the local gate before a tag, steps at the same time; writes the stamp make tag needs
	@MAKE='$(MAKE)' scripts/prerelease.sh

.PHONY: tag verify-release
tag:              ## make tag VERSION=v1.2.3: refuses unless make prerelease passed on HEAD, the tree is clean and pushed, and CHANGELOG.md has the section; then tags and pushes
	@[ "$(origin VERSION)" = "command line" ] || { echo "usage: make tag VERSION=v1.2.3"; exit 2; }
	scripts/tag.sh $(VERSION)

verify-release:   ## make verify-release VERSION=v1.2.3: waits for the release workflow, checks assets, checksum and version from this machine (needs gh)
	@[ "$(origin VERSION)" = "command line" ] || { echo "usage: make verify-release VERSION=v1.2.3"; exit 2; }
	scripts/verify-release.sh $(VERSION)

clean:
	rm -rf bin dist

# Team launcher (scripts/team): a layer on top of a2p2, tested with a fake harness.
.PHONY: team-test
team-test: build  ## the team launcher against a fake harness and a throwaway home (about 5 seconds)
	python3 scripts/team/test_team.py

# Chaos: real daemons in Docker containers, killed, paused, disconnected and
# partitioned from a seed (scripts/chaos). Needs Docker and minutes; not part
# of check. Logs and the summary go to .chaos/ (git-ignored).
.PHONY: chaos
chaos:            ## make chaos [NODES=3] [SEED=1] [DURATION=90]; exit 1 violation, 2 run or cleanup failed, 3 inconclusive
	python3 scripts/chaos/chaos.py run --nodes $${NODES:-3} --seed $${SEED:-1} --duration $${DURATION:-90}
