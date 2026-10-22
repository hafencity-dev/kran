package service

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type launchd struct {
	o      Options
	label  string
	domain string // cached: gui/<uid>, user/<uid> or system
	noted  bool   // the sudo explanation was printed
}

func launchdLabel(inst string) string {
	if inst == "" {
		return "dev.hafencity.kran"
	}
	return "dev.hafencity.kran." + inst
}

func (l *launchd) agentPlistPath() string {
	return filepath.Join(l.o.Home, "Library", "LaunchAgents", l.label+".plist")
}

func (l *launchd) systemPlistPath() string {
	return filepath.Join(l.o.SystemDir, l.label+".plist")
}

func (l *launchd) plistPath() string {
	if l.o.System {
		return l.systemPlistPath()
	}
	return l.agentPlistPath()
}

func (l *launchd) root() bool { return l.o.UID == 0 }

// writeVerb are the launchctl subcommands that change state; for the system
// domain they need root.
func (l *launchd) ctlWrite(ctx context.Context, args ...string) (string, error) {
	if !l.o.System || l.root() {
		return l.o.Runner.Run(ctx, "launchctl", args...)
	}
	l.explainSudo()
	return l.o.Runner.Run(ctx, "sudo", append([]string{"launchctl"}, args...)...)
}

// explainSudo says once, in one sentence, why a password may be asked.
func (l *launchd) explainSudo() {
	if l.noted || l.root() {
		return
	}
	l.noted = true
	l.o.Logf("administrator rights are needed once to manage a system-wide launch daemon (it lets kran run on this Mac without a GUI login); you may be asked for your password")
}

func xmlEsc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Unit renders the LaunchAgent plist.
//
// ProcessType Interactive: launchd applies CPU/IO throttling to Background
// jobs and children inherit it; kran supervises build and test jobs that must
// run at full speed, and the daemon itself must answer its peers while they
// load the machine (LowPriorityIO false keeps its I/O out of the throttled
// class too). A negative Nice needs root, so there is none. AbandonProcessGroup is the launchd counterpart of
// systemd KillMode=process: stopping or restarting the daemon must not kill
// running job supervisors.
func (l *launchd) Unit() string {
	var b strings.Builder
	str := func(k, v string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, xmlEsc(v)) }
	flag := func(k string, v bool) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<%t/>\n", k, v) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by kran service install (` + formatMarker + `); kran upgrade and kran doctor --fix rewrite it. -->
