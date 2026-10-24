package estimate

import (
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestResultOnly: checks whose result is their output and exit code are
// recognised in the shapes agents write them; anything that writes a file
// the caller may want, or that is not known, is not.
func TestResultOnly(t *testing.T) {
	for _, tc := range []struct {
		shell string
		argv  []string
		want  bool
	}{
		{argv: []string{"go", "test", "./..."}, want: true},
		{argv: []string{"nice", "go", "test", "-race", "-run", "TestX", "./internal/x/"}, want: true},
		{shell: `cd product/backend && GOFLAGS=-trimpath timeout 600 go test ./internal/http/ -run 'TestA|TestB' -count=1 2>&1 | tail -30`, want: true},
		{shell: `go vet ./... && go build ./...`, want: true},
		{shell: `cd product/web && pnpm install --frozen-lockfile >/dev/null 2>&1; pnpm vitest run src/app/\(workspace\)/chat 2>&1 | tail -5`, want: true},
		{shell: `node node_modules/eslint/bin/eslint.js -f json src`, want: true},
		{shell: `npx tsc -p tsconfig.typecheck.json --noEmit 2>&1 | head -20`, want: true},
		{shell: `pnpm types:check`, want: true},
		{shell: `pnpm test`, want: true},
		{argv: []string{"pytest", "-x", "tests/"}, want: true},
		{argv: []string{"cargo", "clippy"}, want: true},

		{argv: []string{"go", "build", "-o", "bin/lk", "./cmd/lk"}, want: false},
		{argv: []string{"go", "build", "./cmd/lk"}, want: false},
		{argv: []string{"go", "test", "-coverprofile=c.out", "./..."}, want: false},
		{argv: []string{"go", "test", "-c", "./internal/x"}, want: false},
		{argv: []string{"go", "test", "./internal/x", "-update"}, want: false},
		{shell: `go test ./... > test.log 2>&1`, want: false},
		{shell: `go test ./... | tee test.log`, want: false},
		{shell: `pnpm vitest run -u src/x`, want: false},
		{shell: `pnpm vitest run --coverage`, want: false},
		{shell: `eslint --fix src`, want: false},
		{shell: `prettier --write .`, want: false},
		{shell: `pnpm lint`, want: false},
		{shell: `pnpm install && pnpm vitest run`, want: false},
		{argv: []string{"tsc", "-p", "tsconfig.json"}, want: false},
		{shell: `sed -i 's/a/b/' x.go && go test ./...`, want: false},
		{shell: `make walls`, want: false},
		{shell: `bash .lkland-ship/remote-job.sh walls`, want: false},
		{shell: `../../bin/lk testdb run -- go test ./internal/http/`, want: false},
		{shell: `go test $(go list ./... | grep -v e2e)`, want: true},
		{shell: `go test $(make pkgs)`, want: false},
		{shell: `cd x && ls`, want: false}, // nothing checked: not a check either
	} {
		spec := &proto.JobSpec{Shell: tc.shell, Argv: tc.argv}
		if tc.shell != "" {
			spec.Argv = []string{"sh", "-c", tc.shell}
		}
		if got := resultOnly(spec); got != tc.want {
			t.Errorf("resultOnly(%q %q) = %v, want %v", tc.shell, tc.argv, got, tc.want)
		}
	}
}
