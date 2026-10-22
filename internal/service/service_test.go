package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	calls []string
	reply func(cmd string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if f.reply != nil {
		return f.reply(cmd)
	}
	return "", nil
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func newMgr(t *testing.T, goos string, r Runner, env map[string]string, mod func(*Options)) (Manager, Options) {
	t.Helper()
	o := Options{
		KranPath: "/home/u/.local/bin/kran", Home: t.TempDir(), GOOS: goos, UID: 501, Runner: r,
		Getenv: envOf(env),
		// Never the real /Library/LaunchDaemons: on a Mac with kran
		// installed system-wide its plist would leak into every test.
		SystemDir: t.TempDir(),
	}
	o.LogPath = filepath.Join(o.Home, "state", "daemon.out.log")
	if mod != nil {
		mod(&o)
	}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return m, o
}

const wantUnit = `# Written by kran service install (kran-unit-format: 2); kran upgrade and
# kran doctor --fix rewrite it.
[Unit]
Description=kran daemon
Documentation=https://github.com/hafencity-dev/kran

[Service]
Type=simple
ExecStart=/home/u/.local/bin/kran daemon
Restart=always
RestartSec=2
# Only the daemon is stopped on restart. Job supervisors and their jobs run
# in their own transient scopes (kran.slice) and must survive daemon upgrades.
KillMode=process
# The daemon keeps the links, leases and heartbeats of the jobs it manages
# alive, so it must stay responsive while they, or anything else, load the
# machine: CPU and I/O weight far above the default 100, best-effort I/O at
# the highest level, and its memory shielded from reclaim (where the parent
# cgroups pass protection down). Nice= below 0 and a lower OOMScoreAdjust=
# need privileges a user service does not have.
CPUWeight=1000
IOWeight=1000
IOSchedulingClass=best-effort
IOSchedulingPriority=0
MemoryLow=256M
Environment="PATH=/usr/bin:/opt/go/bin"

[Install]
WantedBy=default.target
`

func TestSystemdUnitGolden(t *testing.T) {
	m, _ := newMgr(t, "linux", &fakeRunner{}, map[string]string{"PATH": "/usr/bin:/opt/go/bin"}, nil)
	if got := m.Unit(); got != wantUnit {
		t.Fatalf("unit mismatch:\n%s", got)
	}
}

func TestSystemdQuoting(t *testing.T) {
	m, _ := newMgr(t, "linux", &fakeRunner{}, nil, func(o *Options) {
		o.KranPath = `/home/a b/"x"/100%/$k/kran`
		o.Env = map[string]string{"A": `say "hi" 50%`}
	})
	u := m.Unit()
	for _, want := range []string{
		`ExecStart="/home/a b/\"x\"/100%%/$$k/kran" daemon`,
		`Environment="A=say \"hi\" 50%%"`,
	} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %s in\n%s", want, u)
		}
	}
}

const wantPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by kran service install (kran-unit-format: 2); kran upgrade and kran doctor --fix rewrite it. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.hafencity.kran</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/u/.local/bin/kran</string>
		<string>daemon</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>LowPriorityIO</key>
	<false/>
	<key>LowPriorityBackgroundIO</key>
	<false/>
	<key>AbandonProcessGroup</key>
	<true/>
	<key>WorkingDirectory</key>
	<string>/Users/u</string>
	<key>StandardOutPath</key>
	<string>/Users/u/Library/Application Support/kran/daemon.out.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/u/Library/Application Support/kran/daemon.out.log</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/local/bin:/usr/bin</string>
	</dict>
