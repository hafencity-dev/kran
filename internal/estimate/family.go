package estimate

import (
	"path"
	"strconv"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

const gib = int64(1) << 30

// familyDefault is what an unknown job of a family is assumed to need
// (SPEC §6.6). Duration is in origin seconds; it only steers placement
// (offload or not) until real runs are observed.
type familyDefault struct {
	CPUs     float64
	Mem      int64
	Duration float64
	// ScaleCPUs caps CPUs at the node's core count instead of the fixed
	// value (go test uses min(8, cores)).
	ScaleCPUs bool
}

var families = map[string]familyDefault{
	"go-test":      {CPUs: 8, Mem: 4 * gib, Duration: 60, ScaleCPUs: true},
	"go-build":     {CPUs: 4, Mem: 2 * gib, Duration: 30},
	"next-build":   {CPUs: 4, Mem: 6 * gib, Duration: 120},
	"vitest":       {CPUs: 4, Mem: 4 * gib, Duration: 60},
	"jest":         {CPUs: 4, Mem: 4 * gib, Duration: 60},
	"tsc":          {CPUs: 2, Mem: 3 * gib, Duration: 30},
	"pytest":       {CPUs: 2, Mem: 2 * gib, Duration: 60},
	"docker-build": {CPUs: 4, Mem: 4 * gib, Duration: 180},
	"cargo":        {CPUs: 8, Mem: 4 * gib, Duration: 120},
	// Extensions beyond the SPEC list, for common heavy commands that
	// would otherwise get the 1 CPU / 1 GiB default.
	"make":     {CPUs: 2, Mem: 2 * gib, Duration: 60},
	"js-test":  {CPUs: 4, Mem: 4 * gib, Duration: 60},
	"js-build": {CPUs: 4, Mem: 4 * gib, Duration: 90},
	"js-lint":  {CPUs: 2, Mem: 2 * gib, Duration: 30},
	"vllm":     {CPUs: 4, Mem: 24 * gib, Duration: 600},
	// Unknown commands: below the 5 s offload threshold, so they run
	// locally until a measured run says they are worth moving.
	"other": {CPUs: 1, Mem: 1 * gib, Duration: 3},
}

// familyRank orders families by weight; in a shell pipeline the heaviest
// command decides.
var familyRank = map[string]int{
	"other": 0, "js-lint": 1, "tsc": 2, "go-build": 3, "make": 4, "pytest": 5,
	"js-test": 6, "vitest": 6, "jest": 6, "go-test": 7, "js-build": 8,
	"next-build": 9, "cargo": 9, "docker-build": 10, "vllm": 11,
}

// Family returns the command family of argv, looking through wrappers
// (env, time, nice, npx, pnpm exec, uv run, python -m, sh -c, …).
func family(argv []string) string {
	if len(argv) >= 3 && isShell(argv[0]) && (argv[1] == "-c" || argv[1] == "-lc" || argv[1] == "-ec") {
		return shellFamily(argv[2])
	}
	return wordsFamily(argv)
}

func isShell(arg0 string) bool {
	switch path.Base(arg0) {
	case "sh", "bash", "zsh", "dash", "fish", "ksh":
		return true
	}
	return false
}

// shellFamily splits a shell string into simple commands and returns the
// heaviest family among them. Quoting is handled only as far as needed to
// find command words.
func shellFamily(s string) string {
	best := "other"
	for _, seg := range splitShell(s) {
		f := wordsFamily(shellWords(seg))
		if familyRank[f] > familyRank[best] {
			best = f
		}
	}
	return best
}

func splitShell(s string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
			cur.WriteByte(c)
		case c == ';' || c == '&' || c == '|' || c == '\n' || c == '(' || c == ')':
			segs = append(segs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	return append(segs, cur.String())
}

func shellWords(s string) []string {
	var words []string
	var cur strings.Builder
	var quote byte
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			in = true
		case c == ' ' || c == '\t':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}

// unwrap strips env assignments and wrapper commands, returning the words
// of the command that does the work.
func unwrap(w []string) []string {
	for len(w) > 0 {
		base := path.Base(w[0])
		switch {
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-"):
			w = w[1:] // FOO=bar cmd
		case base == "env" || base == "cross-env":
			w = skipFlags(w[1:], map[string]bool{"-u": true, "--unset": true, "-C": true, "--chdir": true})
		case base == "time" || base == "nohup" || base == "caffeinate" || base == "xvfb-run" || base == "chrt":
			w = skipFlags(w[1:], nil)
		case base == "nice":
			w = skipFlags(w[1:], map[string]bool{"-n": true, "--adjustment": true})
		case base == "ionice":
			w = skipFlags(w[1:], map[string]bool{"-c": true, "-n": true, "-p": true})
		case base == "timeout" || base == "gtimeout":
			w = skipFlags(w[1:], map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true})
			if len(w) > 0 {
				w = w[1:] // DURATION
			}
		case base == "npx" || base == "pnpx" || base == "bunx":
			w = skipFlags(w[1:], map[string]bool{"-p": true, "--package": true})
		case base == "dotenv" || base == "direnv" && len(w) > 2 && w[1] == "exec":
			if base == "direnv" {
				w = w[3:]
			} else {
				w = afterDoubleDash(w[1:])
			}
		case base == "mise" && len(w) > 1 && (w[1] == "exec" || w[1] == "x"):
			w = afterDoubleDash(w[2:])
		case (base == "uv" || base == "poetry" || base == "pipenv" || base == "pdm" || base == "rye" || base == "hatch") && len(w) > 1 && w[1] == "run":
			w = skipFlags(w[2:], map[string]bool{"--with": true, "--python": true, "-p": true, "--env-file": true})
		case base == "bundle" && len(w) > 1 && w[1] == "exec":
			w = w[2:]
		case strings.HasPrefix(base, "python") && len(w) > 2 && w[1] == "-m":
			w = w[2:]
		case base == "gotestsum":
			return append([]string{"go", "test"}, afterDoubleDash(w[1:])...)
		case base == "sudo" || base == "doas":
			w = skipFlags(w[1:], map[string]bool{"-u": true})
		default:
			return w
		}
	}
	return w
}

// skipFlags drops leading flags; withValue lists flags that take a separate value.
func skipFlags(w []string, withValue map[string]bool) []string {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") {
		if w[0] == "--" {
			return w[1:]
		}
		if withValue[w[0]] && len(w) > 1 {
			w = w[2:]
			continue
		}
		w = w[1:]
	}
	return w
}

