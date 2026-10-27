package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/config"
	"github.com/hafencity-dev/kran/internal/estimate"
	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// RunFlags are the parsed flags of `kran run`.
type RunFlags struct {
	Local, Remote, AnyOS, Stdin, Quiet, JSONEvents, NoFallback, GPU, NoDocker bool
	Node, Mem, Priority, Sh                                                   string
	CPUs                                                                      float64
	Labels, ExtraEnv                                                          []string
}

// SpecInput is everything BuildSpec needs, injectable for tests.
type SpecInput struct {
	Flags  RunFlags
	Argv   []string // command (empty when Flags.Sh is set)
	Cwd    string
	Env    []string
	Home   string
	TTY    *proto.TTY
	Owner  proto.Owner
	Now    time.Time
	PID    int
	Config *config.Config
	// Detect / project config are indirections so tests do not need git.
	Detect  func(ctx context.Context, cwd string) (proto.Workspace, error)
	Project func(root string) (*workspace.ProjectConfig, error)
	// GoVersion reports the local Go toolchain for dir ("1.26.2"); nil
	// uses `go env GOVERSION`.
	GoVersion func(ctx context.Context, dir string, env []string) string
	// Notice receives one-line messages for stderr (nil = dropped).
	Notice func(string)
}

var localhostRe = regexp.MustCompile(`(?i)(^|[^a-z0-9.-])(localhost|127\.0\.0\.1|0\.0\.0\.0|\[::1\])([^a-z0-9.-]|$)`)

// BuildSpec turns flags, project config and the environment into a JobSpec
// (SPEC §6.1 step 1).
func BuildSpec(ctx context.Context, in SpecInput) (proto.JobSpec, error) {
	f := in.Flags
	spec := proto.JobSpec{
		Cwd:         in.Cwd,
		Owner:       in.Owner,
		SubmittedAt: in.Now.UTC(),
		ClientPID:   in.PID,
		NoFallback:  f.NoFallback || f.Remote,
		Stdin:       f.Stdin,
		TTY:         in.TTY,
	}
	if f.Sh != "" {
		spec.Shell = f.Sh
		spec.Argv = []string{shellName(in.Env), "-c", f.Sh}
	} else {
		spec.Argv = append([]string(nil), in.Argv...)
	}
	if len(spec.Argv) == 0 {
		return spec, usagef("no command given; try `kran run -- go test ./...` or `kran run --sh 'make test'`")
	}

	detect := in.Detect
	if detect == nil {
		detect = workspace.Detect
	}
	ws, wsErr := detect(ctx, in.Cwd)
	if wsErr != nil {
		ws = proto.Workspace{Root: in.Cwd, Name: filepath.Base(in.Cwd)}
	}
	spec.Workspace = ws

	loadProject := in.Project
	if loadProject == nil {
		loadProject = workspace.LoadProjectConfig
	}
	pc, err := loadProject(ws.Root)
	if err != nil {
		return spec, fmt.Errorf("%w (fix .kran.toml or run with --local)", err)
	}
	if pc == nil {
		pc = &workspace.ProjectConfig{}
	}

	cmdline := commandLine(spec)
	rule := matchRule(pc.Rules, cmdline)

	req := proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferAuto}
	if in.Config != nil && in.Config.Submit.Prefer != "" {
		req.Prefer = in.Config.PreferMode()
	}
	if pc.OS == proto.OSAny {
		req.OS = proto.OSAny
	}
	var memStr string
	if rule != nil {
		if rule.OS != "" {
			req.OS = rule.OS
		}
		req.CPUs = rule.CPUs
		memStr = rule.Mem
		req.GPU = rule.GPU
		if rule.Local {
			req.OriginOnly, req.OriginOnlyReason = true, "a rule in .kran.toml keeps this command local"
		}
	}
	// Explicit flags beat rules.
	if f.AnyOS {
		req.OS = proto.OSAny
	}
	if f.GPU {
		req.GPU = true
	}
	if f.CPUs > 0 {
		req.CPUs = f.CPUs
	}
	if f.Mem != "" {
		memStr = f.Mem
	}
	if memStr != "" {
		n, err := config.ParseSize(memStr)
		if err != nil {
			return spec, usagef("--mem: %v", err)
		}
		req.Mem = n
	}
	switch {
	case f.Local && f.Remote:
		return spec, usagef("--local and --remote cannot be combined")
	case f.Local:
		req.Prefer = proto.PreferLocal
	case f.Remote:
		req.Prefer = proto.PreferRemote
	}
	if f.Node != "" {
		req.Node = f.Node
	}
	req.Toolchains = workspace.ToolchainsAt(ws.Root, spec.Cwd)
	if isGoCommand(spec.Argv, spec.Shell) {
		// Pin the exact toolchain this machine uses (go.mod only names a
		// minimum), so a remote run compiles with the identical Go.
		gv := in.GoVersion
		if gv == nil {
			gv = localGoVersion
		}
		if v := gv(ctx, in.Cwd, in.Env); v != "" {
			if req.Toolchains == nil {
				req.Toolchains = map[string]string{}
			}
			req.Toolchains["go"] = v
		}
	}
	req.NoDocker = f.NoDocker || pc.Docker != nil && !*pc.Docker
	if reason := originOnlyReason(spec, ws, wsErr, in.Home); reason != "" && !req.OriginOnly {
		req.OriginOnly, req.OriginOnlyReason = true, reason
	}
	if !req.OriginOnly && wsErr == nil && ws.Git {
		if reason := unsyncedProgram(ctx, spec, ws, pc.Sync.Include, in.Home, envValue(in.Env, "PATH")); reason != "" {
			req.OriginOnly, req.OriginOnlyReason = true, reason
		}
	}
	// submit.prefer = "remote" in config.toml is a default, not --remote: a
	// job that can only run here stays here instead of failing with 125.
	if req.OriginOnly && req.Prefer == proto.PreferRemote && !f.Remote {
		req.Prefer = proto.PreferAuto
	}
	spec.Req = req

	// Priority: flag, rule, tty default.
	prio := proto.Priority(f.Priority)
	if prio == "" && rule != nil {
		prio = rule.Priority
	}
	if prio == "" {
		prio = proto.PriorityNormal
		if in.TTY != nil {
			prio = proto.PriorityHigh
		}
	}
	switch prio {
	case proto.PriorityHigh, proto.PriorityNormal, proto.PriorityLow:
	default:
		return spec, usagef("--priority %q: use high, normal or low", prio)
	}
	spec.Priority = prio

	explicit := append(append([]string(nil), pc.Env.Forward...), f.ExtraEnv...)
	spec.Env = FilterEnv(in.Env, explicit)
	if in.Notice != nil {
		for _, name := range forwardedSecrets(in.Env, pc.Env.Forward, f.ExtraEnv) {
			in.Notice(".kran.toml forwards " + name + " to other machines")
		}
	}
	for k, v := range pc.Env.Set {
		spec.Env[k] = v
	}

	if len(f.Labels) > 0 {
		spec.Labels = map[string]string{}
		for _, l := range f.Labels {
			k, v, ok := strings.Cut(l, "=")
			if !ok || k == "" {
				return spec, usagef("--label %q: use key=value", l)
			}
			spec.Labels[k] = v
		}
	}
	if pc.Setup != nil && pc.Setup.Run != "" {
		spec.Setup = &proto.SetupSpec{Run: pc.Setup.Run, Key: pc.Setup.Key}
	} else if s := workspace.DefaultSetup(ws.Root, spec.Argv); s != nil {
		spec.Setup = s
	}
	spec.SyncInclude, spec.SyncExclude = pc.Sync.Include, pc.Sync.Exclude
	return spec, nil
}

