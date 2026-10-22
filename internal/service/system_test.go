package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSystemMgr(t *testing.T, r Runner, uid int, mod func(*Options)) (Manager, Options) {
	t.Helper()
	dir := t.TempDir()
	return newMgr(t, "darwin", r, map[string]string{"PATH": "/opt/homebrew/bin:/usr/bin", "KRAN_STATE_DIR": ""}, func(o *Options) {
		o.System, o.SystemDir, o.User, o.Group, o.UID = true, dir, "acme", "staff", uid
		o.Home = "/Users/dev"
		o.LogPath = filepath.Join(t.TempDir(), "state", "daemon.out.log")
		if mod != nil {
			mod(o)
		}
	})
}

func TestSystemPlist(t *testing.T) {
	m, o := newSystemMgr(t, &fakeRunner{}, 501, nil)
	u := m.Unit()
	for _, want := range []string{
		"<key>Label</key>\n\t<string>dev.hafencity.kran</string>",
		"<key>UserName</key>\n\t<string>acme</string>",
		"<key>GroupName</key>\n\t<string>staff</string>",
		"<key>InitGroups</key>\n\t<true/>",
		"<key>RunAtLoad</key>\n\t<true/>", "<key>KeepAlive</key>\n\t<true/>",
		"<key>HOME</key>\n\t\t<string>/Users/dev</string>",
		"<key>USER</key>\n\t\t<string>acme</string>",
		"<key>LOGNAME</key>\n\t\t<string>acme</string>",
		"<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/usr/bin</string>",
		"<string>/home/u/.local/bin/kran</string>\n\t\t<string>daemon</string>",
		"<key>StandardOutPath</key>\n\t<string>" + o.LogPath + "</string>",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q in:\n%s", want, u)
		}
	}
	// The agent flavor must not carry user switching.
	a, _ := newMgr(t, "darwin", &fakeRunner{}, nil, nil)
	if strings.Contains(a.Unit(), "UserName") {
		t.Fatal("agent plist must not set UserName")
	}
}

func TestSystemPlistKranOverridesAndEscaping(t *testing.T) {
	m, _ := newSystemMgr(t, &fakeRunner{}, 501, func(o *Options) {
		o.Getenv = envOf(map[string]string{"KRAN_STATE_DIR": "/Users/l/Library/Application Support/kran-dev", "PATH": "/a&b"})
	})
	u := m.Unit()
	if !strings.Contains(u, "<key>KRAN_STATE_DIR</key>\n\t\t<string>/Users/l/Library/Application Support/kran-dev</string>") || !strings.Contains(u, "/a&amp;b") {
		t.Fatal(u)
	}
	if !strings.Contains(m.(*launchd).label, "dev.hafencity.kran.") {
		t.Fatal("overrides must derive an instance label")
	}
}

func TestSystemInstallUsesSudoInOrder(t *testing.T) {
	loaded := false
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		switch {
		case strings.HasPrefix(cmd, "sudo launchctl bootstrap"):
			loaded = true
		case strings.HasPrefix(cmd, "launchctl print system/") || strings.HasPrefix(cmd, "sudo -n launchctl print system/"):
			if !loaded {
				return "", os.ErrNotExist
			}
		}
		return "", nil
	}}
	var logs []string
	m, o := newSystemMgr(t, r, 501, func(o *Options) { o.Logf = func(f string, a ...any) { logs = append(logs, f) } })
	changed, err := m.Install(context.Background())
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "administrator rights") {
		t.Fatalf("exactly one sudo explanation expected: %v", logs)
	}
	iInstall := indexPrefix(r.calls, "sudo install -m 644 -o root -g wheel ")
	iBoot := indexPrefix(r.calls, "sudo launchctl bootstrap system "+filepath.Join(o.SystemDir, "dev.hafencity.kran.plist"))
	iEnable := indexPrefix(r.calls, "sudo launchctl enable system/dev.hafencity.kran")
	if iInstall < 0 || iBoot < iInstall || iEnable < iInstall {
		t.Fatalf("order wrong: %q", r.calls)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "launchctl bootstrap") || strings.HasPrefix(c, "launchctl enable") {
			t.Errorf("state changes must go through sudo: %s", c)
		}
	}
	if st, err := os.Stat(o.LogPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("log must be pre-created by the user: %v %v", st, err)
	}
}

func TestSystemInstallRootSkipsSudo(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		if strings.Contains(cmd, "print") {
			return "", os.ErrNotExist
		}
		return "", nil
	}}
	m, o := newSystemMgr(t, r, 0, nil)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.calls {
		if strings.HasPrefix(c, "sudo") {
			t.Errorf("root needs no sudo: %s", c)
		}
	}
	b, err := os.ReadFile(filepath.Join(o.SystemDir, "dev.hafencity.kran.plist"))
	if err != nil || string(b) != m.Unit() {
		t.Fatal(err)
	}
}