func afterDoubleDash(w []string) []string {
	for i, a := range w {
		if a == "--" {
			return w[i+1:]
		}
	}
	return skipFlags(w, nil)
}

// firstArg returns the first non-flag word (the subcommand).
func firstArg(w []string) (string, []string) {
	for i, a := range w {
		if !strings.HasPrefix(a, "-") {
			return a, w[i+1:]
		}
	}
	return "", nil
}

var jsRunners = map[string]bool{"pnpm": true, "npm": true, "yarn": true, "bun": true}

// jsRunnerBuiltins are package-manager subcommands that are not scripts.
// jsRunnerValueFlags take a separate value (pnpm --filter web build).
var jsRunnerValueFlags = map[string]bool{
	"--filter": true, "-F": true, "-C": true, "--dir": true, "--prefix": true,
	"--cwd": true, "-w": true, "--workspace": true, "--loglevel": true,
}

var jsRunnerBuiltins = map[string]bool{
	"install": true, "i": true, "add": true, "remove": true, "ci": true, "update": true,
	"up": true, "link": true, "publish": true, "pack": true, "init": true, "create": true,
	"why": true, "list": true, "ls": true, "outdated": true, "audit": true, "store": true,
}

func wordsFamily(argv []string) string {
	w := unwrap(argv)
	if len(w) == 0 {
		return "other"
	}
	base := path.Base(w[0])
	rest := w[1:]
	switch base {
	case "go":
		sub, _ := firstArg(skipFlags(rest, map[string]bool{"-C": true}))
		switch sub {
		case "test":
			return "go-test"
		case "build", "vet", "install", "run", "generate":
			return "go-build"
		}
	case "golangci-lint", "staticcheck":
		return "go-build"
	case "next":
		if sub, _ := firstArg(rest); sub == "build" {
			return "next-build"
		}
	case "vitest":
		return "vitest"
	case "jest":
		return "jest"
	case "tsc", "vue-tsc", "tsgo", "svelte-check":
		return "tsc"
	case "pytest", "py.test":
		return "pytest"
	case "docker", "podman", "nerdctl":
		return dockerFamily(rest)
	case "docker-compose":
		if sub, _ := firstArg(rest); sub == "build" {
			return "docker-build"
		}
	case "buildah":
		if sub, _ := firstArg(rest); sub == "bud" || sub == "build" {
			return "docker-build"
		}
	case "cargo":
		for len(rest) > 0 && strings.HasPrefix(rest[0], "+") {
			rest = rest[1:] // +nightly toolchain selector
		}
		sub, _ := firstArg(rest)
		switch sub {
		case "build", "b", "test", "t", "check", "c", "clippy", "run", "r", "bench", "nextest", "doc", "install":
			return "cargo"
		}
	case "make", "gmake", "ninja":
		return "make"
	case "eslint", "biome", "oxlint", "prettier":
		return "js-lint"
	case "turbo", "nx":
		sub, args := firstArg(rest)
		if sub == "run" {
			sub, _ = firstArg(args)
		}
		return scriptFamily(sub)
	case "vllm":
		return "vllm"
	}
	if jsRunners[base] {
		sub, args := firstArg(skipFlags(rest, jsRunnerValueFlags))
		switch {
		case sub == "run" || sub == "run-script":
			sub, args = firstArg(args)
			return scriptFamily(sub)
		case sub == "exec" || sub == "dlx" || sub == "x":
			return wordsFamily(args)
		case jsRunnerBuiltins[sub]:
			return "other"
		case sub == "test" || sub == "t":
			return "js-test"
		}
		// `pnpm vitest run` runs a binary, `pnpm build` a script.
		if f := wordsFamily(append([]string{sub}, args...)); f != "other" {
			return f
		}
		return scriptFamily(sub)
	}
	return "other"
}

