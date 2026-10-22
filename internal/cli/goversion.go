package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// goCommands start the go toolchain (directly or as a thin wrapper), so
// their result depends on the exact Go version.
var goCommands = map[string]bool{"go": true, "gotestsum": true, "golangci-lint": true, "staticcheck": true, "ginkgo": true}

// isGoCommand reports whether the job runs a Go tool: argv[0], or any
// command word of a --sh string ("cd x && go test ./...").
func isGoCommand(argv []string, shell string) bool {
	if shell == "" {
		return len(argv) > 0 && goCommands[filepath.Base(argv[0])]
	}
	cmdStart := true
	for _, w := range strings.Fields(shell) {
		switch w {
		case "&&", "||", ";", "|", "(", "$(", "{":
			cmdStart = true
			continue
		}
		if cmdStart {
			if goCommands[filepath.Base(strings.TrimLeft(w, "($"))] {
				return true
			}
			// VAR=value prefixes and wrappers keep the next word a command.
			cmdStart = strings.Contains(w, "=") || w == "time" || w == "env" || w == "nice" || w == "exec"
		}
		if strings.HasSuffix(w, ";") || strings.HasSuffix(w, "&&") {
			cmdStart = true
		}
	}
	return false
}

var goVersionRe = regexp.MustCompile(`^go(\d+\.\d+(?:\.\d+)?(?:(?:rc|beta)\d+)?)$`)

// parseGoVersion turns `go env GOVERSION` output ("go1.26.2") into the
// version workspace pins use ("1.26.2"); devel builds yield "".
func parseGoVersion(out string) string {
	m := goVersionRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return ""
	}
	return m[1]
}

var goVersionCache struct {
	sync.Mutex
	byDir map[string]string
}

// localGoVersion is the Go toolchain a Go command in dir would use here:
// `go env GOVERSION` resolves go.mod/go.work toolchain lines and
// GOTOOLCHAIN exactly like the job would. Cached per process and dir; ""
// when go is not on PATH or does not answer within 3 s.
func localGoVersion(ctx context.Context, dir string, env []string) string {
	goVersionCache.Lock()
	defer goVersionCache.Unlock()
	if v, ok := goVersionCache.byDir[dir]; ok {
		return v
	}
	v := ""
	if bin, lerr := lookPath("go", env, dir); lerr == nil {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		cmd := exec.CommandContext(ctx, bin, "env", "GOVERSION")
		cmd.Dir, cmd.Env = dir, env
		cmd.WaitDelay = time.Second
		if out, err := cmd.Output(); err == nil {
			v = parseGoVersion(string(out))
		}
		cancel()
	}
	if goVersionCache.byDir == nil {
		goVersionCache.byDir = map[string]string{}
	}
	goVersionCache.byDir[dir] = v
	return v
}
