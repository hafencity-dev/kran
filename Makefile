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

.PHONY: help build test lint ui release-snapshot install smoke chaos clean

help: ## list targets
	@grep -E "^[a-z-]+:.*## " $(MAKEFILE_LIST) | sed "s/:.*## /\t/" | sort

build: ## build bin/kran for this machine
	@mkdir -p bin
	$(GOBUILD) -o bin/kran ./cmd/kran

test: ## go test -race, gently
	nice -n 10 go test -race -p 4 ./...

lint: ## gofmt, vet for linux and darwin, staticcheck when installed
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...
	@if command -v staticcheck >/dev/null 2>&1; then staticcheck ./...; else echo "staticcheck not installed, skipped"; fi

ui: ## build the web dashboard into internal/ui/dist
	@if [ -f web/package.json ]; then cd web && npm ci && npm run build; \
	else echo "web/ not present in this checkout; internal/ui/dist is used as committed"; fi

release-snapshot: ## local goreleaser dry run into dist/
	goreleaser release --snapshot --clean

install: ## install to $(PREFIX) and fill the binary cache for all four platforms
	@mkdir -p "$(PREFIX)" bin
	$(GOBUILD) -o bin/kran ./cmd/kran
	cp bin/kran "$(PREFIX)/.kran.tmp" && chmod 755 "$(PREFIX)/.kran.tmp" && mv -f "$(PREFIX)/.kran.tmp" "$(PREFIX)/kran"
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; dir="$(CACHE)/bin/$(VERSION)/$$os-$$arch"; \
		mkdir -p "$$dir" && GOOS=$$os GOARCH=$$arch $(GOBUILD) -o "$$dir/kran" ./cmd/kran || exit 1; \
		echo "cached $$dir/kran"; \
	done
	@echo "installed $(PREFIX)/kran $(VERSION). A running daemon keeps the old binary: run: kran service restart (waits for idle jobs)."
	@case ":$$PATH:" in *":$(PREFIX):"*) ;; *) echo "note: $(PREFIX) is not on your PATH";; esac

smoke: ## real-machine smoke test: make smoke NODE=devmac
	@test -n "$(NODE)" || { echo "usage: make smoke NODE=<linked node>"; exit 2; }
	@if ls internal/e2e/*_test.go >/dev/null 2>&1; then KRAN_SMOKE_NODE=$(NODE) go test -tags e2e -run Smoke -count=1 ./internal/e2e/...; \
	else echo "smoke tests are not implemented yet (internal/e2e has no tests)"; fi

chaos: ## randomized failure suite
	@if ls internal/e2e/*_test.go >/dev/null 2>&1; then go test -tags e2e -run Chaos -count=1 ./internal/e2e/...; \
	else echo "chaos tests are not implemented yet (internal/e2e has no tests)"; fi

clean: ## remove build output
	rm -rf bin dist

include ui.mk
