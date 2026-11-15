SHELL := /bin/sh
MODULE  := github.com/hafencity-dev/kran
VERSION := $(or $(VERSION),$(shell v=$$(git describe --tags --always --dirty 2>/dev/null | sed "s/^v//"); echo "$${v:-dev}"))
COMMIT  := $(or $(COMMIT),$(shell git rev-parse --short HEAD 2>/dev/null || echo none))
DATE    := $(or $(DATE),$(shell date -u +%Y-%m-%dT%H:%M:%SZ))
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildDate=$(DATE)
GOBUILD := CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)"

# Same rules as internal/paths.CacheDir: other-platform binaries live here
# so `kran link` can push them without downloading.
ifneq ($(KRAN_CACHE_DIR),)
CACHE ?= $(KRAN_CACHE_DIR)
else ifeq ($(shell uname -s),Darwin)
CACHE ?= $(HOME)/Library/Caches/kran
else ifneq ($(XDG_CACHE_HOME),)
CACHE ?= $(XDG_CACHE_HOME)/kran
else
CACHE ?= $(HOME)/.cache/kran
endif
PREFIX ?= $(HOME)/.local/bin
PLATFORMS := linux/arm64 linux/amd64 darwin/arm64 darwin/amd64

.PHONY: help build test fmt-check lint ui release-snapshot install cache e2e smoke chaos clean

help: ## list targets
	@grep -E "^[a-z-]+:.*## " $(MAKEFILE_LIST) | sed "s/:.*## /\t/" | sort

build: ## build bin/kran for this machine
	@mkdir -p bin
	$(GOBUILD) -o bin/kran ./cmd/kran

# On a systemd Linux host the test suites run in a scope capped at
# TEST_MEM memory and TEST_TASKS processes and threads: a test that goes
# wrong (a runaway self-start, a leak) fails alone instead of freezing the
# machine. `make test TEST_CAP=` runs uncapped.
TEST_MEM   ?= 24G
TEST_TASKS ?= 8192
TEST_CAP   ?= $(shell [ "$$(uname -s)" = Linux ] && command -v systemd-run >/dev/null 2>&1 && \
	systemd-run --user --scope --quiet --collect true >/dev/null 2>&1 && \
	echo systemd-run --user --scope --quiet --collect -p MemoryMax=$(TEST_MEM) -p MemorySwapMax=0 -p TasksMax=$(TEST_TASKS) --)

test: ## go test -race, gently and capped
	$(TEST_CAP) nice -n 10 go test -race -p 4 ./...

# third_party holds vendored forks kept byte-identical to upstream.
fmt-check: ## fail when a Go file needs gofmt
	@out=$$(gofmt -l $$(find . -name '*.go' -not -path './third_party/*' -not -path './web/*')); \
	if [ -n "$$out" ]; then echo "gofmt needed (run gofmt -w on these):"; echo "$$out"; exit 1; fi

lint: fmt-check ## gofmt, vet for linux and darwin, staticcheck when installed
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed, skipped"; fi

ui: ## build the web dashboard into internal/ui/dist
	@if [ -f web/package.json ]; then cd web && npm ci && npm run build; \
	else echo "web/ not present in this checkout; internal/ui/dist is used as committed"; fi

release-snapshot: ## local goreleaser dry run into dist/
	goreleaser release --snapshot --clean

# The binary cache entry of this commit: all four platforms and the
# SHA256SUMS that `kran upgrade --version <this version>` checks (a build
# between releases comes from the cache only). The build date is the
# commit's (UTC), so the same commit and Go give the same bytes. Refuses a
# dirty tree: a cached version is one commit.
COMMIT_DATE = $(shell TZ=UTC git log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ 2>/dev/null)
CACHE_LDFLAGS = -s -w -X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) -X $(MODULE)/internal/version.BuildDate=$(COMMIT_DATE)
SHA256 = $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo "shasum -a 256")

cache: ## fill the binary cache for linux/darwin x arm64/amd64 with SHA256SUMS (no install)
	@case "$(VERSION)" in *dirty*) echo "the tree has uncommitted changes; commit first (a cached version is one commit)"; exit 1;; esac
	@set -e; dir="$(CACHE)/bin/$(VERSION)"; mkdir -p "$$dir"; : > "$$dir/SHA256SUMS.tmp"; \
	for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; mkdir -p "$$dir/$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(CACHE_LDFLAGS)" -o "$$dir/$$os-$$arch/kran.tmp" ./cmd/kran; \
		mv "$$dir/$$os-$$arch/kran.tmp" "$$dir/$$os-$$arch/kran"; \
		(cd "$$dir" && $(SHA256) "$$os-$$arch/kran") >> "$$dir/SHA256SUMS.tmp"; \
		echo "cached $$dir/$$os-$$arch/kran"; \
	done; \
	mv "$$dir/SHA256SUMS.tmp" "$$dir/SHA256SUMS"; cat "$$dir/SHA256SUMS"

install: ## install to $(PREFIX) and fill the binary cache for all four platforms
	@mkdir -p "$(PREFIX)" bin
	$(GOBUILD) -o bin/kran ./cmd/kran
	cp bin/kran "$(PREFIX)/.kran.tmp" && chmod 755 "$(PREFIX)/.kran.tmp" && mv -f "$(PREFIX)/.kran.tmp" "$(PREFIX)/kran"
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir="$(CACHE)/bin/$(VERSION)/$$os-$$arch"; \
		mkdir -p "$$dir" && GOOS=$$os GOARCH=$$arch $(GOBUILD) -o "$$dir/kran" ./cmd/kran || exit 1; \
		echo "cached $$dir/kran"; \
	done
	@echo "installed $(PREFIX)/kran $(VERSION). A running daemon keeps the old binary: run: kran service restart (running and queued jobs carry over)."
	@case ":$$PATH:" in *":$(PREFIX):"*) ;; *) echo "note: $(PREFIX) is not on your PATH";; esac

# System-level suites (internal/e2e) run the real kran binary, built once
# with the race detector; output stays visible (-v) for the summaries.
E2E_GO = $(TEST_CAP) nice -n 10 go test -count=1 -v
CHAOS_DURATION ?= 3m

e2e: ## black-box suite on real daemons (KRAN_TEST_DOCKER=1 adds container tests)
	KRAN_E2E_RACE=1 $(E2E_GO) -tags e2e -timeout 60m ./internal/e2e/

chaos: ## randomized failure suite: make chaos [CHAOS_DURATION=3m] [CHAOS_SEED=n]
	KRAN_E2E_RACE=1 KRAN_CHAOS_DURATION=$(CHAOS_DURATION) KRAN_CHAOS_SEED=$(CHAOS_SEED) \
		$(E2E_GO) -tags chaos -run Chaos -timeout 60m ./internal/e2e/

smoke: ## real-machine smoke test: make smoke NODE=devmac
	@test -n "$(NODE)" || { echo "usage: make smoke NODE=<ssh host>"; exit 2; }
	KRAN_SMOKE_NODE=$(NODE) $(E2E_GO) -tags smoke -run Smoke -timeout 60m ./internal/e2e/

clean: ## remove build output
	rm -rf bin dist

include ui.mk
