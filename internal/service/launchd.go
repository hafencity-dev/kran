package service

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type launchd struct {
	o      Options
	label  string
	domain string // cached: gui/<uid> or user/<uid>
}

func launchdLabel(inst string) string {
	if inst == "" {
		return "dev.hafencity.kran"
	}
	return "dev.hafencity.kran." + inst
}

func (l *launchd) plistPath() string {
	return filepath.Join(l.o.Home, "Library", "LaunchAgents", l.label+".plist")
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
// run at full speed. AbandonProcessGroup is the launchd counterpart of
// systemd KillMode=process: stopping or restarting the daemon must not kill
// running job supervisors.
func (l *launchd) Unit() string {
	var b strings.Builder
	str := func(k, v string) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<string>%s</string>\n", k, xmlEsc(v)) }
	flag := func(k string, v bool) { fmt.Fprintf(&b, "\t<key>%s</key>\n\t<%t/>\n", k, v) }
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	str("Label", l.label)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>daemon</string>\n\t</array>\n", xmlEsc(l.o.KranPath))
	flag("RunAtLoad", true)
	flag("KeepAlive", true)
	fmt.Fprintf(&b, "\t<key>ThrottleInterval</key>\n\t<integer>2</integer>\n")
	str("ProcessType", "Interactive")
	flag("AbandonProcessGroup", true)
	str("WorkingDirectory", l.o.Home)
	str("StandardOutPath", l.o.LogPath)
	str("StandardErrorPath", l.o.LogPath)
	if env := l.o.unitEnv(); len(env) > 0 {
		b.WriteString("\t<key>EnvironmentVariables</key>\n\t<dict>\n")
		for _, e := range env {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n\t\t<string>%s</string>\n", xmlEsc(e.K), xmlEsc(e.V))
		}
		b.WriteString("\t</dict>\n")
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func (l *launchd) ctl(ctx context.Context, args ...string) (string, error) {
	return l.o.Runner.Run(ctx, "launchctl", args...)
}

// dom picks gui/<uid> when a GUI session exists, else user/<uid> (ssh-only).
func (l *launchd) dom(ctx context.Context) string {
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
	_, err := l.ctl(ctx, "bootstrap", l.dom(ctx), l.plistPath())
	if err == nil {
		return nil
	}
	if _, err2 := l.ctl(ctx, "load", "-w", l.plistPath()); err2 == nil {
		return nil
	}
	return fmt.Errorf("could not load the LaunchAgent (is a user session active? log in on the console or run `kran daemon` directly): %w", err)
}

func (l *launchd) bootout(ctx context.Context) error {
	if !l.loaded(ctx) {
		return nil
	}
	if _, err := l.ctl(ctx, "bootout", l.target(ctx)); err != nil {
		if _, err2 := l.ctl(ctx, "unload", l.plistPath()); err2 != nil {
			return err
		}
	}
	return nil
}

func (l *launchd) Install(ctx context.Context) (bool, error) {
	changed, err := writeIfChanged(l.plistPath(), l.Unit())
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(l.o.LogPath), 0o700); err != nil {
		return changed, err
	}
	loaded := l.loaded(ctx)
	if loaded && changed {
		// A changed plist only takes effect after a reload.
		if err := l.bootout(ctx); err != nil {
			return changed, err
		}
		loaded = false
	}
	_, _ = l.ctl(ctx, "enable", l.target(ctx)) // undo a previous `launchctl disable`
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
	if err := os.Remove(l.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

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
	_, err := l.ctl(ctx, "kickstart", l.target(ctx))
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
	if !l.loaded(ctx) {
		return l.bootstrap(ctx)
	}
	_, err := l.ctl(ctx, "kickstart", "-k", l.target(ctx))
	return err
}

var (
	stateRe = regexp.MustCompile(`(?m)^\s*state = (\S+)`)
	pidRe   = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
)

func (l *launchd) Status(ctx context.Context) (Status, error) {
	st := Status{Manager: "launchd", UnitPath: l.plistPath(), LogPath: l.o.LogPath}
	_, ferr := os.Stat(l.plistPath())
	st.Installed = ferr == nil
	out, err := l.ctl(ctx, "print", l.target(ctx))
	if err == nil {
		st.Enabled = true
		parseLaunchdPrint(out, &st)
	}
	switch {
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