func commandLine(spec proto.JobSpec) string {
	if spec.Shell != "" {
		return spec.Shell
	}
	return strings.Join(spec.Argv, " ")
}

// shellName picks the shell for --sh strings. Only the base name travels so a
// target with a different layout still finds it on PATH; anything but bash or
// zsh becomes sh because agents write POSIX.
func shellName(env []string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "SHELL="); ok {
			switch filepath.Base(v) {
			case "bash":
				return "bash"
			case "zsh":
				return "zsh"
			}
		}
	}
	return "sh"
}

// matchRule returns the first rule whose glob matches the command line.
func matchRule(rules []workspace.Rule, cmdline string) *workspace.Rule {
	for i := range rules {
		if globMatch(rules[i].Match, cmdline) {
			return &rules[i]
		}
	}
	return nil
}

// globMatch supports * (any run, including spaces and slashes) and ?.
func globMatch(pattern, s string) bool {
	p, t := []rune(pattern), []rune(s)
	pi, ti, star, mark := 0, 0, -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]) && p[pi] != '*':
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// originOnlyReason reports why a job cannot leave the origin ("" = it can).
func originOnlyReason(spec proto.JobSpec, ws proto.Workspace, wsErr error, home string) string {
	if wsErr != nil {
		return "could not detect the workspace (" + wsErr.Error() + ")"
	}
	root := filepath.Clean(ws.Root)
	switch {
	case root == "/" || (home != "" && root == filepath.Clean(home)):
		return "not inside a project (no git repository here)"
	case !ws.Git && (root == "/tmp" || root == "/var/tmp" || root == os.TempDir()):
		return "not inside a project (shared temp directory)"
	case strings.HasPrefix(ws.Rel, ".."):
		return "the working directory is outside the workspace"
	case home != "" && !ws.Git && strings.HasPrefix(filepath.Clean(home), root+string(filepath.Separator)):
		return "not inside a project (directory contains your home)"
	}
	text := spec.Shell
	if text == "" {
		text = strings.Join(spec.Argv, " ")
	}
	if localhostRe.MatchString(text) {
		return "the command refers to localhost, which only exists on this machine"
	}
	return ""
}

