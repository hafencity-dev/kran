# UI targets. Included from the root Makefile: include ui.mk
.PHONY: ui-dev ui-mock ui-shots

# `make ui` (build the dashboard into internal/ui/dist) lives in the root Makefile.

# Mock daemon on :7271 (serves the API and the built UI), then Vite on :5173.
ui-mock:
	go run ./internal/ui/mockserver

ui-dev:
	cd web && npm run dev

# Screenshots against a running mock (make ui-mock in another shell).
ui-shots:
	cd web && SETTLE=40000 node scripts/shoot.mjs
