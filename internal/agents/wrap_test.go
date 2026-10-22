package agents

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWrapPolicy(t *testing.T) {
	wrap := []string{
		"go test ./...",
		"go test -race -count=1 ./internal/...",
		"go vet ./...",
		"go build ./...",
		"go build -o /dev/null ./cmd/kran",
		"cd /home/me/repo && go test ./...",
		"cd web && pnpm test && pnpm build",
		"FOO=1 go test ./...",
		"CGO_ENABLED=0 GOOS=linux go build ./...",
		"go test ./... 2>&1 | tail -n 20",
		"go test ./... 2>&1 | grep -v ok | head -50",
		"go test ./... > /dev/null 2>&1",
		"go test ./... &> /dev/null",
		"go build ./... && go vet ./... && go test ./...",
		"go test ./... || true",
		"go test ./...\n",
		"go test ./... ; echo done",
		"time go test ./...",
		"nice -n 10 go test ./...",
		"env CI=1 go test ./...",
		"/usr/local/go/bin/go test ./...",
		"cargo build --release",
		"cargo test",
		"cargo +nightly clippy --all-targets",
		"next build",
		"npx next build",
		"pnpm exec next build",
		"npx vitest run",
		"npx --yes jest --ci",
		"pnpm vitest run --coverage",
		"yarn jest",
		"npx playwright test --project=chromium",
		"pnpm dlx playwright test",
		"tsc -b",
		"npx tsc --build --force",
		"tsc --noEmit -p tsconfig.json",
		"npm test",
		"npm run test",
		"npm run build",
		"npm run typecheck",
		"npm run test:unit",
		"pnpm test",
		"pnpm build",
		"pnpm run lint",
		"pnpm --filter web build",
		"pnpm -r test",
		"yarn build",
		"bun test",
		"bun run build",
		"pytest",
		"pytest -x tests/ -q",
		"python -m pytest tests",
		"python3.12 -m pytest",
		"uv run pytest",
		"uv run --frozen python -m pytest -q",
		"poetry run pytest",
		"docker build -t foo .",
		"docker buildx build --platform linux/amd64 .",
		"make test",
		"make -j8 build",
		"make -C sub all",
		"make CC=clang check",
		"./gradlew build",
		"gradle test --no-daemon",
		"mvn -q package",
		"./mvnw verify",
		"dotnet test",
		"swift build",
		"bazel test //...",
		"cmake --build build -j8",
		"echo start && go test ./...",
		"export GOFLAGS=-mod=vendor && go test ./...",
		"GOFLAGS='-tags=a b' go test ./...",
	}
	keep := []string{
		"",
		"   ",
		"ls -la",
		"git status",
		"git push && go test ./...",
		"kran run -- go test ./...",
		"kran run --sh 'go test ./...'",
		"/usr/local/bin/kran run -- cargo build",
		"next dev",
		"npx next dev",
		"vite",
		"vite build --watch",
		"npm run dev",
		"npm run start",
		"npm run watch",
		"npm run test:watch",
		"pnpm dev",
		"pnpm --filter web dev",
		"npm install",
		"pnpm install",
		"docker compose up",
		"docker compose up -d && go test ./...",
		"docker run --rm alpine true",
		"docker build -o out .",
		"docker buildx build --load .",
		"go test ./... &",
		"go build -o bin/kran ./cmd/kran",
		"go build -o=bin/kran ./cmd/kran",
		"go run ./cmd/server",
		"go mod tidy",
		"go test ./... > out.txt",
		"go test ./... | tee out.txt",
		"go test ./... | tail -f log",
		"tail -f server.log",
		"sleep 30",
		"sudo go test ./...",
		"ssh host go test ./...",
		"cat input | go test ./...",
		"go test ./... < input.txt",
		"go test <<EOF\nx\nEOF",
		"python",
		"python script.py",
		"python -c 'print(1)'",
		"node",
		"psql",
		"vim file.go",
		"vitest --watch",
		"jest --watchAll",
		"vitest -w",
		"npx playwright test --ui",
		"npx playwright test --headed",
		"npx playwright show-report",
		"tsc",
		"tsc -w",
		"make",
		"make install",
		"make watch",
		"cargo run",
		"cargo build &",
		"go test ./... && curl localhost:8080",
		"go test ./... --url http://127.0.0.1:3000",
		"go test $(go list ./... | grep foo)",
		"go test `go list ./...`",
		"(cd web && go test ./...)",
		"if true; then go test ./...; fi",
		"go test ./... # comment",
		"KRAN_NO_HOOK=1 go test ./...",
		"export KRAN_NO_HOOK=1; go test ./...",
		"cd a b && go test ./...",
		"rm -rf build && go build ./...",
		"echo hi",
		"cd /tmp",
		"go test './...",
		"&& go test ./...",
		"go test ./... &&",
	}
	for _, c := range wrap {
		if got, ok := WrapCommand(c); !ok || got != "kran run --sh "+ShellQuote(c) {
			t.Errorf("should wrap %q: ok=%v got=%q", c, ok, got)
		}
	}
	for _, c := range keep {
		if got, ok := WrapCommand(c); ok {
			t.Errorf("must not wrap %q (got %q)", c, got)
		}
	}
}