<plist version="1.0">
<dict>
`)
	str("Label", l.label)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>daemon</string>\n\t</array>\n", xmlEsc(l.o.KranPath))
	flag("RunAtLoad", true)
	flag("KeepAlive", true)
	fmt.Fprintf(&b, "\t<key>ThrottleInterval</key>\n\t<integer>5</integer>\n")
	str("ProcessType", "Interactive")
	flag("LowPriorityIO", false)
	flag("LowPriorityBackgroundIO", false)
	flag("AbandonProcessGroup", true)
	if l.o.System {
		str("UserName", l.o.User)
		str("GroupName", l.o.Group)
		flag("InitGroups", true)
	}
	str("WorkingDirectory", l.o.Home)
	str("StandardOutPath", l.o.LogPath)
	str("StandardErrorPath", l.o.LogPath)
	env := l.o.unitEnv()
	if l.o.System {
		// A daemon starts with an empty environment; give it the user's.
		extra := map[string]string{"HOME": l.o.Home, "USER": l.o.User, "LOGNAME": l.o.User}
		for _, e := range env {
			delete(extra, e.K)
		}
		for k, v := range extra {
			env = append(env, kv{k, v})
		}
		sort.Slice(env, func(i, j int) bool { return env[i].K < env[j].K })
	}
	if len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, e := range env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEsc(e.K), xmlEsc(e.V))
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// ctl runs a read-only launchctl query. For the system domain it retries with
// `sudo -n` (never prompting) because some macOS versions restrict `print`.
func (l *launchd) ctl(ctx context.Context, args ...string) (string, error) {
	out, err := l.o.Runner.Run(ctx, "launchctl", args...)
	if err != nil && l.o.System && !l.root() {
		if out2, err2 := l.o.Runner.Run(ctx, "sudo", append([]string{"-n", "launchctl"}, args...)...); err2 == nil {
			return out2, nil
		}
	}
	return out, err
}

// dom picks gui/<uid> when a GUI session exists, else user/<uid> (ssh-only).
func (l *launchd) dom(ctx context.Context) string {
	if l.o.System {
		return "system"
	}
	if l.domain == "" {
		uid := strconv.Itoa(l.o.UID)
		l.domain = "user/" + uid
		if _, err := l.ctl(ctx, "print", "gui/"+uid); err == nil {
			l.domain = "gui/" + uid
		}
	}
	return l.domain
}

func (l *launchd) target(ctx context.Context) string { return l.dom(ctx) + "/" + l.label }

func (l *launchd) loaded(ctx context.Context) bool {
	_, err := l.ctl(ctx, "print", l.target(ctx))
	return err == nil
}

// bootstrap loads the plist, falling back to legacy `load -w` when the
// domain refuses the modern call.
func (l *launchd) bootstrap(ctx context.Context) error {
	_, err := l.ctlWrite(ctx, "bootstrap", l.dom(ctx), l.plistPath())
	if err == nil {
		return nil
	}
	if _, err2 := l.ctlWrite(ctx, "load", "-w", l.plistPath()); err2 == nil {
		return nil
	}
	if l.o.System {
		return fmt.Errorf("could not load the launch daemon: %w", err)
	}
	return fmt.Errorf("could not load the LaunchAgent (there is no GUI login session on this Mac; run `kran service install --system` once, or just use kran: the daemon starts on demand): %w", err)
}

func (l *launchd) bootout(ctx context.Context) error {
	if !l.loaded(ctx) {
		return nil
	}
	if _, err := l.ctlWrite(ctx, "bootout", l.target(ctx)); err != nil {
		if _, err2 := l.ctlWrite(ctx, "unload", l.plistPath()); err2 != nil {
			return err
		}
	}
	return nil
}

func (l *launchd) Install(ctx context.Context) (bool, error) {
	var changed bool
	var err error
	if err := refuseRoot(l.o); err != nil {
		return false, err
	}
	if l.o.System {
		if changed, err = l.writeSystemPlist(ctx, l.Unit()); err != nil {
			return false, err
		}
		l.removeAgent(ctx)
	} else {
		if changed, err = writeIfChanged(l.plistPath(), l.Unit()); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(l.o.LogPath), 0o700); err != nil {
		return changed, err
	}
	if l.o.System {
		// launchd opens the log as root when it creates it; pre-create it so
		// the daemon (running as the user) can write to it.
		if f, err := os.OpenFile(l.o.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			f.Close()
		}
	}
	loaded := l.loaded(ctx)
	if loaded && changed {
		// A changed plist only takes effect after a reload.
		if err := l.bootout(ctx); err != nil {
			return changed, err
		}
		loaded = false
	}
	_, _ = l.ctlWrite(ctx, "enable", l.target(ctx)) // undo a previous `launchctl disable`
	if !loaded {
		if err := l.bootstrap(ctx); err != nil {
			return changed, err
		}
	}
	return changed, nil
}

func (l *launchd) Uninstall(ctx context.Context) error {
	if _, err := os.Stat(l.plistPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := l.bootout(ctx); err != nil {
		return err
	}
	if l.o.System && !l.root() {
		l.explainSudo()
		_, err := l.o.Runner.Run(ctx, "sudo", "rm", "-f", l.plistPath())
		return err
	}
	if err := os.Remove(l.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// writeSystemPlist installs the daemon plist as root:wheel 0644 (launchd
// rejects daemon plists that others can write). It stages the file in the
// user's temp dir and lets sudo move it into place.
func (l *launchd) writeSystemPlist(ctx context.Context, want string) (bool, error) {
	if cur, err := os.ReadFile(l.plistPath()); err == nil && string(cur) == want {
		return false, nil
	}
	if l.root() {
		return true, writeFileAtomic(l.plistPath(), []byte(want))
	}
	tmp, err := os.CreateTemp("", "kran-plist-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(want); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	l.explainSudo()
	if _, err := l.o.Runner.Run(ctx, "sudo", "install", "-m", "644", "-o", "root", "-g", "wheel", tmp.Name(), l.plistPath()); err != nil {
		return false, fmt.Errorf("could not install %s (needs sudo): %w", l.plistPath(), err)
	}
	return true, nil
}

// removeAgent retires a per-user LaunchAgent of the same label so two
// daemons never fight over the socket.
func (l *launchd) removeAgent(ctx context.Context) {
	a := &launchd{o: l.o, label: l.label}
	a.o.System = false
	if !fileExists(a.plistPath()) {
		return
	}
	_ = a.bootout(ctx)
	_ = os.Remove(a.plistPath())
}

// Plan lists what Install would do (for --dry-run); nothing is executed.
func (l *launchd) Plan() []string {
	if !l.o.System {
		return []string{"write " + l.plistPath(), "launchctl bootstrap " + l.dom0() + " " + l.plistPath()}
	}
	steps := []string{
		"sudo install -m 644 -o root -g wheel <rendered plist> " + l.plistPath(),
		"launchctl bootout of the per-user LaunchAgent " + l.agentPlistPath() + " (only if present)",
		"touch " + l.o.LogPath,
		"sudo launchctl bootstrap system " + l.plistPath(),
		"sudo launchctl enable system/" + l.label,
	}
	return steps
}

func (l *launchd) dom0() string { return "gui/" + strconv.Itoa(l.o.UID) }

func (l *launchd) installed() error {
	if _, err := os.Stat(l.plistPath()); err != nil {
		return ErrNotInstalled
	}
	return nil
}

func (l *launchd) Start(ctx context.Context) error {
	if err := l.installed(); err != nil {
		return err
	}
	if !l.loaded(ctx) {
		return l.bootstrap(ctx)
	}
	_, err := l.ctlWrite(ctx, "kickstart", l.target(ctx))
	return err
}

// Stop unloads the agent: with KeepAlive a plain kill would be undone at once.
func (l *launchd) Stop(ctx context.Context) error {
	if err := l.installed(); err != nil {
		return err
	}
	return l.bootout(ctx)
}

func (l *launchd) Restart(ctx context.Context, ro RestartOptions) error {
	if err := l.installed(); err != nil {
		return err
	}
	if err := restartWhenIdle(ctx, l.o, ro); err != nil {
		return err
	}
	// An upgrade restarts through here: bring the plist up to date first.
	// launchd reads a changed plist only when it loads it again.
	changed, err := l.Refresh(ctx)
	if err != nil {
		l.o.Logf("could not update %s: %v", l.plistPath(), err)
	}
	if !l.loaded(ctx) {
		return l.bootstrap(ctx)
	}
	if changed {
		if err := l.bootout(ctx); err != nil {
			return err
		}
		return l.bootstrap(ctx)
	}
	_, err = l.ctlWrite(ctx, "kickstart", "-k", l.target(ctx))
	return err
}

// Refresh rewrites the installed plist in the current format, keeping its
// binary and environment. It takes effect when launchd loads it again
// (Restart does that).
func (l *launchd) Refresh(ctx context.Context) (bool, error) {
	cur, err := os.ReadFile(l.plistPath())
	if err != nil {
		return false, ErrNotInstalled
	}
	exe, env := parsePlist(cur)
	if exe == "" {
		return false, nil // not a unit kran can read back: leave it alone
	}
	want := (&launchd{o: installedOptions(l.o, exe, env), label: l.label}).Unit()
	if string(cur) == want {
		return false, nil
	}
	if l.o.System {
		return l.writeSystemPlist(ctx, want)
	}
	return writeIfChanged(l.plistPath(), want)
}

// parsePlist returns ProgramArguments[0] and EnvironmentVariables of a
// plist kran wrote.
func parsePlist(b []byte) (exe string, env map[string]string) {
	env = map[string]string{}
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	dec.Strict = false
	var stack []string
	var envKey, top string
	first := true
	for {
		tok, err := dec.Token()
		if err != nil {
			return exe, env
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) == 0 {
				continue
			}
			text := string(t)
			switch depth, el := len(stack), stack[len(stack)-1]; {
			case depth == 3 && el == "key": // plist > dict > key
				top = text
			case depth == 4 && top == "ProgramArguments" && el == "string" && first:
				exe, first = text, false
			case depth == 4 && top == "EnvironmentVariables" && el == "key":
				envKey = text
			case depth == 4 && top == "EnvironmentVariables" && el == "string":
				env[envKey] = text
			}
		}
	}
}

var (
	stateRe = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	pidRe   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
)

func (l *launchd) Status(ctx context.Context) (Status, error) {
	st := Status{Manager: "launchd", Mode: "launchd-agent", UnitPath: l.plistPath(), LogPath: l.o.LogPath}
	if l.o.System {
		st.Mode = "launchd-system"
	} else {
		st.NoGUI = !HasGUISession(ctx, l.o)
	}
	cur, ferr := os.ReadFile(l.plistPath())
	st.Installed = ferr == nil
	st.Outdated = st.Installed && !strings.Contains(string(cur), formatMarker)
	out, err := l.ctl(ctx, "print", l.target(ctx))
	if err == nil {
		st.Enabled = true
		parseLaunchdPrint(out, &st)
	}
	switch {
	case !st.Installed && st.NoGUI:
		st.Advice = append(st.Advice, "No GUI login session on this Mac: kran starts its daemon on demand when a job or a linked machine connects. To keep it running permanently, run `kran service install --system`.")
	case !st.Installed:
		st.Advice = append(st.Advice, "Run `kran service install` to install the service.")
	case !st.Running:
		st.Advice = append(st.Advice, "Run `kran service start`; if it keeps failing see "+l.o.LogPath+".")
	}
	return st, nil
}

func parseLaunchdPrint(out string, st *Status) {
	if m := stateRe.FindStringSubmatch(out); m != nil {
		st.Running = m[1] == "running"
		st.Detail = m[1]
	}
	if m := pidRe.FindStringSubmatch(out); m != nil && st.Running {
		st.PID, _ = strconv.Atoi(m[1])
	}
}