</dict>
</plist>
`

func TestLaunchdPlistGolden(t *testing.T) {
	m, _ := newMgr(t, "darwin", &fakeRunner{}, map[string]string{"PATH": "/usr/local/bin:/usr/bin"}, func(o *Options) {
		o.KranPath = "/Users/u/.local/bin/kran"
		o.Home = "/Users/u"
		o.LogPath = "/Users/u/Library/Application Support/kran/daemon.out.log"
	})
	if got := m.Unit(); got != wantPlist {
		t.Fatalf("plist mismatch:\n%s", got)
	}
}

func TestLaunchdEscaping(t *testing.T) {
	m, _ := newMgr(t, "darwin", &fakeRunner{}, nil, func(o *Options) {
		o.KranPath = `/Users/a&b/<x>/kran`
		o.Env = map[string]string{"Q": `a"b`}
	})
	u := m.Unit()
	if !strings.Contains(u, "/Users/a&amp;b/&lt;x&gt;/kran") || !strings.Contains(u, "a&#34;b") {
		t.Fatalf("not escaped:\n%s", u)
	}
}

func TestInstanceDerivation(t *testing.T) {
	if DeriveInstance(envOf(nil)) != "" {
		t.Fatal("standard env must have no instance")
	}
	e1 := map[string]string{"KRAN_CONFIG_DIR": "/tmp/a", "KRAN_STATE_DIR": "/tmp/b"}
	a, b := DeriveInstance(envOf(e1)), DeriveInstance(envOf(e1))
	if len(a) != 8 || a != b {
		t.Fatalf("unstable: %q %q", a, b)
	}
	if a == DeriveInstance(envOf(map[string]string{"KRAN_CONFIG_DIR": "/tmp/other"})) {
		t.Fatal("different dirs must differ")
	}
	m, _ := newMgr(t, "linux", &fakeRunner{}, e1, nil)
	u := m.Unit()
	if !strings.Contains(u, "Description=kran daemon ("+a+")") || !strings.Contains(u, `Environment="KRAN_CONFIG_DIR=/tmp/a"`) || !strings.Contains(u, `Environment="KRAN_STATE_DIR=/tmp/b"`) {
		t.Fatalf("instance env missing:\n%s", u)
	}
	s := m.(*systemd)
	if s.unit != "kran-"+a+".service" {
		t.Fatal(s.unit)
	}
	d, _ := newMgr(t, "darwin", &fakeRunner{}, e1, nil)
	if d.(*launchd).label != "dev.hafencity.kran."+a {
		t.Fatal(d.(*launchd).label)
	}
}

func TestSystemdInstallIdempotent(t *testing.T) {
	r := &fakeRunner{}
	m, o := newMgr(t, "linux", r, nil, nil)
	changed, err := m.Install(context.Background())
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	want := []string{"systemctl --user daemon-reload", "systemctl --user enable --now kran.service"}
	if strings.Join(r.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls: %v", r.calls)
	}
	path := filepath.Join(o.Home, ".config/systemd/user/kran.service")
	st1, err := os.Stat(path)
	if err != nil || st1.Mode().Perm() != 0o644 {
		t.Fatal(err, st1)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(path, old, old)
	r.calls = nil
	changed, err = m.Install(context.Background())
	if err != nil || changed {
		t.Fatal("second install must report unchanged", changed, err)
	}
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(old.Truncate(time.Second)) && st2.ModTime().After(old.Add(time.Second)) {
		t.Fatal("unit rewritten")
	}
	if strings.Join(r.calls, "|") != "systemctl --user enable --now kran.service" {
		t.Fatalf("calls: %v", r.calls)
	}
}

func TestSystemdUninstall(t *testing.T) {
	r := &fakeRunner{}
	m, o := newMgr(t, "linux", r, nil, nil)
	if err := m.Uninstall(context.Background()); err != nil || len(r.calls) != 0 {
		t.Fatalf("absent uninstall must be a no-op: %v %v", err, r.calls)
	}
	m.Install(context.Background())
	r.calls = nil
	if err := m.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.calls, "|") != "systemctl --user disable --now kran.service|systemctl --user daemon-reload" {
		t.Fatalf("calls: %v", r.calls)
	}
	if _, err := os.Stat(filepath.Join(o.Home, ".config/systemd/user/kran.service")); err == nil {
		t.Fatal("unit not removed")
	}
	if err := m.Start(context.Background()); !errors.Is(err, ErrNotInstalled) {
		t.Fatal(err)
	}
}

