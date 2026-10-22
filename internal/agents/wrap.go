package agents

import (
	"path"
	"regexp"
	"strings"
)

// ShellQuote quotes s for POSIX sh using single quotes (' becomes '\”).
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safePathRe = regexp.MustCompile(`^[A-Za-z0-9_./@%+=:,-]+$`)

// quoteIfNeeded quotes a path only when a shell would otherwise split it.
func quoteIfNeeded(s string) string {
	if safePathRe.MatchString(s) {
		return s
	}
	return ShellQuote(s)
}

// WrapCommand returns `kran run --sh '<cmd>'` when cmd is a heavy command that
// is safe to move (see shouldWrap), else ok=false and the input is untouched.
func WrapCommand(cmd string) (string, bool) { return wrapWith("kran", cmd) }

func wrapWith(kranPath, cmd string) (string, bool) {
	if !shouldWrap(cmd) {
		return "", false
	}
	if kranPath == "" {
		kranPath = "kran"
	}
	return quoteIfNeeded(kranPath) + " run --sh " + ShellQuote(cmd), true
}

// ---------------------------------------------------------------------------
// Policy. Whitelist approach: the command must be made only of
//   * one or more heavy commands (never reading a pipe),
//   * `cd DIR`, `export X=y`, `true`, `echo …`,
//   * output filters (tail, head, grep, …) that read the heavy command's pipe,
// joined by && || ; | or newlines, with redirections only to /dev/null or
// fd duplication (2>&1). Anything else (git, ssh, sudo, servers, heredocs,
// backgrounding, subshells, command substitution) means "leave it alone":
// a wrongly wrapped command costs the user more than a missed one.

type shellCmd struct {
	words []string
	op    string // operator joining this command to the previous one ("" first)
}

func shouldWrap(cmd string) bool {
	if strings.TrimSpace(cmd) == "" || strings.ContainsRune(cmd, 0) {
		return false
	}
	// Command substitution would need real shell parsing; backticks likewise.
	if strings.Contains(cmd, "$(") || strings.Contains(cmd, "`") {
		return false
	}
	low := strings.ToLower(cmd)
	for _, bad := range []string{"localhost", "127.0.0.1", "0.0.0.0", "[::1]", "kran_no_hook"} {
		if strings.Contains(low, bad) {
			return false
		}
	}
	cmds, ok := lexShell(cmd)
	if !ok || len(cmds) == 0 {
		return false
	}
	heavy := 0
	for _, c := range cmds {
		words := stripPrefixes(c.words)
		if words == nil {
			return false
		}
		if len(words) == 0 { // bare assignments
			continue
		}
		switch {
		case isBenign(words):
		case c.op == "|" && isFilter(words):
		case isHeavy(words):
			if c.op == "|" { // would read the previous command's output
				return false
			}
			heavy++
		default:
			return false
		}
	}
	return heavy > 0
}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// stripPrefixes drops `VAR=x`, `time`, `nice [-n N]` and `env` prefixes.
// It returns nil when the command must not be wrapped (KRAN_NO_HOOK set).
func stripPrefixes(w []string) []string {
	for len(w) > 0 {
		switch {
		case assignRe.MatchString(w[0]):
			if strings.HasPrefix(w[0], "KRAN_NO_HOOK=") {
				return nil
			}
			w = w[1:]
		case w[0] == "time" || w[0] == "env":
			w = w[1:]
		case w[0] == "nice":
			w = w[1:]
			if len(w) >= 2 && w[0] == "-n" {
				w = w[2:]
			} else if len(w) >= 1 && regexp.MustCompile(`^-\d+$`).MatchString(w[0]) {
				w = w[1:]
			}
		default:
			return w
		}
	}
	return w
}

func isBenign(w []string) bool {
	switch w[0] {
	case "cd":
		return len(w) == 2
	case "export":
		for _, a := range w[1:] {
			if !assignRe.MatchString(a) || strings.HasPrefix(a, "KRAN_NO_HOOK=") {
				return false
			}
		}
		return true
	case "true", ":", "echo":
		return true
	}
	return false
}