func TestWrapWithPath(t *testing.T) {
	got, ok := wrapWith("/home/me/.local/bin/kran", "go test ./...")
	if !ok || got != "/home/me/.local/bin/kran run --sh 'go test ./...'" {
		t.Fatal(got)
	}
	got, _ = wrapWith("/Users/a b/kran", "go test ./...")
	if got != "'/Users/a b/kran' run --sh 'go test ./...'" {
		t.Fatal(got)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":         `''`,
		"abc":      `'abc'`,
		"it's":     `'it'\''s'`,
		"'":        `''\'''`,
		"a b\"c$d": `'a b"c$d'`,
	} {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q)=%s want %s", in, got, want)
		}
	}
}

// The wrapped string must reach kran's --sh argument byte-for-byte.
func TestWrapRoundTripThroughSh(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "kran")
	out := filepath.Join(dir, "got")
	script := "#!/bin/sh\n[ \"$1\" = run ] && [ \"$2\" = --sh ] && [ $# -eq 3 ] || exit 9\nprintf %s \"$3\" > " + out + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmds := []string{
		"go test ./...",
		`go test -run 'TestA|TestB' ./... 2>&1 | grep "it's" | head -5`,
		"cd 'dir with space' && go test ./... # not reached\n",
		`GOFLAGS="-tags=a b" go test -run Test\$ ./...`,
		"go test ./... | grep \\\\ | tail -n 3",
		"go test ./... && echo 'ünïcödé ✓ !done!'",
		"go test ./...; echo \"a\nb\"",
		"echo $HOME && go test ./...",
		"go test ./...\n\necho x\n",
		"go test -run '^$' ./... ; echo '' ; echo \"''\"",
	}
	for _, c := range cmds {
		wrapped := quoteIfNeeded(fake) + " run --sh " + ShellQuote(c)
		_ = os.Remove(out)
		if b, err := exec.Command("sh", "-c", wrapped).CombinedOutput(); err != nil {
			t.Fatalf("%q: %v %s", c, err, b)
		}
		got, _ := os.ReadFile(out)
		if string(got) != c {
			t.Errorf("round trip mismatch:\n in: %q\nout: %q", c, got)
		}
	}
}

func TestIsOurCommand(t *testing.T) {
	yes := []string{"kran hook claude", "/usr/local/bin/kran hook claude", "'/Users/a b/.local/bin/kran' hook claude"}
	no := []string{"kran hook codex", "/usr/bin/other hook claude", "foo kran hook claude", "/a b/kran hook claude", "kranx hook claude"}
	for _, c := range yes {
		if !isOurCommand(c) {
			t.Errorf("should match %q", c)
		}
	}
	for _, c := range no {
		if isOurCommand(c) {
			t.Errorf("should not match %q", c)
		}
	}
}
