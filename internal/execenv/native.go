package execenv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostEnv is the node's own environment for native jobs, captured once at
// daemon start: a launchd/systemd daemon has a minimal PATH, while the
// user's login shell knows where their toolchains live (SPEC §6.8).
type hostEnv struct {
	path  string
	home  string
	user  string
	shell string
	vars  map[string]string // cache locations set in the login shell
	tools map[string]string // toolchain key → version found on path
	mise  string            // absolute path of mise, "" if absent
}

// loginVars are read from the login shell besides PATH; when set there the
// node owner configured them deliberately and jobs should share them.
var loginVars = []string{"GOPATH", "GOCACHE", "GOMODCACHE", "npm_config_store_dir", "npm_config_cache", "PNPM_HOME", "CARGO_HOME", "RUSTUP_HOME", "MISE_DATA_DIR"}

func captureHostEnv(ctx context.Context) *hostEnv {
	e := &hostEnv{vars: map[string]string{}}
	e.home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		e.user = u.Username
		if e.home == "" {
			e.home = u.HomeDir
		}
	}
	e.shell = os.Getenv("SHELL")
	if e.shell == "" {
		e.shell = "/bin/sh"
	}
	vals := loginShellVars(ctx, e.shell, append([]string{"PATH"}, loginVars...))
	e.path = vals["PATH"]
	if e.path == "" {
		e.path = os.Getenv("PATH")
	}
	for _, k := range loginVars {
		if v := vals[k]; v != "" {
			e.vars[k] = v
		}
	}
	e.path = withWellKnownDirs(e.path, e.home)
	e.tools, e.mise = probeTools(ctx, e.path, e.home)
	return e
}

// loginShellVars runs `$SHELL -lc` once and extracts the named variables
// between markers, so banners printed by profile scripts do not leak in.
func loginShellVars(ctx context.Context, shell string, names []string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var script strings.Builder
	for _, n := range names {
		fmt.Fprintf(&script, `printf '\036KRAN:%s=%%s\037' "$%s";`, n, n)
	}
	cmd := exec.CommandContext(ctx, shell, "-lc", script.String())
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	out, _ := cmd.Output()
	vals := map[string]string{}
	for _, part := range bytes.Split(out, []byte{0x1e}) {
		s, ok := strings.CutPrefix(string(part), "KRAN:")
		if !ok {
			continue
		}
		s, _, _ = strings.Cut(s, "\x1f")
		if k, v, ok := strings.Cut(s, "="); ok {
			vals[k] = v
		}
	}
	return vals
}

// withWellKnownDirs appends common tool locations missing from path
// (lowest precedence) so a sparse login PATH still finds Homebrew, mise
// shims, Go and Cargo binaries.
func withWellKnownDirs(path, home string) string {
	have := map[string]bool{}
	parts := filepath.SplitList(path)
	for _, p := range parts {
		have[p] = true
	}
	cands := []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"}
	if runtime.GOOS == "darwin" {
		cands = append([]string{"/opt/homebrew/bin"}, cands...)
	}
	if home != "" {
		cands = append(cands,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, ".local", "share", "mise", "shims"),
			filepath.Join(home, "go", "bin"),
			filepath.Join(home, ".cargo", "bin"),
			filepath.Join(home, ".bun", "bin"))
	}
	cands = append(cands, "/usr/local/go/bin")
	for _, c := range cands {
		if have[c] {
			continue
		}
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			parts = append(parts, c)
			have[c] = true
		}
	}
	return strings.Join(parts, string(filepath.ListSeparator))
}

type toolProbe struct {
	key  string
	bin  string
	args []string
}

var toolProbes = []toolProbe{
	{"go", "go", []string{"env", "GOVERSION"}},
	{"node", "node", []string{"--version"}},
	{"python", "python3", []string{"--version"}},
	{"pnpm", "pnpm", []string{"--version"}},
	{"yarn", "yarn", []string{"--version"}},
	{"npm", "npm", []string{"--version"}},
	{"bun", "bun", []string{"--version"}},
	{"deno", "deno", []string{"--version"}},
	{"rust", "rustc", []string{"--version"}},
	{"uv", "uv", []string{"--version"}},
	{"mise", "mise", []string{"--version"}},
}

var probedTools = func() map[string]bool {
	m := map[string]bool{}
	for _, p := range toolProbes {
		m[p.key] = true
	}
	return m
}()

