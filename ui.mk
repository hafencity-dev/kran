# UI targets (owner D). Included from the root Makefile: include ui.mk
# Standalone: make -f ui.mk ui
.PHONY: ui ui-dev ui-mock ui-shots

# Build the dashboard into internal/ui/dist (committed, embedded by go build).
ui:
	cd web && npm ci --no-audit --no-fund && npm run build

# Mock daemon on :7271 (serves the API and the built UI), then Vite on :5173.
ui-mock:
	go run ./internal/ui/mockserver

ui-dev:
	cd web && npm run dev

# Screenshots against a running mock (make ui-mock in another shell).
ui-shots:
	cd web && SETTLE=40000 node scripts/shoot.mjs
