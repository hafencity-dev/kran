package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/hafencity-dev/kran/internal/paths"
)

type systemd struct {
	o    Options
	unit string // kran.service or kran-<inst>.service
}

func unitName(inst string) string {
	if inst == "" {
		return "kran.service"
	}
	return "kran-" + inst + ".service"
}

func (s *systemd) unitPath() string {
	return filepath.Join(s.o.Home, ".config", "systemd", "user", s.unit)
}

var safeExec = regexp.MustCompile(`^[A-Za-z0-9_./+:@,=-]+$`)

var sdEscape = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "\n", `\n`)

// sdQuote quotes a value for a systemd unit. In ExecStart `$` starts a
// variable, so it is doubled there.
func sdQuote(v string, exec bool) string {
	v = sdEscape.Replace(v)
	if exec {
		v = strings.ReplaceAll(v, "$", "$$")
	}
	return `"` + v + `"`
}

func (s *systemd) Unit() string {
	var b strings.Builder
	exe := s.o.KranPath
	if !safeExec.MatchString(exe) {
		exe = sdQuote(exe, true)
	}
	desc := "kran daemon"
	if s.o.Instance != "" {
		desc += " (" + s.o.Instance + ")"
	}
	b.WriteString("# Written by kran service install (" + formatMarker + "); kran upgrade and\n# kran doctor --fix rewrite it.\n")
	b.WriteString("[Unit]\nDescription=" + desc + "\n")
	b.WriteString("Documentation=https://github.com/hafencity-dev/kran\n\n")
	b.WriteString("[Service]\nType=simple\n")
	b.WriteString("ExecStart=" + exe + " daemon\n")
	b.WriteString("Restart=always\nRestartSec=2\n")
	b.WriteString("# Only the daemon is stopped on restart. Job supervisors and their jobs run\n")
	b.WriteString("# in their own transient scopes (kran.slice) and must survive daemon upgrades.\n")
	b.WriteString("KillMode=process\n")
	b.WriteString(priorityComment)
	b.WriteString("CPUWeight=1000\nIOWeight=1000\nIOSchedulingClass=best-effort\nIOSchedulingPriority=0\nMemoryLow=256M\n")
	for _, e := range s.o.unitEnv() {
		b.WriteString("Environment=" + sdQuote(e.K+"="+e.V, false) + "\n")
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

const priorityComment = `# The daemon keeps the links, leases and heartbeats of the jobs it manages
# alive, so it must stay responsive while they, or anything else, load the
# machine: CPU and I/O weight far above the default 100, best-effort I/O at
# the highest level, and its memory shielded from reclaim (where the parent
# cgroups pass protection down). Nice= below 0 and a lower OOMScoreAdjust=
# need privileges a user service does not have.
`

var sdUnescape = strings.NewReplacer(`\\`, `\`, `\"`, `"`, "%%", "%", `\n`, "\n")

// sdUnquote reverses sdQuote (a bare word is returned as is).
func sdUnquote(v string, exec bool) string {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v
	}
	v = v[1 : len(v)-1]
	if exec {
		v = strings.ReplaceAll(v, "$$", "$")
	}
	return sdUnescape.Replace(v)
}

// parseUnit returns the binary and the environment of a unit kran wrote.
func parseUnit(text string) (exe string, env map[string]string) {
	env = map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "ExecStart="):
			v := strings.TrimSuffix(strings.TrimPrefix(line, "ExecStart="), " daemon")
			exe = sdUnquote(v, true)
		case strings.HasPrefix(line, "Environment="):
			if k, val, ok := strings.Cut(sdUnquote(strings.TrimPrefix(line, "Environment="), false), "="); ok {
				env[k] = val
			}
		}
	}
	return exe, env
}

func (s *systemd) Refresh(ctx context.Context) (bool, error) {
	cur, err := os.ReadFile(s.unitPath())
	if err != nil {
		return false, ErrNotInstalled
	}
	exe, env := parseUnit(string(cur))
	if exe == "" {
		return false, nil // not a unit kran can read back: leave it alone
	}
	want := (&systemd{o: installedOptions(s.o, exe, env), unit: s.unit}).Unit()
	changed, err := writeIfChanged(s.unitPath(), want)
	if err != nil || !changed {
		return changed, err
	}
	_, err = s.ctl(ctx, "daemon-reload")
	return true, err
}

// ctl runs `systemctl --user …` and turns a missing user bus into advice.
func (s *systemd) ctl(ctx context.Context, args ...string) (string, error) {
	out, err := s.o.Runner.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil && strings.Contains(err.Error(), "Failed to connect to bus") {
		return out, fmt.Errorf("cannot reach the systemd user manager (no user session bus): run `loginctl enable-linger $USER`, then `export XDG_RUNTIME_DIR=/run/user/$(id -u)` and retry: %w", err)
	}
	return out, err
}

func (s *systemd) Install(ctx context.Context) (bool, error) {
	if err := refuseRoot(s.o); err != nil {
		return false, err
	}
	changed, err := writeIfChanged(s.unitPath(), s.Unit())
	if err != nil {
		return false, err
	}
	if changed {
		if _, err := s.ctl(ctx, "daemon-reload"); err != nil {
			return changed, err
		}
	}
	if _, err := s.ctl(ctx, "enable", "--now", s.unit); err != nil {
		return changed, err
	}
	return changed, nil
}

func (s *systemd) Uninstall(ctx context.Context) error {
	if _, err := os.Stat(s.unitPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := s.ctl(ctx, "disable", "--now", s.unit); err != nil {
		return err
	}
	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err := s.ctl(ctx, "daemon-reload")
	return err
}

func (s *systemd) installed() error {
	if _, err := os.Stat(s.unitPath()); err != nil {
		return ErrNotInstalled
	}
	return nil
}

func (s *systemd) Start(ctx context.Context) error {
	if err := s.installed(); err != nil {
		return err
	}
	_, err := s.ctl(ctx, "start", s.unit)
	return err
}

func (s *systemd) Stop(ctx context.Context) error {
	if err := s.installed(); err != nil {
		return err
	}
	_, err := s.ctl(ctx, "stop", s.unit)
	return err
}

func (s *systemd) Restart(ctx context.Context, ro RestartOptions) error {
	if err := s.installed(); err != nil {
		return err
	}
	if err := restartWhenIdle(ctx, s.o, ro); err != nil {
		return err
	}
	// An upgrade restarts through here: bring the unit up to date first.
	if _, err := s.Refresh(ctx); err != nil {
		s.o.Logf("could not update %s: %v", s.unitPath(), err)
	}
	_, err := s.ctl(ctx, "restart", s.unit)
	return err
}

func (s *systemd) Status(ctx context.Context) (Status, error) {
	st := Status{Manager: "systemd", Mode: "systemd", UnitPath: s.unitPath(), LogPath: paths.LogFile()}
	cur, ferr := os.ReadFile(s.unitPath())
	st.Installed = ferr == nil
	st.Outdated = st.Installed && !strings.Contains(string(cur), formatMarker)
	out, err := s.ctl(ctx, "show", s.unit, "-p", "ActiveState,SubState,MainPID,UnitFileState,LoadState")
	if err != nil {
		if st.Installed {
			st.Detail = err.Error()
			st.Advice = append(st.Advice, "Start the user session manager: run `loginctl enable-linger $USER`, then retry.")
		}
		return st, nil
	}
	p := parseShow(out)
	st.Enabled = strings.HasPrefix(p["UnitFileState"], "enabled")
	st.Running = p["ActiveState"] == "active" && p["SubState"] == "running"
	st.PID, _ = strconv.Atoi(p["MainPID"])
	if !st.Running {
		st.PID = 0
	}
	if p["LoadState"] == "loaded" {
		st.Installed = true
	}
	st.Detail = p["ActiveState"] + " (" + p["SubState"] + ")"
	if lg := s.linger(ctx); lg != nil {
		st.Linger = lg
		if !*lg {
			st.Advice = append(st.Advice, "Run `loginctl enable-linger $USER` so kran keeps running after you log out.")
		}
	}
	switch {
	case !st.Installed:
		st.Advice = append(st.Advice, "Run `kran service install` to install the service.")
	case !st.Running:
		st.Advice = append(st.Advice, "Run `kran service start`; if it keeps failing see `journalctl --user -u "+strings.TrimSuffix(s.unit, ".service")+" -e`.")
	}
	return st, nil
}

func (s *systemd) linger(ctx context.Context) *bool {
	out, err := s.o.Runner.Run(ctx, "loginctl", "show-user", strconv.Itoa(s.o.UID), "-p", "Linger")
	if err != nil {
		return nil
	}
	v := parseShow(out)["Linger"]
	if v != "yes" && v != "no" {
		return nil
	}
	b := v == "yes"
	return &b
}

func parseShow(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

// EnableLinger runs `loginctl enable-linger` for the current user so the
// service survives logout. It may need privileges on some systems; the
// returned error then carries loginctl's message.
func EnableLinger(ctx context.Context, opts Options) error {
	o, err := opts.normalize()
	if err != nil {
		return err
	}
	_, err = o.Runner.Run(ctx, "loginctl", "enable-linger", strconv.Itoa(o.UID))
	if err != nil {
		return fmt.Errorf("could not enable lingering (try `sudo loginctl enable-linger $USER`): %w", err)
	}
	return nil
}
