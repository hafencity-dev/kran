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
// Leading `cd DIR` commands stay outside: `cd DIR && kran run --sh '<rest>'`.
func WrapCommand(cmd string) (string, bool) { return wrapWith("kran", cmd) }

func wrapWith(kranPath, cmd string) (string, bool) {
	prefix, rest := hoistCd(cmd)
	if !shouldWrap(rest) {
		return "", false
	}
	if kranPath == "" {
		kranPath = "kran"
	}
	return prefix + quoteIfNeeded(kranPath) + " run --sh " + ShellQuote(rest), true
}

// hoistCd splits leading `cd DIR` commands off cmd so they run in the agent's
// shell. kran takes the workspace from the directory it starts in: wrapped
// whole, `cd ../other-repo && go test` would sync the shell's directory and
// then fail its cd on the other machine. The prefix keeps the original text
// and operators; rest is cmd when nothing is hoisted.
func hoistCd(cmd string) (prefix, rest string) {
	cmds, ok := lexShell(cmd)
	if !ok {
		return "", cmd
	}
	s, n := cmd, 0
	for ; n < len(cmds)-1; n++ {
		c, next := cmds[n], cmds[n+1].op
		if len(c.words) != 2 || c.words[0] != "cd" || (next != "&&" && next != ";" && next != "\n") {
			break
		}
		t := strings.TrimLeft(s, " \t\n")
		if !strings.HasPrefix(t, c.raw) {
			return "", cmd
		}
		t = strings.TrimLeft(t[len(c.raw):], " \t")
		if !strings.HasPrefix(t, next) {
			return "", cmd
		}
		s = t[len(next):]
	}
	if n == 0 {
		return "", cmd
	}
	prefix = strings.TrimLeft(cmd[:len(cmd)-len(s)], " \t\n")
	if !strings.HasSuffix(prefix, "\n") {
		prefix += " "
	}
	return prefix, strings.TrimLeft(s, " \t\n")
}

// ---------------------------------------------------------------------------
// Policy. Move what benefits and leave the rest: tests, checks, type checks,
// lints and builds that take long and whose result is the exit code and the
// output. Anything whose point is a file or image on THIS machine (go build
// of one package, -o, --load, coverage/junit files, --fix, snapshot updates)
// stays local, as do servers, watchers, installs and quick commands.
// Whitelist approach: the command must be made only of
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
	raw   string // the command's text as written (for permission rules)
}