func TestSystemdBusError(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		return "", &RunError{Cmd: cmd, Err: errors.New("exit status 1"), Stderr: "Failed to connect to bus: No medium found"}
	}}
	m, _ := newMgr(t, "linux", r, nil, nil)
	_, err := m.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "enable-linger") || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
		t.Fatal(err)
	}
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return nil
}

func TestRestartWaitsForIdle(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			r := &fakeRunner{}
			clk := &fakeClock{now: time.Unix(0, 0)}
			var logs []string
			m, _ := newMgr(t, goos, r, nil, func(o *Options) {
				o.Now, o.Sleep = clk.Now, clk.Sleep
				o.Logf = func(f string, a ...any) { logs = append(logs, f) }
			})
			if _, err := m.Install(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.calls = nil
			n := 0
			err := m.Restart(context.Background(), RestartOptions{Idle: func(context.Context) (bool, string, error) {
				n++
				return n >= 4, "2 jobs running", nil
			}})
			if err != nil || n != 4 {
				t.Fatal(err, n)
			}
			if len(logs) != 1 {
				t.Fatalf("want exactly one waiting line, got %v", logs)
			}
			if clk.now.Sub(time.Unix(0, 0)) != 6*time.Second {
				t.Fatalf("slept %v", clk.now)
			}
			last := r.calls[len(r.calls)-1]
			if !strings.Contains(last, "restart kran.service") && !strings.Contains(last, "kickstart -k") {
				t.Fatalf("calls: %v", r.calls)
			}
		})
	}
}

func TestRestartForceTimeoutAndUnreachable(t *testing.T) {
	r := &fakeRunner{}
	clk := &fakeClock{now: time.Unix(0, 0)}
	m, _ := newMgr(t, "linux", r, nil, func(o *Options) { o.Now, o.Sleep = clk.Now, clk.Sleep })
	m.Install(context.Background())
	busy := func(context.Context) (bool, string, error) { return false, "1 job running", nil }

	r.calls = nil
	if err := m.Restart(context.Background(), RestartOptions{Force: true, Idle: busy}); err != nil || len(r.calls) != 1 {
		t.Fatal(err, r.calls)
	}
	r.calls = nil
	err := m.Restart(context.Background(), RestartOptions{Idle: busy, Wait: 10 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "--force") || len(r.calls) != 0 {
		t.Fatalf("want timeout error without restart: %v %v", err, r.calls)
	}
	down := func(context.Context) (bool, string, error) { return false, "", errors.New("connection refused") }
	if err := m.Restart(context.Background(), RestartOptions{Idle: down}); err != nil {
		t.Fatal(err)
	}
}

const showOut = `ActiveState=active
SubState=running
MainPID=4242
UnitFileState=enabled
LoadState=loaded
`

func TestSystemdStatus(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "systemctl --user show kran.service"):
			return showOut, nil
		case strings.HasPrefix(cmd, "loginctl show-user 501 -p Linger"):
			return "Linger=no\n", nil
		}
		return "", nil
	}}
	m, _ := newMgr(t, "linux", r, nil, nil)
	m.Install(context.Background())
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Installed || !st.Enabled || !st.Running || st.PID != 4242 || st.Manager != "systemd" {
		t.Fatalf("%+v", st)
	}
	if st.Linger == nil || *st.Linger || len(st.Advice) != 1 || !strings.Contains(st.Advice[0], "loginctl enable-linger") {
		t.Fatalf("%+v", st)
	}
}

