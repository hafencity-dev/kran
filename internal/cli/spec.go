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
