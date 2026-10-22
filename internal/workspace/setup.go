package workspace

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

func defaultSetup(root string, argv []string) *proto.SetupSpec {
	return SetupFor(func(rel string) bool {
		_, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}, argv)
}

// lockSetup is one auto-detected setup, in priority order per ecosystem.
type lockSetup struct {
	lock string
	run  string
	eco  string // "js" | "py" | "go"
}

var lockSetups = []lockSetup{
	{"pnpm-lock.yaml", "pnpm install --frozen-lockfile --prefer-offline", "js"},
	{"bun.lock", "bun install --frozen-lockfile", "js"},
	{"bun.lockb", "bun install --frozen-lockfile", "js"},
	{"yarn.lock", "yarn install --immutable", "js"},
	{"package-lock.json", "npm ci", "js"},
	{"uv.lock", "uv sync --frozen", "py"},
	{"poetry.lock", "poetry install", "py"},
	{"go.sum", "go mod download", "go"},
}

var jsTools = setOf("node", "npm", "npx", "pnpm", "pnpx", "yarn", "bun", "bunx", "vitest", "jest",
	"tsc", "next", "vite", "eslint", "prettier", "turbo", "nx", "playwright", "tsx", "ts-node",
	"webpack", "rollup", "esbuild", "mocha", "ava", "astro", "nuxt", "svelte-kit", "storybook",
	"lerna", "biome", "oxlint", "vue-tsc", "remix", "react-scripts", "c8", "nyc", "knip")

var pyTools = setOf("python", "python3", "pytest", "py.test", "mypy", "pyright", "ruff", "poetry",
	"tox", "nox", "django-admin", "coverage", "black", "isort", "flake8", "pylint", "alembic", "celery")

// wrappers are skipped when looking for the real program of a command.
var wrappers = setOf("env", "time", "nice", "nohup", "exec", "command", "caffeinate", "xvfb-run", "timeout")

var shells = setOf("sh", "bash", "zsh", "dash", "fish", "ksh")

func setOf(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// SetupFor picks the setup for argv given which files exist in the
// workspace (exists takes slash paths relative to the root). It returns nil
// when the command needs none or installs dependencies itself. The target
// uses it with the committed manifest; DefaultSetup with the origin's files.
func SetupFor(exists func(rel string) bool, argv []string) *proto.SetupSpec {
	cmds := commandWords(argv)
	if len(cmds) == 0 {
		return nil
	}
	needs := map[string]bool{}
	for _, words := range cmds {
		prog := words[0]
		sub := ""
		if len(words) > 1 {
			sub = words[1]
		}
		switch {
		case isInstall(prog, sub, words):
			// The command installs dependencies itself; a setup would
			// only duplicate the work (and could fail on purpose-made
			// lockfile changes).
			return nil
		case jsTools[prog]:
			needs["js"] = true
		case prog == "uv":
			// uv run / uv sync keep the venv in sync themselves.
		case pyTools[prog]:
			needs["py"] = true
		case prog == "go":
			needs["go"] = true
		}
	}
	for _, ls := range lockSetups {
		if needs[ls.eco] && exists(ls.lock) {
			return &proto.SetupSpec{Run: ls.run, Key: []string{ls.lock}}
		}
	}
	return nil
}

func isInstall(prog, sub string, words []string) bool {
	switch prog {
	case "npm", "pnpm", "bun":
		return sub == "install" || sub == "i" || sub == "ci" || sub == "add"
	case "yarn":
		return len(words) == 1 || sub == "install" || sub == "add"
	case "poetry":
		return sub == "install" || sub == "sync"
	case "go":
		return sub == "mod" && len(words) > 2 && words[2] == "download"
	}
	return false
}

// commandWords extracts the simple commands of argv. Shell invocations
// (`sh -c '…'`) are split on control operators so `cd web && pnpm test`
// yields [cd web] and [pnpm test]. Program names are reduced to basenames.
func commandWords(argv []string) [][]string {
	if len(argv) == 0 {
		return nil
	}
	if shells[filepath.Base(argv[0])] {
		for i := 1; i < len(argv)-1; i++ {
			if strings.HasPrefix(argv[i], "-") && strings.Contains(argv[i], "c") {
				return splitScript(argv[i+1])
			}
		}
	}
	if w := stripWrappers(argv); len(w) > 0 {
		return [][]string{w}
	}
	return nil
}

func splitScript(s string) [][]string {
	var out [][]string
	repl := strings.NewReplacer("&&", "\n", "||", "\n", ";", "\n", "|", "\n", "(", "\n", ")", "\n", "&", "\n")
	for _, seg := range strings.Split(repl.Replace(s), "\n") {
		if w := stripWrappers(strings.Fields(seg)); len(w) > 0 {
			out = append(out, w)
		}
	}
	return out
}

func stripWrappers(words []string) []string {
	for len(words) > 0 {
		w := words[0]
		switch {
		case strings.Contains(w, "=") && !strings.HasPrefix(w, "="):
			words = words[1:] // VAR=value prefix
		case wrappers[filepath.Base(w)]:
			words = words[1:]
			// skip the wrapper's own flags and numeric args (nice -n 10, timeout 30)
			for len(words) > 0 && (strings.HasPrefix(words[0], "-") || isDigits(strings.TrimRight(words[0], "smhd"))) {
				words = words[1:]
			}
		case filepath.Base(w) == "kran":
			// `kran run [flags] -- cmd`: the command follows "--".
			i := indexOf(words, "--")
			if i < 0 {
				return nil
			}
			words = words[i+1:]
		default:
			out := append([]string{filepath.Base(w)}, words[1:]...)
			return out
		}
	}
	return nil
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}
