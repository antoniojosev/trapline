# Local commands mirror CI exactly. If `make check` passes, CI passes —
# anything else makes the gates advisory, which is the same as not having them.

BINARY      := trapline
CMD         := ./cmd/trapline
# The published promise is a ~30 MB binary, so the gate enforces the promise.
# If this trips, that is a design conversation, not a number to raise.
MAX_BIN_MB  := 30
LINT_VERSION := v2.12.2
# Pinned like the linter, and for the same reason: a security scanner that
# changes its ruleset under you turns a green nightly into a red one with no
# commit to blame.
GOSEC_VERSION := v2.22.9
FUZZTIME     := 60s

# Every target declares its own .PHONY line, right above itself.
#
# One shared list at the top read better and was the single most conflicted
# line in the repository: four sessions in a row added a target to it and all
# four had to resolve the same conflict by hand. A declaration that lives next
# to what it declares is never edited by two people at once.

.PHONY: workflows help
help:
	@grep -E '^[a-z-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: bootstrap
bootstrap: ## Check and install what the gates need, idempotently
	./scripts/bootstrap.sh

.PHONY: check
check: fmt-check vet lint vuln workflows test-race size ## Everything CI runs on a PR
.PHONY: check-all
check-all: check smoke ## check plus the end-to-end run

.PHONY: fmt
fmt: ## Format the tree
	gofmt -w .

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt'd:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
workflows: ## Los workflows declaran jobs que GitHub puede ejecutar
	@python3 scripts/check-workflows.py

vet: ## go vet
	go vet ./...

.PHONY: lint
lint: ## golangci-lint, including the depguard boundary rules
	golangci-lint run

.PHONY: fuzz
fuzz: ## Fuzz the envelope parser (override the duration with FUZZTIME=5m)
	go test -run=XXX -fuzz=FuzzParse -fuzztime=$(FUZZTIME) ./internal/envelope/

.PHONY: bench
bench: ## Ingest throughput gate — the executable form of ADR 001's volume claim
	# Both workloads: errors, and transactions. An installation that takes a
	# hundred errors a second and forty transactions a second has kept half
	# the promise, so the second one is gated at the same threshold rather
	# than extrapolated from the first.
	TRAPLINE_BENCH=1 go test -count=1 \
		-run 'TestIngestThroughputGate|TestTransactionThroughputGate' \
		-v -timeout 30m ./internal/bench/

.PHONY: bench-sourcemap
bench-sourcemap: ## What symbolication costs the ingest path (ADR 018's 10% budget)
	# Apart from `bench` because it asserts on a ratio between two
	# measurements rather than on a throughput figure, and because it is the
	# only gate whose subject is a feature that can be switched off.
	TRAPLINE_BENCH=1 go test -count=1 \
		-run 'TestSymbolicationOverheadGate' \
		-v -timeout 20m ./internal/bench/

.PHONY: vuln
vuln: ## Known vulnerabilities in what this code actually calls
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "govulncheck not installed; run: make tools"; exit 1; }
	govulncheck ./...

.PHONY: test
test: ## Tests
	go test ./...

.PHONY: test-race
test-race: ## Tests with the race detector — ingestion is pure concurrency
	go test -race ./...

.PHONY: cover
cover: ## Coverage per package
	go test -cover ./...

.PHONY: build
build: ## Build for this machine
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) $(CMD)

.PHONY: build-all
build-all: ## Cross-compile the release targets (proves CGO-free, ADR 009)
	@for target in linux/amd64 linux/arm64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  $$target"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w" \
			-o /dev/null $(CMD) || exit 1; \
	done

.PHONY: size
size: build ## Enforce the binary size budget
	@bytes=$$(stat -c%s $(BINARY)); \
	mb=$$((bytes / 1024 / 1024)); \
	if [ $$mb -gt $(MAX_BIN_MB) ]; then \
		echo "binary is $${mb}MB, budget is $(MAX_BIN_MB)MB"; exit 1; \
	fi; \
	echo "binary $${mb}MB / $(MAX_BIN_MB)MB budget"

.PHONY: install-test
install-test: ## Build a real snapshot release and install it on a clean Debian
	./scripts/install-test.sh

.PHONY: docker
docker: ## Build the container image (FROM scratch; ADR 009 is what allows it)
	docker build --build-arg VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev) \
		-t trapline:local .

.PHONY: web
web: ## Rebuild the embedded panel (needs Node)
	cd web && npm ci --no-fund --no-audit && npx tsc -b --noEmit && npx vite build

.PHONY: compat
compat: ## Compatibility matrix: the SDKs that need no container (runs on every PR)
	./scripts/compat.sh --tier smoke