func TestSystemdStatusStopped(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		if strings.Contains(cmd, " show ") {
			return "ActiveState=inactive\nSubState=dead\nMainPID=0\nUnitFileState=disabled\nLoadState=not-found\n", nil
		}
		return "Linger=yes\n", nil
	}}
	m, _ := newMgr(t, "linux", r, nil, nil)
	st, _ := m.Status(context.Background())
	if st.Installed || st.Running || st.Enabled || len(st.Advice) != 1 || !strings.Contains(st.Advice[0], "kran service install") {
		t.Fatalf("%+v", st)
	}
}

const printOut = `gui/501/dev.hafencity.kran = {
	active count = 1
	path = /Users/u/Library/LaunchAgents/dev.hafencity.kran.plist
	type = LaunchAgent
	state = running

	program = /Users/u/.local/bin/kran
	pid = 777
	immediate reason = ipc (mach)
	properties = keepalive | runatload
}
`

func launchdReply(guiOK bool, loaded *bool) func(string) (string, error) {
	fail := func(cmd string) (string, error) {
		return "", &RunError{Cmd: cmd, Err: errors.New("exit status 113"), Stderr: "Could not find service"}
	}
	return func(cmd string) (string, error) {
		switch cmd {
		case "launchctl print gui/501":
			if guiOK {
				return "domain stuff", nil
			}
			return fail(cmd)
		case "launchctl print gui/501/dev.hafencity.kran", "launchctl print user/501/dev.hafencity.kran":
			if *loaded {
				return printOut, nil
			}
			return fail(cmd)
		}
		if strings.HasPrefix(cmd, "launchctl bootstrap") {
			*loaded = true
		}
		if strings.HasPrefix(cmd, "launchctl bootout") {
			*loaded = false
		}
		return "", nil
	}
}

func TestLaunchdInstallRestartStatusUninstall(t *testing.T) {
	loaded := false
	r := &fakeRunner{reply: launchdReply(true, &loaded)}
	m, o := newMgr(t, "darwin", r, nil, nil)
	ctx := context.Background()
	if changed, err := m.Install(ctx); err != nil || !changed {
		t.Fatal(changed, err)
	}
	plist := filepath.Join(o.Home, "Library/LaunchAgents/dev.hafencity.kran.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Fatal(err)
	}
	if !contains(r.calls, "launchctl bootstrap gui/501 "+plist) || !contains(r.calls, "launchctl enable gui/501/dev.hafencity.kran") {
		t.Fatalf("calls: %v", r.calls)
	}
	r.calls = nil
	if changed, err := m.Install(ctx); err != nil || changed || contains(r.calls, "launchctl bootstrap gui/501 "+plist) {
		t.Fatalf("idempotent install: %v %v %v", changed, err, r.calls)
	}
	r.calls = nil
	if err := m.Restart(ctx, RestartOptions{}); err != nil || !contains(r.calls, "launchctl kickstart -k gui/501/dev.hafencity.kran") {
		t.Fatal(err, r.calls)
	}
	st, _ := m.Status(ctx)
	if !st.Installed || !st.Running || st.PID != 777 || st.Manager != "launchd" || st.LogPath != filepath.Join(o.Home, "state", "daemon.out.log") {
		t.Fatalf("%+v", st)
	}
	if err := m.Stop(ctx); err != nil || loaded {
		t.Fatal(err, loaded)
	}
	st, _ = m.Status(ctx)
	if st.Running || !st.Installed || len(st.Advice) != 1 {
		t.Fatalf("%+v", st)
	}
	if err := m.Start(ctx); err != nil || !loaded {
		t.Fatal(err, loaded)
	}
	if err := m.Uninstall(ctx); err != nil || loaded {
		t.Fatal(err, loaded)
	}
	if _, err := os.Stat(plist); err == nil {
		t.Fatal("plist not removed")
	}
}