// unsyncedProgram names a workspace file the command runs that would not
// reach another node (ignored by git, like a locally built bin/lk), or "".
// Run there, the job would fail with "No such file". Programs after a
// build step (make, go build, a package script, …) are not checked: the
// command may build them where it runs. Directories the target maintains
// itself (node_modules, .venv, target) count as present.
func unsyncedProgram(ctx context.Context, spec proto.JobSpec, ws proto.Workspace, include []string, home, pathEnv string) string {
	dir := spec.Cwd
	for _, w := range estimate.Commands(&spec) {
		base := filepath.Base(w[0])
		if base == "cd" {
			if len(w) > 1 && !strings.ContainsAny(w[1], "$~`") {
				if filepath.IsAbs(w[1]) {
					dir = w[1]
				} else {
					dir = filepath.Join(dir, w[1])
				}
				if rel, err := filepath.Rel(ws.Root, dir); err == nil && strings.HasPrefix(rel, "..") {
					if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
						return "it changes into " + w[1] + ", outside the workspace, which other nodes do not have"
					}
				}
			}
			continue
		}
		if mayBuild(w) {
			return ""
		}
		if p := homeTool(w[0], home, pathEnv); p != "" {
			return fmt.Sprintf("it runs %s from %s, which other nodes do not have", w[0], p)
		}
		progs := []string{w[0]}
		if scriptRunners[base] || strings.HasPrefix(base, "python") {
			for _, a := range w[1:] {
				if !strings.HasPrefix(a, "-") {
					progs = append(progs, a)
					break
				}
			}
		}
		for _, p := range progs {
			if !strings.Contains(p, "/") || strings.ContainsAny(p, "$~`*?") {
				continue
			}
			abs := p
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(dir, p)
			}
			rel, err := filepath.Rel(ws.Root, abs)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue // outside the workspace: not ours to judge
			}
			if fi, err := os.Stat(abs); err != nil || fi.IsDir() {
				continue
			}
			if !workspace.Synced(ctx, ws.Root, filepath.ToSlash(rel), include) {
				return fmt.Sprintf("it runs %s, which git ignores, so it would not be on another node (build it in the command, or list it in sync.include in .kran.toml)", p)
			}
		}
	}
	return ""
}

// homeTool returns the directory a bare command name resolves to when that
// is a personal bin directory in home (~/.local/bin/lk, ~/go/bin/x): such
// tools exist only on this machine. Toolchains kran provides on a target
// (mise installs, go, node, python, …) are not.
func homeTool(name, home, pathEnv string) string {
	if home == "" || strings.Contains(name, "/") || providedTools[name] || strings.HasPrefix(name, "python") {
		return ""
	}
	personal := []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), filepath.Join(home, "go", "bin")}
	for _, d := range filepath.SplitList(pathEnv) {
		if d == "" {
			continue
		}
		fi, err := os.Stat(filepath.Join(d, name))
		if err != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
			continue
		}
		for _, p := range personal {
			if filepath.Clean(d) == p {
				return strings.Replace(p, home, "~", 1)
			}
		}
		return "" // the first match decides, as for the shell
	}
	return ""
}

// providedTools are commands a target has or kran installs there.
var providedTools = map[string]bool{
	"go": true, "gofmt": true, "node": true, "npm": true, "npx": true, "pnpm": true, "pnpx": true, "yarn": true,
	"corepack": true, "bun": true, "deno": true, "pip": true, "pip3": true, "uv": true, "uvx": true, "cargo": true,
	"rustc": true, "rustup": true, "java": true, "mise": true, "git": true, "make": true, "docker": true,
}

var scriptRunners = map[string]bool{"sh": true, "bash": true, "zsh": true, "node": true, "bun": true, "deno": true, "ruby": true, "perl": true}

// mayBuild reports whether a command may produce programs a later part of
// the same command line runs.
func mayBuild(w []string) bool {
	base := filepath.Base(w[0])
	switch base {
	case "make", "gmake", "ninja", "cmake", "bazel", "just", "task", "npm", "pnpm", "yarn", "bun", "npx", "pnpx", "cp", "mv", "ln", "install", "tar", "unzip", "curl", "wget":
		return true
	case "go", "cargo":
		for _, a := range w[1:] {
			if a == "build" || a == "install" || a == "generate" {
				return true
			}
		}
	}
	return false
}

// forwardedSecrets lists the variables that a repo's env.forward sends
// although the filter would drop them as secrets (and the user did not ask
// with --env): the repo's own file decides that, so the user hears of it.
func forwardedSecrets(environ, forward, extra []string) []string {
	byName := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			byName[k] = v
		}
	}
	var out []string
	for _, name := range forward {
		v, set := byName[name]
		if !set || slices.Contains(extra, name) || slices.Contains(out, name) {
			continue
		}
		if isSecretEnv(name) || urlCredRe.MatchString(v) {
			out = append(out, name)
		}
	}
	return out
}

// envValue is name's value in an environment list ("" when unset).
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], name+"="); ok {
			return v
		}
	}
	return ""
}