.PHONY: compat-full
compat-full: ## The whole matrix, containers included (nightly; needs Docker)
	./scripts/compat.sh --tier full

.PHONY: smoke
smoke: ## End-to-end: boot, set up, create a project, back up, measure memory
	./scripts/smoke.sh

.PHONY: stats
stats: ## The dashboard end to end: buckets, search, and history that outlives events
	./scripts/stats.sh
.PHONY: releases
releases: ## The release lifecycle end to end, including "resolve in next release"
	./scripts/releases.sh

.PHONY: alerts
alerts: ## Alerting end to end: five channels, silence, an outage and a hard restart
	./scripts/alerts.sh
.PHONY: digest
digest: ## The weekly report end to end: two weeks of history, and what survives retention
	./scripts/digest.sh

.PHONY: crons
crons: ## Cron monitors end to end, with one real minute of real time
	./scripts/crons.sh

.PHONY: fuzz-cron
fuzz-cron: ## Fuzz the cron parser's one invariant: Next(t) is always after t
	go test -run=XXX -fuzz=FuzzCronNext -fuzztime=$(FUZZTIME) ./internal/domain/

.PHONY: uptime
uptime: ## Uptime monitoring end to end: a container switched off, the SSRF guard, the alert
	./scripts/uptime.sh

.PHONY: tracing
tracing: ## Tracing end to end: known latencies, sampling, and a downsample that keeps the percentiles
	./scripts/tracing.sh

.PHONY: fuzz-sketch
fuzz-sketch: ## Fuzz the latency sketch's decoder, which reads bytes off disk
	go test -run=XXX -fuzz=FuzzUnmarshalBinary -fuzztime=$(FUZZTIME) ./internal/engine/sketch/

.PHONY: fuzz-transaction
fuzz-transaction: ## Fuzz the transaction decoder, which reads bytes from a public endpoint
	go test -run=XXX -fuzz=FuzzDecodeTransaction -fuzztime=$(FUZZTIME) ./internal/sentry/

.PHONY: fuzz-sourcemap
fuzz-sourcemap: ## Fuzz the source map parser, which reads a file a user uploaded
	go test -run=XXX -fuzz=FuzzSourceMap -fuzztime=$(FUZZTIME) ./internal/sourcemap/

.PHONY: fuzz-bundle
fuzz-bundle: ## Fuzz the artifact bundle reader, which parses an archive somebody uploaded
	go test -run=XXX -fuzz=FuzzRead -fuzztime=$(FUZZTIME) ./internal/artifactbundle/

.PHONY: sourcemaps
sourcemaps: ## Source maps end to end: the real sentry-cli uploading, and a check with teeth (nightly; needs Docker)
	./scripts/sourcemaps.sh


.PHONY: health
health: ## Release health end to end: a crash-free rate, two kinds of restart, and a full window
	./scripts/health.sh

.PHONY: fuzz-session
fuzz-session: ## Fuzz the session decoders, which read bytes from a public endpoint
	go test -run=XXX -fuzz=FuzzDecodeSession -fuzztime=$(FUZZTIME) ./internal/sentry/

.PHONY: mcp
mcp: ## The fourth client end to end: stdio and HTTP, one table, and a refusal with teeth
	./scripts/mcp.sh

.PHONY: sentry-cli
sentry-cli: ## The release lifecycle driven by the real sentry-cli (nightly; needs Docker)
	./scripts/sentry-cli.sh

.PHONY: ui-smoke
ui-smoke: ## The panel in a real, version-pinned browser (needs Docker)
	./scripts/ui-smoke.sh

.PHONY: changelog
changelog: ## Preview the next release from changelog.d/ (VERSION=vX.Y.Z writes it in)
	./scripts/changelog.sh $(VERSION)

.PHONY: tools
tools: ## Install the pinned dev tooling
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(LINT_VERSION)
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/securego/gosec/v2/cmd/gosec@$(GOSEC_VERSION)

.PHONY: hardening
hardening: ## The security pass end to end: ceilings, bombs, headers, CSRF, tokens, gosec, fuzz
	./scripts/hardening.sh

.PHONY: footprint
footprint: ## Measure what this costs to run, minimum and with everything switched on
	./scripts/footprint.sh

.PHONY: docs
docs: ## The documentation as a gate: every runnable example runs, every link resolves
	./scripts/docs.sh

.PHONY: demo
demo: ## The star demo, end to end, with the agent step replaced by its patches
	./demo/demo.sh --no-agent

.PHONY: clean
clean:
	rm -f $(BINARY) coverage.out