func TestLaunchdChangedPlistReloads(t *testing.T) {
	loaded := false
	r := &fakeRunner{reply: launchdReply(true, &loaded)}
	m, o := newMgr(t, "darwin", r, nil, nil)
	ctx := context.Background()
	m.Install(ctx)
	os.WriteFile(filepath.Join(o.Home, "Library/LaunchAgents/dev.hafencity.kran.plist"), []byte("old"), 0o644)
	r.calls = nil
	if changed, err := m.Install(ctx); err != nil || !changed {
		t.Fatal(changed, err)
	}
	bo, bs := index(r.calls, "launchctl bootout gui/501/dev.hafencity.kran"), indexPrefix(r.calls, "launchctl bootstrap")
	if bo < 0 || bs < 0 || bo > bs {
		t.Fatalf("want bootout before bootstrap: %v", r.calls)
	}
}

func TestLaunchdUserDomainFallbackAndLegacyLoad(t *testing.T) {
	loaded := false
	r := &fakeRunner{}
	base := launchdReply(false, &loaded)
	r.reply = func(cmd string) (string, error) {
		if strings.HasPrefix(cmd, "launchctl bootstrap") {
			return "", &RunError{Cmd: cmd, Err: errors.New("exit status 5"), Stderr: "Bootstrap failed: 5: Input/output error"}
		}
		return base(cmd)
	}
	m, _ := newMgr(t, "darwin", r, nil, nil)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if indexPrefix(r.calls, "launchctl bootstrap user/501 ") < 0 {
		t.Fatalf("expected user domain: %v", r.calls)
	}
	if indexPrefix(r.calls, "launchctl load -w ") < 0 {
		t.Fatalf("expected legacy fallback: %v", r.calls)
	}
}

func TestUnsupportedPlatform(t *testing.T) {
	if _, err := New(Options{GOOS: "plan9", KranPath: "/x/kran", Home: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "kran daemon") {
		t.Fatal(err)
	}
}

func TestEnableLinger(t *testing.T) {
	r := &fakeRunner{}
	if err := EnableLinger(context.Background(), Options{GOOS: "linux", UID: 1000, Runner: r, KranPath: "/x/kran", Home: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "loginctl enable-linger 1000" {
		t.Fatal(r.calls)
	}
}

func contains(l []string, s string) bool { return index(l, s) >= 0 }
func index(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}
func indexPrefix(l []string, p string) int {
	for i, x := range l {
		if strings.HasPrefix(x, p) {
			return i
		}
	}
	return -1
}

// An old unit (no format marker, no priority settings) is reported by
// Status and rewritten by Refresh, keeping the binary path and the
// environment it was installed with (an upgrade over ssh has a minimal
// PATH; the user's must survive).
func TestSystemdRefreshKeepsInstalledEnv(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		if strings.Contains(cmd, "show kran.service") {
			return "ActiveState=active\nSubState=running\nMainPID=42\nUnitFileState=enabled\nLoadState=loaded\n", nil
		}
		return "", nil
	}}
	m, o := newMgr(t, "linux", r, map[string]string{"PATH": "/usr/bin"}, nil)
	path := filepath.Join(o.Home, ".config/systemd/user/kran.service")
	old := `[Unit]
Description=kran daemon

[Service]
Type=simple
ExecStart="/opt/my tools/kran" daemon
Restart=always
RestartSec=2
KillMode=process
Environment="KRAN_STATE_DIR=/srv/kr 100%%"
Environment="PATH=/home/u/go/bin:/usr/local/bin:/usr/bin"

[Install]
WantedBy=default.target
`
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(old), 0o644)
	ctx := context.Background()
	st, _ := m.Status(ctx)
	if !st.Outdated {
		t.Fatalf("old unit not reported: %+v", st)
	}
	r.calls = nil
	changed, err := m.Refresh(ctx)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	b, _ := os.ReadFile(path)
	u := string(b)
	for _, want := range []string{
		`ExecStart="/opt/my tools/kran" daemon`,
		`Environment="KRAN_STATE_DIR=/srv/kr 100%%"`,
		`Environment="PATH=/home/u/go/bin:/usr/local/bin:/usr/bin"`,
		"CPUWeight=1000", "MemoryLow=",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %s in\n%s", want, u)
		}
	}
	if !contains(r.calls, "systemctl --user daemon-reload") {
		t.Fatalf("calls %v", r.calls)
	}
	if st, _ := m.Status(ctx); st.Outdated {
		t.Fatal("still outdated after refresh")
	}
	r.calls = nil
	if changed, err := m.Refresh(ctx); err != nil || changed || len(r.calls) != 0 {
		t.Fatalf("second refresh: %v %v %v", changed, err, r.calls)
	}
	// An upgrade's restart refreshes first.
	os.WriteFile(path, []byte(old), 0o644)
	r.calls = nil
	if err := m.Restart(ctx, RestartOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if i, j := index(r.calls, "systemctl --user daemon-reload"), index(r.calls, "systemctl --user restart kran.service"); i < 0 || j < i {
		t.Fatalf("restart must reload the refreshed unit first: %v", r.calls)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "CPUWeight=1000") {
		t.Fatal("restart did not refresh the unit")
	}
}

