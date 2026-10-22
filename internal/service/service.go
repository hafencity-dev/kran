// Package service installs and controls the kran daemon as a per-user
// service: a systemd user unit on Linux, a launchd LaunchAgent on macOS.
// No root is ever needed. Owner: E.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
)

// Runner runs an external command and returns its stdout. On failure the
// error includes the command's stderr so callers can show it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (stdout string, err error)
}

// Options configure a Manager. Zero values mean production defaults.
type Options struct {
	// KranPath is the absolute binary the service runs: `<KranPath> daemon`.
	KranPath string
	Home     string // default os.UserHomeDir()
	GOOS     string // default runtime.GOOS; tests render both flavors on one host
	UID      int    // default os.Getuid()
	// Instance isolates dev/test installs from the real service. When empty
	// and any of KRAN_CONFIG_DIR/KRAN_STATE_DIR/KRAN_RUNTIME_DIR is set, a
	// short stable id is derived from them (unit kran-<inst>.service, label
	// dev.hafencity.kran.<inst>).
	Instance string
	// Env is extra environment baked into the unit. The KRAN_*_DIR overrides
	// present in the environment and PATH are always included (user services
	// start with a minimal PATH, but jobs need go, node, docker); entries
	// here win.
	Env    map[string]string
	Runner Runner
	Logf   func(string, ...any)

	// LogPath is the file launchd captures stdout/stderr into (default
	// <state dir>/daemon.out.log). Unused on Linux (journald).
	LogPath string

	// Test seams.
	Getenv       func(string) string
	Now          func() time.Time
	Sleep        func(context.Context, time.Duration) error
	PollInterval time.Duration // idle polling during Restart (default 2s)
}

// Status describes the installed service.
type Status struct {
	Manager   string // "systemd" | "launchd" | "none"
	Installed bool
	Enabled   bool
	Running   bool
	PID       int
	UnitPath  string
	LogPath   string
	// Linger is the Linux `loginctl enable-linger` state; nil when unknown
	// or not applicable.
	Linger *bool
	// Advice holds plain sentences with the exact command to run next.
	Advice []string
	Detail string
}

// RestartOptions control Restart.
type RestartOptions struct {
	Force bool
	// Idle reports whether the daemon has no active jobs, and a human reason
	// when it has. Restart waits up to Wait (default 10 min) unless Force.
	// nil means restart immediately.
	Idle func(ctx context.Context) (idle bool, reason string, err error)
	Wait time.Duration
}

// Manager controls the service of one kran instance.
type Manager interface {
	// Install writes the unit, registers it, enables it and starts it. It is
	// idempotent: an unchanged unit is not rewritten (changed=false) but the
	// service is still enabled and running afterwards.
	Install(ctx context.Context) (changed bool, err error)
	// Uninstall stops and removes the unit; no-op when absent.
	Uninstall(ctx context.Context) error
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	// Restart waits for the daemon to be idle unless o.Force.
	Restart(ctx context.Context, o RestartOptions) error
	Status(ctx context.Context) (Status, error)
	// Unit returns the rendered unit/plist text.
	Unit() string
}

// ErrNotInstalled is returned by Start/Restart when no unit exists.
var ErrNotInstalled = errors.New("the kran service is not installed; run `kran service install`")

// New returns the Manager for opts.GOOS (linux: systemd, darwin: launchd).
func New(opts Options) (Manager, error) {
	o, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	switch o.GOOS {
	case "linux":
		return &systemd{o: o, unit: unitName(o.Instance)}, nil
	case "darwin":
		return &launchd{o: o, label: launchdLabel(o.Instance)}, nil
	}
	return nil, fmt.Errorf("service management is not supported on %s; run `kran daemon` yourself", o.GOOS)
}

func (o Options) normalize() (Options, error) {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return o, errors.New("cannot determine the home directory; set HOME")
		}
		o.Home = h
	}
	if o.UID == 0 && o.GOOS == runtime.GOOS {
		o.UID = os.Getuid()
	}
	if o.KranPath == "" {
		p, err := os.Executable()
		if err != nil {
			return o, fmt.Errorf("cannot find the kran binary: %w", err)
		}
		o.KranPath = p
	}
	if !filepath.IsAbs(o.KranPath) {
		abs, err := filepath.Abs(o.KranPath)
		if err != nil {
			return o, err
		}
		o.KranPath = abs
	}
	if o.Instance == "" {
		o.Instance = DeriveInstance(o.Getenv)
	}
	o.Instance = sanitizeInstance(o.Instance)
	if o.Runner == nil {
		o.Runner = &execRunner{uid: o.UID, getenv: o.Getenv}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	if o.PollInterval <= 0 {
		o.PollInterval = 2 * time.Second
	}
	if o.LogPath == "" {
		o.LogPath = filepath.Join(paths.StateDir(), "daemon.out.log")
	}
	return o, nil
}

var instRe = regexp.MustCompile(`[^a-z0-9-]+`)

func sanitizeInstance(s string) string {
	return strings.Trim(instRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

var dirEnvKeys = []string{"KRAN_CONFIG_DIR", "KRAN_STATE_DIR", "KRAN_RUNTIME_DIR", "KRAN_CACHE_DIR"}

// DeriveInstance returns "" for a standard environment, else a stable
// 8-hex id of the KRAN_*_DIR overrides, so dev installs never collide with
// the real service.
func DeriveInstance(getenv func(string) string) string {
	c, s, r := getenv("KRAN_CONFIG_DIR"), getenv("KRAN_STATE_DIR"), getenv("KRAN_RUNTIME_DIR")
	if c == "" && s == "" && r == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("config=" + c + "\nstate=" + s + "\nruntime=" + r + "\n"))
	return hex.EncodeToString(sum[:])[:8]
}

type kv struct{ K, V string }

// unitEnv is the sorted environment baked into the unit.
func (o Options) unitEnv() []kv {
	m := map[string]string{}
	if p := o.Getenv("PATH"); p != "" {
		m["PATH"] = p
	}
	for _, k := range dirEnvKeys {
		if v := o.Getenv(k); v != "" {
			m[k] = v
		}
	}
	for k, v := range o.Env {
		m[k] = v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]kv, 0, len(keys))
	for _, k := range keys {
		out = append(out, kv{k, m[k]})
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// writeFileAtomic writes via temp+rename, creating parents with 0755 (they
// are the user's systemd/launchd dirs, not kran's private ones).
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o644)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("write %s: %w", path, werr)
	}
	return nil
}

// writeIfChanged returns whether the file content changed.
func writeIfChanged(path, content string) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && string(cur) == content {
		return false, nil
	}
	return true, writeFileAtomic(path, []byte(content))
}

// restartWhenIdle implements the shared wait-for-idle policy.
func restartWhenIdle(ctx context.Context, o Options, ro RestartOptions) error {
	if ro.Force || ro.Idle == nil {
		return nil
	}
	wait := ro.Wait
	if wait <= 0 {
		wait = 10 * time.Minute
	}
	deadline := o.Now().Add(wait)
	announced := false
	for {
		idle, reason, err := ro.Idle(ctx)
		if err != nil || idle {
			// An unreachable daemon has nothing to protect.
			return nil
		}
		if !announced {
			o.Logf("waiting: %s (use --force to restart now)", reason)
			announced = true
		}
		if !o.Now().Add(o.PollInterval).Before(deadline) {
			return fmt.Errorf("the daemon is still busy after %s (%s); wait for the jobs, or use --force to restart now", wait, reason)
		}
		if err := o.Sleep(ctx, o.PollInterval); err != nil {
			return err
		}
	}
}