func isFilter(w []string) bool {
	args := w[1:]
	switch path.Base(w[0]) {
	case "tail":
		for _, a := range args {
			if strings.HasPrefix(a, "--follow") {
				return false
			}
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "fF") {
				return false
			}
		}
		return true
	case "head", "grep", "egrep", "fgrep", "wc", "sort", "uniq", "cut", "tr", "cat":
		return true
	case "tee":
		for _, a := range args {
			if a != "/dev/null" {
				return false
			}
		}
		return true
	}
	return false
}

// isHeavy reports whether w (prefixes already stripped) is a heavy command.
func isHeavy(w []string) bool {
	if len(w) == 0 {
		return false
	}
	for _, a := range w[1:] {
		switch a {
		case "-w", "watch", "--ui", "--headed", "--debug", "--inspect", "--inspect-brk":
			return false
		}
		if strings.HasPrefix(a, "--watch") {
			return false
		}
	}
	name := path.Base(w[0])
	args := w[1:]
	switch {
	case name == "go":
		return goHeavy(args)
	case name == "cargo":
		return in(firstPositional(args, "+"), "build", "test", "check", "clippy", "nextest", "bench")
	case name == "make" || name == "gmake":
		return makeHeavy(args)
	case name == "docker":
		return dockerHeavy(args)
	case name == "gradle" || name == "gradlew":
		return anyIn(args, "build", "test", "check", "assemble", "integrationTest")
	case name == "mvn" || name == "mvnw":
		return anyIn(args, "package", "test", "verify", "install", "compile")
	case name == "dotnet" || name == "swift":
		return in(firstPositional(args), "build", "test")
	case name == "bazel" || name == "bazelisk":
		return in(firstPositional(args), "build", "test")
	case name == "cmake":
		return anyIn(args, "--build")
	case name == "pytest" || name == "py.test":
		return !anyIn(args, "--looponfail", "-f")
	case isPython(name):
		return len(args) >= 2 && args[0] == "-m" && (args[1] == "pytest")
	case name == "uv" || name == "poetry" || name == "pipenv":
		return runnerHeavy(args)
	case in(name, "vitest", "jest", "playwright", "tsc", "next"):
		return jsToolHeavy(name, args)
	case in(name, "npx", "bunx"):
		return jsRunnerHeavy(args)
	case in(name, "npm", "pnpm", "yarn", "bun"):
		return pmHeavy(name, args)
	}
	return false
}

func in(s string, set ...string) bool {
	for _, x := range set {
		if s == x {
			return true
		}
	}
	return false
}

func anyIn(args []string, set ...string) bool {
	for _, a := range args {
		if in(a, set...) {
			return true
		}
	}
	return false
}

func isPython(name string) bool {
	return regexp.MustCompile(`^python[0-9.]*$`).MatchString(name)
}

// firstPositional returns the first argument not starting with "-" (or with
// any of the extra prefixes).
func firstPositional(args []string, extraSkip ...string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		skip := false
		for _, p := range extraSkip {
			if strings.HasPrefix(a, p) {
				skip = true
			}
		}
		if !skip {
			return a
		}
	}
	return ""
}

func goHeavy(args []string) bool {
	sub := firstPositional(args)
	if !in(sub, "test", "build", "vet") {
		return false
	}
	for i, a := range args {
		if a == "-o" || strings.HasPrefix(a, "-o=") {
			// A build artifact would stay on the machine that built it; only
			// `-o /dev/null` (compile check) is safe.
			if a == "-o" && i+1 < len(args) && args[i+1] == "/dev/null" {
				continue
			}
			return false
		}
	}
	return true
}

