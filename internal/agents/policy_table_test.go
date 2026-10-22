package agents

import "testing"

// TestWrapDecisionTable lists commands as coding agents really write them,
// each with the decision and the reason. The rule it pins down: move what is
// long-running and whose product is an exit code plus output (tests, checks,
// type checks, lints, builds over many packages); keep everything that is
// quick, interactive, long-lived, or meant to leave a file, image, install or
// edit on THIS machine.
func TestWrapDecisionTable(t *testing.T) {
	const (
		wrap = true
		keep = false
	)
	for _, tc := range []struct {
		cmd  string
		want bool
		why  string
	}{
		// --- go
		{"go test ./...", wrap, "the canonical heavy job"},
		{"go test -race -count=1 -timeout 10m ./internal/...", wrap, "flags do not matter"},
		{"go test ./pkg/store -run TestOpen -v", wrap, "tests may be slow or need CPU even when narrowed"},
		{"cd backend && go test ./... 2>&1 | tail -n 40", wrap, "cd, pipe to tail"},
		{"go test ./... -json | grep -c fail", wrap, "pipe to grep"},
		{"timeout 600 go test ./...", wrap, "timeout prefix"},
		{"set -e; go build ./... && go test ./...", wrap, "set -e is harmless"},
		{"set -euo pipefail\ngo vet ./...", wrap, "multi-line script header"},
		{"go vet ./...", wrap, "whole module"},
		{"go build ./...", wrap, "compile check, no artifact"},
		{"go build -o /dev/null ./cmd/kran", wrap, "compile check"},
		{"go vet ./pkg", keep, "one package is quick"},
		{"go vet", keep, "current package is quick"},
		{"go build ./cmd/kran", keep, "writes ./kran here"},
		{"go build .", keep, "writes a binary here"},
		{"go build -o bin/kran ./cmd/kran", keep, "artifact wanted here"},
		{"go test -coverprofile=cover.out ./...", keep, "cover.out is wanted here"},
		{"go test ./... -cpuprofile cpu.out", keep, "profile is wanted here"},
		{"go test ./... -update", keep, "golden files are rewritten here"},
		{"go run ./cmd/server", keep, "long-lived program"},
		{"go mod tidy", keep, "edits go.mod here"},
		{"go get github.com/x/y@latest", keep, "edits go.mod here"},
		{"gofmt -l .", keep, "quick"},
		{"gofmt -w main.go", keep, "edits files here"},
		{"go fmt ./...", keep, "edits files here"},
		{"golangci-lint run ./...", wrap, "slow linter over the module"},
		{"golangci-lint run --fix", keep, "edits files here"},

		// --- node
		{"npm test", wrap, ""},
		{"CI=true npm test -- --watchAll=false", wrap, "watch explicitly off"},
		{"pnpm test --watch=false", wrap, "watch explicitly off"},
		{"cd web && pnpm test && pnpm build", wrap, "chain of heavy steps"},
		{"npm run build", wrap, "build; output stays on the other machine"},
		{"NODE_OPTIONS=--max-old-space-size=8192 npm run build", wrap, "env prefix"},
		{"pnpm -r build", wrap, "workspace build"},
		{"pnpm --filter @app/web test", wrap, "filter flag with value"},
		{"npx vitest run --reporter=dot", wrap, ""},
		{"npx tsc --noEmit -p tsconfig.json", wrap, "type check"},
		{"turbo run build test --filter=web", wrap, "task runner over heavy tasks"},
		{"npx nx run-many -t test", wrap, "task runner over heavy tasks"},
		{"npm run lint", wrap, "lint script"},
		{"npm run lint -- --fix", keep, "edits files here"},
		{"pnpm lint --fix", keep, "edits files here"},
		{"npx vitest -u", keep, "rewrites snapshots here"},
		{"npx jest --updateSnapshot", keep, "rewrites snapshots here"},
		{"npx prettier --write .", keep, "edits files here"},
		{"npm test -- --watch", keep, "watcher"},
		{"npm run test:watch", keep, "watcher"},
		{"npm run dev", keep, "dev server"},
		{"pnpm dev --port 3001", keep, "dev server"},
		{"next dev", keep, "dev server"},
		{"next start", keep, "server"},
		{"vite preview", keep, "server"},
		{"npm run storybook", keep, "server"},
		{"npm install", keep, "node_modules belongs here"},
		{"pnpm install --frozen-lockfile", keep, "node_modules belongs here"},
		{"pnpm add zod", keep, "edits package.json here"},
		{"npm ci && npm test", keep, "install must run here"},
		{"node scripts/seed.js", keep, "not a test or build"},

		// --- python
		{"pytest -k 'parse and not slow' -x", wrap, ""},
		{"pytest tests/unit -q -n auto", wrap, ""},
		{"python -m pytest --maxfail=1", wrap, ""},
		{"uv run pytest -q", wrap, ""},
		{"pytest --junitxml=report.xml", keep, "report file wanted here"},
		{"pytest --cov=app --cov-report=html", keep, "report directory wanted here"},
		{"pytest --looponfail", keep, "watcher"},
		{"python manage.py runserver", keep, "server"},
		{"python script.py", keep, "arbitrary program"},
		{"pip install -r requirements.txt", keep, "environment belongs here"},
		{"uv sync", keep, "environment belongs here"},

		// --- rust, jvm, native
		{"cargo test --workspace -- --nocapture", wrap, ""},
		{"cargo clippy --all-targets -- -D warnings", wrap, ""},
		{"cargo nextest run", wrap, ""},
		{"cargo build --release", wrap, "build; binary stays on the other machine"},
		{"cargo run", keep, "runs a program"},
		{"cargo fmt", keep, "edits files here"},
		{"make -j$(nproc) test", wrap, "the remote shell expands nproc"},
		{"make -j8 build", wrap, ""},
		{"make lint", wrap, ""},
		{"make", keep, "default target is unknown"},
		{"make -j8", keep, "default target is unknown"},
		{"make install", keep, "installs here"},
		{"make dev", keep, "server"},
		{"make clean", keep, "quick, local files"},
		{"./gradlew test --tests com.acme.FooTest", wrap, ""},
		{"mvn -pl api -am test", wrap, ""},
		{"cmake --build build -j8", wrap, ""},
		{"ctest --test-dir build -j8", wrap, ""},

		// --- containers
		{"docker build -t app:dev .", wrap, "image stays on the other machine"},
		{"docker buildx build --platform linux/amd64 .", wrap, ""},
		{"docker build --push -t reg/app .", keep, "registry login lives here"},
		{"docker buildx build --load .", keep, "image wanted here"},
		{"docker build -o out .", keep, "files wanted here"},
		{"docker compose up -d", keep, "starts services here"},
		{"docker compose up -d && go test ./...", keep, "tests need the services here"},
		{"docker compose down", keep, "local services"},
		{"docker ps", keep, "quick"},
		{"docker run --rm -it alpine sh", keep, "interactive"},

		// --- not build jobs at all
		{"git status", keep, ""},
		{"git diff --stat && go test ./...", keep, "unknown command in the chain"},
		{"cat go.mod", keep, ""},
		{"grep -rn TODO .", keep, ""},
		{"ls -la", keep, ""},
		{"curl -s localhost:8080/health", keep, "needs local services"},
		{"go test ./... && curl localhost:8080/health", keep, "needs local services"},
		{"sleep 5 && go test ./...", keep, "unknown command in the chain"},
		{"tail -f server.log", keep, "follows a file"},
		{"kran run -- go test ./...", keep, "already kran"},
		{"/usr/local/bin/kran run --sh 'make test'", keep, "already kran"},
		{"KRAN_NO_HOOK=1 go test ./...", keep, "explicit opt-out"},
		{"go test ./... | tee out.log", keep, "writes a file here"},
		{"go test ./... > out.log 2>&1", keep, "writes a file here"},
		{"go test ./... &", keep, "background job"},
	} {
		got, ok := WrapCommand(tc.cmd)
		if ok != tc.want {
			t.Errorf("WrapCommand(%q) = %v, want %v (%s)", tc.cmd, ok, tc.want, tc.why)
		}
		if prefix, rest := hoistCd(tc.cmd); ok && got != prefix+"kran run --sh "+ShellQuote(rest) {
			t.Errorf("WrapCommand(%q) wrapped to %q", tc.cmd, got)
		}
	}
}