func TestSystemInstallIdempotent(t *testing.T) {
	r := &fakeRunner{}
	m, o := newSystemMgr(t, r, 501, nil)
	os.WriteFile(filepath.Join(o.SystemDir, "dev.hafencity.kran.plist"), []byte(m.Unit()), 0o644)
	changed, err := m.Install(context.Background())
	if err != nil || changed {
		t.Fatal(changed, err)
	}
	if indexPrefix(r.calls, "sudo install") >= 0 {
		t.Fatalf("unchanged plist must not be rewritten: %q", r.calls)
	}
}

func TestSystemInstallRemovesAgent(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		if strings.Contains(cmd, "print") {
			return "", os.ErrNotExist
		}
		return "", nil
	}}
	home := t.TempDir()
	m, _ := newSystemMgr(t, r, 501, func(o *Options) { o.Home = home })
	agent := filepath.Join(home, "Library", "LaunchAgents", "dev.hafencity.kran.plist")
	os.MkdirAll(filepath.Dir(agent), 0o755)
	os.WriteFile(agent, []byte("x"), 0o644)
	if _, err := m.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agent); err == nil {
		t.Fatal("the per-user agent must be retired when the system daemon is installed")
	}
}

func TestSystemUninstallAndLifecycleUseSudo(t *testing.T) {
	r := &fakeRunner{}
	m, o := newSystemMgr(t, r, 501, nil)
	plist := filepath.Join(o.SystemDir, "dev.hafencity.kran.plist")
	os.WriteFile(plist, []byte("x"), 0o644)
	ctx := context.Background()
	if err := m.Restart(ctx, RestartOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if indexPrefix(r.calls, "sudo launchctl kickstart -k system/dev.hafencity.kran") < 0 {
		t.Fatalf("%q", r.calls)
	}
	r.calls = nil
	if err := m.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if indexPrefix(r.calls, "sudo launchctl bootout system/dev.hafencity.kran") < 0 || indexPrefix(r.calls, "sudo rm -f "+plist) < 0 {
		t.Fatalf("%q", r.calls)
	}
}

func TestSystemStatusAndPlan(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) {
		if strings.HasSuffix(cmd, "print system/dev.hafencity.kran") {
			return "state = running\n\tpid = 99\n", nil
		}
		return "", nil
	}}
	m, o := newSystemMgr(t, r, 501, nil)
	os.WriteFile(filepath.Join(o.SystemDir, "dev.hafencity.kran.plist"), []byte("x"), 0o644)
	st, err := m.Status(context.Background())
	if err != nil || st.Mode != "launchd-system" || !st.Running || st.PID != 99 || st.NoGUI {
		t.Fatalf("%+v %v", st, err)
	}
	plan := m.(interface{ Plan() []string }).Plan()
	if len(plan) < 4 || !strings.HasPrefix(plan[0], "sudo install") {
		t.Fatalf("%v", plan)
	}
}

func TestNoGUIAdviceAndMode(t *testing.T) {
	r := &fakeRunner{reply: func(cmd string) (string, error) { return "", os.ErrNotExist }}
	m, o := newMgr(t, "darwin", r, nil, nil)
	st, _ := m.Status(context.Background())
	if !st.NoGUI || st.Installed || len(st.Advice) != 1 || !strings.Contains(st.Advice[0], "kran service install --system") {
		t.Fatalf("%+v", st)
	}
	if HasGUISession(context.Background(), o) {
		t.Fatal("print failed, so no GUI session")
	}
	if Mode(o) != "" {
		t.Fatal("nothing installed")
	}
	os.MkdirAll(filepath.Join(o.Home, "Library", "LaunchAgents"), 0o755)
	os.WriteFile(filepath.Join(o.Home, "Library", "LaunchAgents", "dev.hafencity.kran.plist"), []byte("x"), 0o644)
	if Mode(o) != "launchd-agent" {
		t.Fatal(Mode(o))
	}
	o.SystemDir = t.TempDir()
	os.WriteFile(filepath.Join(o.SystemDir, "dev.hafencity.kran.plist"), []byte("x"), 0o644)
	if Mode(o) != "launchd-system" {
		t.Fatal(Mode(o))
	}
}

func TestNewAutoSelectsInstalledSystemDaemon(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "dev.hafencity.kran.plist"), []byte("x"), 0o644)
	m, _ := newMgr(t, "darwin", &fakeRunner{}, nil, func(o *Options) { o.SystemDir = dir })
	if !m.(*launchd).o.System {
		t.Fatal("an installed system daemon must be managed by default")
	}
}

// SPEC §9: jobs never run as root. `sudo kran service install` would bake
// root into the unit (or run a user agent as root), so it is refused.
func TestInstallRefusesRoot(t *testing.T) {
	m, _ := newSystemMgr(t, &fakeRunner{}, 0, func(o *Options) { o.User = "root" })
	if _, err := m.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Fatalf("system daemon as root: %v", err)
	}
	a, _ := newMgr(t, "darwin", &fakeRunner{}, nil, func(o *Options) { o.UID = 0 })
	if _, err := a.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "without sudo") {
		t.Fatalf("launch agent as root: %v", err)
	}
	if err := refuseRoot(Options{GOOS: "linux", UID: 0}); err == nil {
		t.Fatal("systemd user unit as root")
	}
	if err := refuseRoot(Options{GOOS: "linux", UID: 1000}); err != nil {
		t.Fatal(err)
	}
}