func makeHeavy(args []string) bool {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if in(a, "-C", "-f", "-j", "-I", "-o") {
			skipNext = a != "-j"
			continue
		}
		if strings.HasPrefix(a, "-") || strings.Contains(a, "=") {
			continue
		}
		if a == "test" || a == "tests" || a == "build" || a == "all" || a == "check" ||
			strings.HasPrefix(a, "test-") || strings.HasPrefix(a, "test_") || strings.HasPrefix(a, "build-") {
			return true
		}
	}
	return false
}

func dockerHeavy(args []string) bool {
	sub := firstPositional(args)
	if sub == "buildx" {
		rest := dropThrough(args, "buildx")
		if firstPositional(rest) != "build" {
			return false
		}
		args = dropThrough(rest, "build")
	} else if sub == "build" {
		args = dropThrough(args, "build")
	} else {
		return false
	}
	for _, a := range args {
		if in(a, "-o", "--load") || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--load") {
			return false // result must land on this machine
		}
	}
	return true
}

func dropThrough(args []string, word string) []string {
	for i, a := range args {
		if a == word {
			return args[i+1:]
		}
	}
	return nil
}

func runnerHeavy(args []string) bool {
	// uv run [flags] pytest | python -m pytest ; poetry run pytest ; pipenv run …
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		i++
	}
	if i >= len(args) || args[i] != "run" {
		return false
	}
	rest := args[i+1:]
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	return isHeavy(rest)
}

func jsToolHeavy(tool string, args []string) bool {
	switch tool {
	case "vitest", "jest":
		return true
	case "playwright":
		return firstPositional(args) == "test"
	case "tsc":
		return anyIn(args, "-b", "--build", "--noEmit")
	case "next":
		return firstPositional(args) == "build"
	}
	return false
}

// jsRunnerHeavy handles `npx [-y] TOOL …`, `pnpm dlx TOOL …`, etc.
func jsRunnerHeavy(args []string) bool {
	i := 0
	for i < len(args) && (strings.HasPrefix(args[i], "-") || args[i] == "--") {
		i++
	}
	if i >= len(args) {
		return false
	}
	return isHeavy(args[i:])
}

var valueFlags = []string{"--filter", "-F", "--prefix", "-C", "--cwd", "--workspace", "--dir"}

var heavyScript = regexp.MustCompile(`^(test|build|lint|typecheck|type-check|check|e2e|ci)([:_-][A-Za-z0-9:_-]*)?$`)
var serverScript = regexp.MustCompile(`(?i)(watch|dev|serve|start|preview)`)

func scriptHeavy(s string) bool { return heavyScript.MatchString(s) && !serverScript.MatchString(s) }