func shouldWrap(cmd string) bool {
	if strings.TrimSpace(cmd) == "" || strings.ContainsRune(cmd, 0) {
		return false
	}
	// `make -j$(nproc)` is the one substitution agents write all the time, and
	// the remote shell should expand it for the machine that runs the job.
	cmd = strings.NewReplacer("$(nproc)", "8", "$(nproc --all)", "8").Replace(cmd)
	// Other command substitution would need real shell parsing; backticks likewise.
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

var durationRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[smhd]?$`)
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
		case w[0] == "timeout" && len(w) >= 3 && durationRe.MatchString(w[1]):
			w = w[2:]
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
		// Leading cds are hoisted (hoistCd); one later in the command must
		// stay inside the synced workspace, which an absolute or home path
		// or a variable may leave.
		return len(w) == 2 && !strings.HasPrefix(w[1], "/") && !strings.HasPrefix(w[1], "~") && !strings.Contains(w[1], "$")
	case "export":
		for _, a := range w[1:] {
			if !assignRe.MatchString(a) || strings.HasPrefix(a, "KRAN_NO_HOOK=") {
				return false
			}
		}
		return true
	case "true", ":", "echo":
		return true
	case "set": // set -e, set -euo pipefail
		for _, a := range w[1:] {
			if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") && a != "pipefail" && a != "errexit" && a != "nounset" {
				return false
			}
		}
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
		if staysLocalFlag(a) {
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
	case name == "ctest":
		return true
	case name == "golangci-lint":
		return firstPositional(args) == "run"
	case name == "turbo" || name == "nx":
		return turboHeavy(args)
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

// staysLocalFlag reports flags that mean "watch" or "write files here":
// the watcher must stay interactive, and --fix/-u/coverage files are wanted in
// this checkout, not on the machine that ran the command.
func staysLocalFlag(a string) bool {
	if strings.HasPrefix(a, "--watch") {
		// --watch=false and --watchAll=false switch watching off.
		return !strings.HasSuffix(a, "=false") && !strings.HasSuffix(a, "=0")
	}
	switch a {
	case "--fix", "--write", "-u", "-update", "--snapshot-update", "--junitxml", "--junit-xml", "--outputFile", "--output-file", "--html":
		return true
	}
	for _, p := range []string{"--fix=", "--write=", "--update", "--junitxml=", "--junit-xml=", "--outputFile=",
		"--output-file=", "-coverprofile", "-cpuprofile", "-memprofile", "-blockprofile", "-mutexprofile", "-trace",
		"-outputdir", "--cov-report=html", "--cov-report=xml", "--cov-report=json", "--cov-report=lcov", "--cov-report=annotate", "--html="} {
		if strings.HasPrefix(a, p) {
			return true
		}
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
	devNull := false
	for i, a := range args {
		if a == "-o" || strings.HasPrefix(a, "-o=") {
			// A build artifact would stay on the machine that built it; only
			// `-o /dev/null` (compile check) is safe.
			if a == "-o" && i+1 < len(args) && args[i+1] == "/dev/null" {
				devNull = true
				continue
			}
			return false
		}
	}
	if sub == "test" || devNull {
		return true
	}
	// build and vet are only worth moving over many packages: `go build .`
	// or `go build ./cmd/x` writes a binary here, `go vet ./pkg` is quick.
	for _, a := range args {
		if strings.Contains(a, "...") {
			return true
		}
	}
	return false
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
		if in(a, "test", "tests", "build", "all", "check", "lint", "ci", "typecheck", "e2e") ||
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
		if in(a, "-o", "--load", "--push") || strings.HasPrefix(a, "--output") || strings.HasPrefix(a, "--load") ||
			strings.HasPrefix(a, "--push") {
			return false // the result must land here, or the registry login lives here
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

// turboHeavy covers `turbo run build test`, `turbo test`, `nx run-many -t test`,
// `nx test web`: a task graph runner is heavy when it runs a heavy task.
func turboHeavy(args []string) bool {
	for _, a := range args {
		if a == "dev" || a == "serve" || a == "start" || a == "watch" {
			return false
		}
	}
	for _, a := range args {
		a = strings.TrimPrefix(strings.TrimPrefix(a, "--target="), "-t=")
		if scriptHeavy(a) {
			return true
		}
	}
	return false
}

func jsToolName(s string) bool { return in(s, "vitest", "jest", "playwright", "tsc", "next") }

// ---------------------------------------------------------------------------
// Minimal shell lexer: enough to split on operators and reject anything
// whose meaning we cannot see (heredocs, subshells, backgrounding, input
// redirects, output redirects other than /dev/null).

func lexShell(s string) ([]shellCmd, bool) {
	rs := []rune(s)
	var (
		cmds      []shellCmd
		words     []string
		cur       strings.Builder
		inWord    bool
		pendingOp string
		unq       = true // current word has only unquoted characters so far
		segStart  int    // rune index where the current command's text starts
		opAt      int    // rune index of the operator being handled
		newSeg    bool   // the next rune starts a new command's text
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
			cmds = append(cmds, shellCmd{words: words, op: pendingOp, raw: strings.TrimSpace(string(rs[segStart:opAt]))})
			words = nil
			pendingOp = op
			newSeg = true
			return true
		}
		newSeg = true
		if (op == "\n" || op == ";") && (len(cmds) == 0 || pendingOp == "\n" || pendingOp == ";") {
			if len(cmds) > 0 {
				pendingOp = op
			}
			return op == "\n" || len(cmds) > 0 // a leading ";" is a syntax error
		}
		return false
	}
	for i := 0; i < len(rs); i++ {
		if newSeg {
			segStart, newSeg = i, false
		}
		opAt = i
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
				if c == '$' && !plainDollar(rs, j, true) {
					return nil, false
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
		case ' ', '\t':
			endWord()
		case '\r': // a word character to the shell, a separator to a reader
			return nil, false
		case '$':
			if !plainDollar(rs, i, false) {
				return nil, false
			}
			cur.WriteRune(r)
			inWord = true
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
	if newSeg {
		segStart = len(rs)
	}
	if len(words) > 0 {
		cmds = append(cmds, shellCmd{words: words, op: pendingOp, raw: strings.TrimSpace(string(rs[segStart:]))})
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
	if j < len(rs) && strings.ContainsRune("'\"\\", rs[j]) { // >/dev/null'x' names another file
		return 0, false
	}
	return j - 1, true
}

var bracedName = regexp.MustCompile(`^\{[A-Za-z_][A-Za-z0-9_]*\}`)

// plainDollar reports whether the '$' at rs[i] is a literal or a plain
// parameter ($NAME, ${NAME}, $1, $?). Everything else — $'…', $"…", $[…],
// ${x@P}, ${(e)x}, ${a[i]} — can run code or quote in ways the lexer does
// not model, in bash or zsh.
func plainDollar(rs []rune, i int, inDQ bool) bool {
	if i+1 >= len(rs) {
		return true
	}
	n := rs[i+1]
	switch {
	case n == '_' || n >= 'A' && n <= 'Z' || n >= 'a' && n <= 'z' || n >= '0' && n <= '9':
		return true
	case strings.ContainsRune("?$!#@*-", n):
		return true
	case n == ' ' || n == '\t' || n == '\n':
		return true
	case n == '"':
		return inDQ
	case n == '{':
		return bracedName.MatchString(string(rs[i+1:]))
	}
	return false
}