func TestLaunchdRefreshKeepsInstalledEnvAndReloads(t *testing.T) {
	loaded := true
	r := &fakeRunner{reply: launchdReply(true, &loaded)}
	m, o := newMgr(t, "darwin", r, map[string]string{"PATH": "/usr/bin"}, nil)
	path := filepath.Join(o.Home, "Library/LaunchAgents/dev.hafencity.kran.plist")
	old := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.hafencity.kran</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/u/bin/k&amp;ran</string>
		<string>daemon</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/opt/homebrew/bin:/usr/bin</string>
	</dict>
</dict>
</plist>
`
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(old), 0o644)
	ctx := context.Background()
	if st, _ := m.Status(ctx); !st.Outdated {
		t.Fatalf("old plist not reported: %+v", st)
	}
	r.calls = nil
	if err := m.Restart(ctx, RestartOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	p := string(b)
	for _, want := range []string{"<string>/Users/u/bin/k&amp;ran</string>", "<string>/opt/homebrew/bin:/usr/bin</string>", "<key>LowPriorityIO</key>", "kran-unit-format: 2"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %s in\n%s", want, p)
		}
	}
	// A changed plist only takes effect when launchd loads it again.
	bo, bs := index(r.calls, "launchctl bootout gui/501/dev.hafencity.kran"), indexPrefix(r.calls, "launchctl bootstrap")
	if bo < 0 || bs < bo || contains(r.calls, "launchctl kickstart -k gui/501/dev.hafencity.kran") {
		t.Fatalf("calls %v", r.calls)
	}
}

// The rendered unit passes systemd's own checks, without warnings.
func TestSystemdUnitVerifies(t *testing.T) {
	sa, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze not installed")
	}
	m, _ := newMgr(t, "linux", &fakeRunner{}, map[string]string{"PATH": "/usr/bin"}, func(o *Options) { o.KranPath = "/bin/true" })
	dir := t.TempDir()
	unit := filepath.Join(dir, "kran-verify-test.service")
	os.WriteFile(unit, []byte(m.Unit()), 0o644)
	out, err := exec.Command(sa, "--user", "verify", unit).CombinedOutput()
	if err != nil || len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("systemd-analyze verify: %v\n%s", err, out)
	}
}

// The rendered plist is valid for plutil (macOS only).
func TestLaunchdPlistLints(t *testing.T) {
	pl, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not installed")
	}
	m, _ := newMgr(t, "darwin", &fakeRunner{}, map[string]string{"PATH": "/usr/bin"}, nil)
	f := filepath.Join(t.TempDir(), "kran.plist")
	os.WriteFile(f, []byte(m.Unit()), 0o644)
	if out, err := exec.Command(pl, "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
}