var versionRe = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func probeTools(ctx context.Context, path, home string) (map[string]string, string) {
	tools := map[string]string{}
	var mise string
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range toolProbes {
		bin, err := lookPathEnv(p.bin, path, "/")
		if err != nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, p.args...)
			cmd.Env = []string{"PATH=" + path, "HOME=" + home, "COREPACK_ENABLE_DOWNLOAD_PROMPT=0", "CI=1", "NO_COLOR=1"}
			cmd.Dir = os.TempDir()
			out, err := cmd.Output()
			if err != nil {
				return
			}
			v := versionRe.FindString(string(out))
			if v == "" {
				return
			}
			mu.Lock()
			tools[p.key] = v
			if p.key == "mise" {
				mise = bin
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return tools, mise
}

// ---------------------------------------------------------------------------
// Version pins

type semver [3]int

func parseVer(s string) (semver, int, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	s = strings.TrimPrefix(s, "go")
	var v semver
	parts := strings.Split(s, ".")
	n := 0
	for i := 0; i < len(parts) && i < 3; i++ {
		p := parts[i]
		if p == "x" || p == "X" || p == "*" {
			break
		}
		// "26rc1" → 26: pre-release suffixes do not matter for pin checks.
		j := 0
		for j < len(p) && p[j] >= '0' && p[j] <= '9' {
			j++
		}
		if j == 0 {
			return v, n, n > 0
		}
		x, _ := strconv.Atoi(p[:j])
		v[i] = x
		n++
		if j < len(p) {
			break
		}
	}
	return v, n, n > 0
}

func cmpVer(a, b semver) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// satisfies reports whether version have meets pin. Pins are exact or
// partial versions ("24", "24.8", "1.25.10"), npm-style ranges ("^1.2",
// "~1.2.3", ">=20 <23", "20.x", "a || b") or aliases ("lts/*", "latest").
func satisfies(have, pin string) bool {
	pin = strings.TrimSpace(pin)
	switch {
	case pin == "" || pin == "*" || pin == "latest" || pin == "stable" || pin == "system" || pin == "lts" || strings.HasPrefix(pin, "lts/") || pin == "node":
		return true
	}
	hv, _, ok := parseVer(have)
	if !ok {
		return false
	}
	for _, alt := range strings.Split(pin, "||") {
		all := true
		fields := strings.FieldsFunc(alt, func(r rune) bool { return r == ' ' || r == ',' })
		if len(fields) == 0 {
			continue
		}
		for _, c := range fields {
			if !satisfiesOne(hv, c) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

func satisfiesOne(hv semver, c string) bool {
	op := ""
	for _, p := range []string{">=", "<=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(c, p) {
			op, c = p, strings.TrimSpace(c[len(p):])
			break
		}
	}
	pv, n, ok := parseVer(c)
	if !ok {
		return false
	}
	switch op {
	case ">=":
		return cmpVer(hv, pv) >= 0
	case ">":
		return cmpVer(hv, pv) > 0
	case "<=":
		return cmpVer(hv, pv) <= 0
	case "<":
		return cmpVer(hv, pv) < 0
	case "^":
		if cmpVer(hv, pv) < 0 {
			return false
		}
		if pv[0] > 0 || n == 1 {
			return hv[0] == pv[0]
		}
		return hv[0] == 0 && hv[1] == pv[1]
	case "~":
		if cmpVer(hv, pv) < 0 {
			return false
		}
		if n == 1 {
			return hv[0] == pv[0]
		}
		return hv[0] == pv[0] && hv[1] == pv[1]
	default: // exact or partial: prefix match on given components
		for i := 0; i < n; i++ {
			if hv[i] != pv[i] {
				return false
			}
		}
		return true
	}
}

// miseVersion turns a pin into something `mise exec tool@…` accepts.
func miseVersion(pin string) string {
	pin = strings.TrimSpace(strings.Split(pin, "||")[0])
	if strings.HasPrefix(pin, "lts") {
		return "lts"
	}
	f := strings.Fields(pin)
	if len(f) > 0 {
		pin = f[0]
	}
	pin = strings.TrimLeft(pin, "^~>=<v")
	pin = strings.TrimSuffix(strings.TrimSuffix(pin, ".x"), ".x")
	if pin == "" || pin == "*" {
		return "latest"
	}
	return pin
}

// goToolchain returns the GOTOOLCHAIN value pinning pin ("1.25" → go1.25.0
// for Go ≥ 1.21, where toolchain names need the patch version); "" when
// the pin is not an exact version (then the local go is used as is).
func goToolchain(pin string) string {
	v, n, ok := parseVer(pin)
	if !ok || n < 2 || strings.ContainsAny(pin, "<>^~=* ") {
		return ""
	}
	if v[0] == 1 && v[1] < 21 {
		return "" // older Go cannot switch toolchains; use what is installed
	}
	if n == 2 {
		v[2] = 0
	}
	return fmt.Sprintf("go%d.%d.%d+auto", v[0], v[1], v[2])
}

// toolPlan is how a native job gets its pinned toolchains.
type toolPlan struct {
	env  map[string]string
	mise []string // tool@version for `mise exec`
	note string
}

// planToolchains resolves the workspace's pins against this node (SPEC
// §6.8). A pin that is neither satisfied nor installable makes the job
// ineligible here (the origin falls back).
func planToolchains(e *hostEnv, pins map[string]string, node string) (toolPlan, error) {
	tp := toolPlan{env: map[string]string{}}
	keys := make([]string, 0, len(pins))
	for k := range pins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var notes []string
	for _, tool := range keys {
		pin := pins[tool]
		have, found := e.tools[tool]
		switch {
		case tool == "go":
			if found {
				if tc := goToolchain(pin); tc != "" {
					tp.env["GOTOOLCHAIN"] = tc
				}
				continue
			}
		case tool == "mise" || tool == "uv":
			if found {
				continue
			}
		case found && satisfies(have, pin):
			continue
		case !found && !probedTools[tool] && e.mise == "":
			// A tool kran cannot probe: let the command find it (or fail
			// with its own message) rather than guessing.
			notes = append(notes, fmt.Sprintf("%s@%s not verified", tool, pin))
			continue
		}
		if e.mise != "" {
			tp.mise = append(tp.mise, tool+"@"+miseVersion(pin))
			continue
		}
		if tool == "pnpm" || tool == "yarn" || tool == "npm" {
			// Package managers are usually provided by corepack per project.
			notes = append(notes, fmt.Sprintf("%s@%s not installed (have %q)", tool, pin, have))
			continue
		}
		return tp, fmt.Errorf("%w: missing toolchain %s@%s on %s (install it there, or install mise so kran can)", ErrRuntimeUnavailable, tool, pin, node)
	}
	tp.note = strings.Join(notes, "; ")
	return tp, nil
}

// hostSpecificEnv reports variables the target sets itself (SPEC §6.5).
// The CLI already drops them; the target drops them again so a stale or
// foreign client cannot point a job at the origin's paths.
func hostSpecificEnv(k string) bool {
	switch k {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP", "DISPLAY",
		"BASH_ENV", "ENV", // shell startup files: paths on the origin
		"GOROOT", "GOPATH", "GOCACHE", "GOMODCACHE", "GOTOOLCHAIN", "PNPM_HOME", "npm_config_cache", "npm_config_store_dir":
		return true
	}
	for _, p := range []string{"XDG_", "SSH_", "WAYLAND_", "DBUS_", "TERM_PROGRAM", "NVM_", "LD_", "DYLD_"} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	if strings.HasSuffix(k, "_SOCK") || strings.HasSuffix(k, "_SOCKET") {
		return true
	}
	if strings.HasPrefix(k, "KRAN_") && k != "KRAN_OWNER" {
		return true
	}
	return false
}

// nativeEnv builds the complete environment of a native job.
func nativeEnv(e *hostEnv, req *StartRequest, node, slotRoot, tmp string, tp toolPlan) []string {
	env := map[string]string{}
	for k, v := range req.Spec.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || hostSpecificEnv(k) {
			continue
		}
		env[k] = v
	}
	env["PATH"] = e.path
	env["HOME"] = e.home
	env["USER"], env["LOGNAME"] = e.user, e.user
	env["SHELL"] = e.shell
	env["TMPDIR"] = tmp
	for k, v := range e.vars {
		env[k] = v
	}
	if _, ok := env["GOCACHE"]; !ok && e.home != "" {
		if d, err := os.UserCacheDir(); err == nil {
			env["GOCACHE"] = filepath.Join(d, "go-build")
		}
	}
	if _, ok := env["GOMODCACHE"]; !ok && e.home != "" {
		gopath := env["GOPATH"]
		if gopath == "" {
			gopath = filepath.Join(e.home, "go")
		}
		env["GOMODCACHE"] = filepath.Join(strings.Split(gopath, string(filepath.ListSeparator))[0], "pkg", "mod")
	}
	for k, v := range tp.env {
		env[k] = v
	}
	if len(tp.mise) > 0 {
		env["MISE_YES"] = "1"
		env["MISE_TRUSTED_CONFIG_PATHS"] = slotRoot
	}
	if req.Spec.TTY != nil && env["TERM"] == "" {
		env["TERM"] = "xterm-256color"
	}
	env["KRAN_JOB_ID"] = req.Spec.ID
	env["KRAN_NODE"] = node
	env["KRAN_ORIGIN"] = req.Origin
	env["KRAN_ORIGIN_ROOT"] = req.Spec.Workspace.Root
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

var shellNames = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "fish": true, "ksh": true}

// nativeArgv adapts the origin's argv to this node: an absolute shell path
// that does not exist here (zsh in /bin on macOS, /usr/bin on Linux) is
// looked up by name, and pinned toolchains missing here run via mise.
func nativeArgv(argv []string, path string, tp toolPlan, mise string) []string {
	out := append([]string(nil), argv...)
	if len(out) > 0 && filepath.IsAbs(out[0]) && shellNames[filepath.Base(out[0])] {
		if _, err := os.Stat(out[0]); err != nil {
			if p, err := lookPathEnv(filepath.Base(out[0]), path, "/"); err == nil {
				out[0] = p
			} else {
				out[0] = "/bin/sh"
			}
		}
	}
	if len(tp.mise) > 0 && mise != "" {
		pre := append([]string{mise, "exec"}, tp.mise...)
		out = append(append(pre, "--"), out...)
	}
	return out
}

// memCap derives the job's memory cap (SPEC §6.8): twice the admitted
// estimate, at least 2 GiB, at most what is free right now.
func memCap(alloc int64, avail int64) int64 {
	if alloc <= 0 {
		return 0
	}
	c := max(2*alloc, 2*gib)
	if avail > 0 && c > avail {
		c = max(avail, alloc)
	}
	return c
}