func pmHeavy(pm string, args []string) bool {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			if in(a, valueFlags...) {
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	if len(pos) == 0 {
		return false
	}
	cmd, rest := pos[0], pos[1:]
	switch cmd {
	case "exec", "dlx", "x":
		if len(rest) == 0 {
			return false
		}
		return isHeavy(rest)
	case "run", "run-script", "rum", "urn":
		if len(rest) == 0 {
			return false
		}
		return scriptHeavy(rest[0]) || jsToolName(rest[0]) && isHeavy(rest)
	case "test", "t", "tst":
		return true // npm test, pnpm test, yarn test, bun test
	}
	if pm == "npm" {
		return false // npm needs `run` for everything else
	}
	if jsToolName(cmd) {
		return isHeavy(pos)
	}
	return scriptHeavy(cmd)
}

func jsToolName(s string) bool { return in(s, "vitest", "jest", "playwright", "tsc", "next") }

// ---------------------------------------------------------------------------
// Minimal shell lexer: enough to split on operators and reject anything
// whose meaning we cannot see (heredocs, subshells, backgrounding, input
// redirects, output redirects other than /dev/null).

func lexShell(s string) ([]shellCmd, bool) {
	var (
		cmds      []shellCmd
		words     []string
		cur       strings.Builder
		inWord    bool
		pendingOp string
		unq       = true // current word has only unquoted characters so far
	)
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
		}
		cur.Reset()
		inWord, unq = false, true
	}
	sep := func(op string) bool {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, shellCmd{words: words, op: pendingOp})
			words = nil
			pendingOp = op
			return true
		}
		if (op == "\n" || op == ";") && (len(cmds) == 0 || pendingOp == "\n" || pendingOp == ";") {
			if len(cmds) > 0 {
				pendingOp = op
			}
			return op == "\n" || len(cmds) > 0 // a leading ";" is a syntax error
		}
		return false
	}
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\'':
			j := i + 1
			for j < len(rs) && rs[j] != '\'' {
				cur.WriteRune(rs[j])
				j++
			}
			if j >= len(rs) {
				return nil, false
			}
			inWord, unq = true, false
			i = j
		case '"':
			j := i + 1
			closed := false
			for j < len(rs) {
				c := rs[j]
				if c == '"' {
					closed = true
					break
				}
				if c == '\\' && j+1 < len(rs) {
					n := rs[j+1]
					if n == '"' || n == '\\' || n == '$' || n == '`' {
						cur.WriteRune(n)
						j += 2
						continue
					}
					if n == '\n' {
						j += 2
						continue
					}
				}
				cur.WriteRune(c)
				j++
			}
			if !closed {
				return nil, false
			}
			inWord, unq = true, false
			i = j
		case '\\':
			if i+1 >= len(rs) {
				return nil, false
			}
			i++
			if rs[i] == '\n' {
				continue
			}
			cur.WriteRune(rs[i])
			inWord, unq = true, false
		case ' ', '\t', '\r':
			endWord()
		case '\n':
			if !sep("\n") {
				return nil, false
			}
		case ';':
			if i+1 < len(rs) && rs[i+1] == ';' {
				return nil, false
			}
			if !sep(";") {
				return nil, false
			}
		case '|':
			op := "|"
			if i+1 < len(rs) && rs[i+1] == '|' {
				op = "||"
				i++
			} else if i+1 < len(rs) && rs[i+1] == '&' {
				return nil, false
			}
			if !sep(op) {
				return nil, false
			}
		case '&':
			if i+1 < len(rs) && rs[i+1] == '&' {
				i++
				if !sep("&&") {
					return nil, false
				}
			} else if i+1 < len(rs) && rs[i+1] == '>' && !inWord {
				j, ok := redirect(rs, i+2)
				if !ok {
					return nil, false
				}
				i = j
			} else {
				return nil, false // background job
			}
		case '>':
			if inWord && unq && isDigits(cur.String()) { // 2>… : the fd belongs to the redirect
				cur.Reset()
				inWord, unq = false, true
			}
			j, ok := redirect(rs, i+1)
			if !ok {
				return nil, false
			}
			i = j
		case '<', '(', ')':
			return nil, false
		case '#':
			if !inWord {
				return nil, false
			}
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	endWord()
	if len(words) > 0 {
		cmds = append(cmds, shellCmd{words: words, op: pendingOp})
	} else if pendingOp == "&&" || pendingOp == "||" || pendingOp == "|" {
		return nil, false
	}
	return cmds, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// redirect parses the part after '>' (at rs[i]) and returns the index of the
// last consumed rune. Only >/dev/null and fd duplication (>&1, 2>&1) pass.
func redirect(rs []rune, i int) (int, bool) {
	if i < len(rs) && rs[i] == '>' {
		i++
	}
	if i < len(rs) && rs[i] == '&' {
		i++
		j := i
		for j < len(rs) && (rs[j] >= '0' && rs[j] <= '9' || rs[j] == '-') {
			j++
		}
		if j == i {
			return 0, false
		}
		return j - 1, true
	}
	for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
		i++
	}
	j := i
	for j < len(rs) && !strings.ContainsRune(" \t\n;|&<>()'\"\\", rs[j]) {
		j++
	}
	if string(rs[i:j]) != "/dev/null" {
		return 0, false
	}
	return j - 1, true
}
