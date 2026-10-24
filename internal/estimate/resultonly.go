package estimate

import (
	"path"
	"regexp"
	"strings"

	"github.com/hafencity-dev/kran/internal/proto"
)

// resultOnly reports whether a command is a check whose whole result is its
// output and exit code: tests, vet, type checks and lints, alone or with
// cd/grep/tail around them. Files a remote run writes stay on that machine,
// so only such a command may be moved for reasons other than its own speed
// (SPEC §6.6, relieving a busy origin). It errs towards false: an unknown
// command, a redirection into a file, or a flag that writes (--fix, -u,
// -o, coverage and profile outputs) makes it a command whose files matter.
func resultOnly(spec *proto.JobSpec) bool {
	text := spec.Shell
	if text == "" && len(spec.Argv) >= 3 && isShell(spec.Argv[0]) && (spec.Argv[1] == "-c" || spec.Argv[1] == "-lc" || spec.Argv[1] == "-ec") {
		text = spec.Argv[2]
	}
	var segs [][]string
	if text != "" {
		text = escapedParen.Replace(text) // src/app/\(workspace\) is a path, not a subshell
		text = harmlessRedirect.ReplaceAllString(text, " ")
		if strings.ContainsAny(text, "><`") {
			return false // writes a file, reads one in, or runs a substitution
		}
		for _, s := range splitShell(text) {
			segs = append(segs, shellWords(s))
		}
	} else {
		segs = [][]string{spec.Argv}
	}
	checks := 0
	for _, w := range segs {
		w = unwrap(w)
		if len(w) == 0 {
			continue
		}
		switch commandKind(w) {
		case kindCheck:
			checks++
		case kindNeutral:
		default:
			return false
		}
	}
	return checks > 0
}

// harmlessRedirect matches redirections that write no file: stream merges
// and /dev/null.
var harmlessRedirect = regexp.MustCompile(`[0-9]?>&[0-9]|&>>?\s*/dev/null|[0-9]?>>?\s*/dev/null`)

var escapedParen = strings.NewReplacer(`\(`, "_", `\)`, "_")

type cmdKind int

const (
	kindOther cmdKind = iota
	kindCheck
	kindNeutral
)

// neutralCommands only read, print or move around.
var neutralCommands = map[string]bool{
	"cd": true, "pushd": true, "popd": true, "export": true, "set": true, "unset": true,
	"echo": true, "printf": true, "true": true, "false": true, ":": true, "test": true, "[": true, "[[": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "head": true, "tail": true, "wc": true,
	"sort": true, "uniq": true, "tr": true, "cut": true, "cat": true, "column": true, "jq": true,
	"sleep": true, "exit": true, "pwd": true, "which": true, "date": true, "ls": true, "id": true,
	"uname": true, "nproc": true, "sed": true, "awk": true,
}

func commandKind(w []string) cmdKind {
	base := path.Base(w[0])
	rest := w[1:]
	if neutralCommands[base] {
		if base == "sed" && anyFlag(rest, func(n string) bool { return n == "i" || strings.HasPrefix(n, "i") || n == "in-place" }) {
			return kindOther
		}
		return kindNeutral
	}
	if anyFlag(rest, writingFlag) {
		return kindOther
	}
	switch base {
	case "node", "bun", "deno":
		// node node_modules/.bin/vitest run …, node ./node_modules/typescript/bin/tsc …
		if script := skipFlags(rest, nil); len(script) > 0 {
			name := strings.TrimSuffix(strings.TrimSuffix(path.Base(script[0]), ".js"), ".mjs")
			if name != "node" && name != base {
				return commandKind(append([]string{name}, script[1:]...))
			}
		}
	case "go":
		sub, args := firstArg(skipFlags(rest, map[string]bool{"-C": true}))
		switch sub {
		case "test", "vet":
			return kindCheck
		case "list", "env", "version":
			return kindNeutral
		case "build":
			// Several packages or a pattern: compiled and discarded. One
			// main package would leave its binary behind.
			for _, a := range args {
				if strings.Contains(a, "...") {
					return kindCheck
				}
			}
		}
	case "golangci-lint", "staticcheck", "vitest", "jest", "pytest", "py.test", "svelte-check", "eslint", "oxlint":
		return kindCheck
	case "biome":
		if sub, _ := firstArg(rest); sub == "check" || sub == "lint" || sub == "ci" {
			return kindCheck
		}
	case "tsc", "vue-tsc", "tsgo":
		if anyFlag(rest, func(n string) bool { return n == "noemit" }) {
			return kindCheck // without --noEmit tsc writes .js files
		}
	case "cargo":
		if sub, _ := firstArg(rest); sub == "test" || sub == "check" || sub == "clippy" {
			return kindCheck
		}
	}
	if jsRunners[base] {
		sub, args := firstArg(skipFlags(rest, jsRunnerValueFlags))
		switch {
		case sub == "run" || sub == "run-script":
			sub, _ = firstArg(args)
			return scriptKind(sub)
		case sub == "exec" || sub == "dlx" || sub == "x":
			if len(args) > 0 {
				return commandKind(args)
			}
			return kindOther
		case (sub == "install" || sub == "i") && anyFlag(args, func(n string) bool { return n == "frozen-lockfile" }),
			sub == "ci":
			// Dependencies as the lockfile pins them: nothing the caller needs.
			return kindNeutral
		case jsRunnerBuiltins[sub]:
			return kindOther
		case sub == "test" || sub == "t":
			return kindCheck
		}
		if k := commandKind(append([]string{sub}, args...)); k == kindCheck {
			return k // pnpm vitest run
		}
		return scriptKind(sub)
	}
	return kindOther
}

// scriptKind: a package script runs whatever package.json says; test and
// type-check scripts are checks, lint scripts are not (they often --fix).
func scriptKind(name string) cmdKind {
	n := strings.ToLower(name)
	switch {
	case n == "":
		return kindOther
	case strings.HasPrefix(n, "test") || strings.HasSuffix(n, ":test"),
		strings.Contains(n, "typecheck") || strings.Contains(n, "type-check") ||
			strings.Contains(n, "types:check") || strings.Contains(n, "check-types"):
		return kindCheck
	}
	return kindOther
}

// writingFlag names flags that write files the caller may want: fixes,
// snapshot updates, outputs, coverage and profiles.
func writingFlag(n string) bool {
	switch n {
	case "o", "c", "u", "w", "fix", "write", "update", "update-snapshots", "updatesnapshot", "apply", "apply-unsafe",
		"out", "outdir", "out-dir", "output", "outputfile", "output-file", "outfile", "emit", "trace",
		"coverprofile", "cpuprofile", "memprofile", "blockprofile", "mutexprofile", "cover", "coverage", "coverpkg",
		"junitxml", "junit", "html", "json-report":
		return true
	}
	return strings.HasPrefix(n, "cov") || strings.HasSuffix(n, "profile")
}

// anyFlag reports whether a word is a flag whose name (no dashes, before
// "=", lower case) satisfies f. Words after "--" are arguments.
func anyFlag(w []string, f func(string) bool) bool {
	for _, a := range w {
		if a == "--" {
			return false
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		n := strings.ToLower(strings.TrimLeft(a, "-"))
		if i := strings.IndexByte(n, '='); i >= 0 {
			n = n[:i]
		}
		if f(n) {
			return true
		}
	}
	return false
}