func dockerFamily(rest []string) string {
	sub, args := firstArg(rest)
	switch sub {
	case "build":
		return "docker-build"
	case "buildx", "image", "builder":
		if s, _ := firstArg(args); s == "build" || s == "bake" {
			return "docker-build"
		}
	case "compose":
		if s, _ := firstArg(args); s == "build" {
			return "docker-build"
		}
	}
	return "other"
}

// scriptFamily guesses a family from a package.json script / task name.
func scriptFamily(name string) string {
	n := strings.ToLower(name)
	switch {
	case n == "":
		return "other"
	case strings.HasPrefix(n, "typecheck") || strings.HasPrefix(n, "type-check") ||
		strings.HasPrefix(n, "check-types") || strings.HasPrefix(n, "tsc"):
		return "tsc"
	case strings.HasPrefix(n, "test") || strings.HasPrefix(n, "e2e") || strings.HasSuffix(n, ":test"):
		return "js-test"
	case strings.HasPrefix(n, "build") || strings.HasSuffix(n, ":build"):
		return "js-build"
	case strings.HasPrefix(n, "lint"):
		return "js-lint"
	}
	return "other"
}

// makeJobs returns N from -jN / -j N / --jobs=N, or 0.
func makeJobs(argv []string) int {
	w := unwrap(argv)
	for i, a := range w {
		var v string
		switch {
		case a == "-j" || a == "--jobs":
			if i+1 < len(w) {
				v = w[i+1]
			}
		case strings.HasPrefix(a, "--jobs="):
			v = strings.TrimPrefix(a, "--jobs=")
		case strings.HasPrefix(a, "-j"):
			v = strings.TrimPrefix(a, "-j")
		}
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}

// Commands splits a job's command line into its simple commands, wrappers
// (env, timeout, nice, …) stripped: what each part actually runs.
func Commands(spec *proto.JobSpec) [][]string {
	text := spec.Shell
	if text == "" && len(spec.Argv) >= 3 && isShell(spec.Argv[0]) && (spec.Argv[1] == "-c" || spec.Argv[1] == "-lc" || spec.Argv[1] == "-ec") {
		text = spec.Argv[2]
	}
	if text == "" {
		if w := unwrap(spec.Argv); len(w) > 0 {
			return [][]string{w}
		}
		return nil
	}
	var out [][]string
	for _, seg := range splitShell(escapedParen.Replace(text)) {
		if w := unwrap(shellWords(seg)); len(w) > 0 {
			out = append(out, w)
		}
	}
	return out
}
