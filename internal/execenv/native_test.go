package execenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestSatisfies(t *testing.T) {
	cases := []struct {
		have, pin string
		want      bool
	}{
		{"24.8.0", "24", true},
		{"24.8.0", "24.8", true},
		{"24.8.0", "24.8.1", false},
		{"24.8.0", "v24.8.0", true},
		{"22.1.0", "24", false},
		{"20.11.1", ">=20", true},
		{"18.0.0", ">=20", false},
		{"20.11.1", ">=18 <21", true},
		{"21.0.0", ">=18 <21", false},
		{"1.4.2", "^1.2", true},
		{"2.0.0", "^1.2", false},
		{"0.2.5", "^0.2.3", true},
		{"0.3.0", "^0.2.3", false},
		{"1.2.9", "~1.2.3", true},
		{"1.3.0", "~1.2.3", false},
		{"20.5.0", "20.x", true},
		{"19.0.0", "18 || 19", true},
		{"9.12.1", "9.12.1", true},
		{"3.12.4", "3.12", true},
		{"1.25.3", "go1.25.3", true},
		{"24.0.0", "lts/*", true},
		{"24.0.0", "latest", true},
		{"garbage", "24", false},
		{"1.26rc1", "1.26", true},
	}
	for _, c := range cases {
		if got := satisfies(c.have, c.pin); got != c.want {
			t.Errorf("satisfies(%q, %q) = %v, want %v", c.have, c.pin, got, c.want)
		}
	}
}

func TestMiseVersionAndGoToolchain(t *testing.T) {
	for pin, want := range map[string]string{
		"24": "24", "^1.2.3": "1.2.3", ">=20 <23": "20", "20.x": "20", "lts/iron": "lts", "~3.12": "3.12", "*": "latest", "1.82.0": "1.82.0",
	} {
		if got := miseVersion(pin); got != want {
			t.Errorf("miseVersion(%q) = %q, want %q", pin, got, want)
		}
	}
	for pin, want := range map[string]string{
		"1.25.10": "go1.25.10+auto", "1.25": "go1.25.0+auto", "1.20": "", "1": "", ">=1.22": "", "1.26.0": "go1.26.0+auto",
	} {
		if got := goToolchain(pin); got != want {
			t.Errorf("goToolchain(%q) = %q, want %q", pin, got, want)
		}
	}
}

func TestPlanToolchains(t *testing.T) {
	env := &hostEnv{tools: map[string]string{"go": "1.26.0", "node": "22.3.0", "pnpm": "8.0.0"}}
	tp, err := planToolchains(env, map[string]string{"go": "1.25.10", "node": "22"}, "n")
	if err != nil || tp.env["GOTOOLCHAIN"] != "go1.25.10+auto" || len(tp.mise) != 0 {
		t.Fatalf("plan %+v %v", tp, err)
	}
	_, err = planToolchains(env, map[string]string{"node": "24"}, "n")
	if !errors.Is(err, ErrRuntimeUnavailable) || !strings.Contains(err.Error(), "missing toolchain node@24") {
		t.Fatalf("err = %v", err)
	}
	// Package manager mismatches are not fatal without mise.
	tp, err = planToolchains(env, map[string]string{"pnpm": "9.1.0"}, "n")
	if err != nil || tp.note == "" {
		t.Fatalf("pnpm: %+v %v", tp, err)
	}
	// With mise, missing pins are installed on demand.
	env.mise = "/usr/bin/mise"
	tp, err = planToolchains(env, map[string]string{"node": "24", "python": "3.12", "terraform": "1.9"}, "n")
	if err != nil || strings.Join(tp.mise, ",") != "node@24,python@3.12,terraform@1.9" {
		t.Fatalf("mise plan %+v %v", tp, err)
	}
	argv := nativeArgv([]string{"pytest", "-x"}, "", tp, env.mise)
	if strings.Join(argv, " ") != "/usr/bin/mise exec node@24 python@3.12 terraform@1.9 -- pytest -x" {
		t.Fatalf("argv %v", argv)
	}
}

func TestNativeArgvShell(t *testing.T) {
	argv := nativeArgv([]string{"/no/such/dir/bash", "-c", "echo hi"}, os.Getenv("PATH"), toolPlan{}, "")
	if argv[0] == "/no/such/dir/bash" || filepath.Base(argv[0]) != "bash" && argv[0] != "/bin/sh" {
		t.Fatalf("shell not mapped: %v", argv)
	}
	argv = nativeArgv([]string{"/bin/sh", "-c", "x"}, "", toolPlan{}, "")
	if argv[0] != "/bin/sh" {
		t.Fatalf("existing shell changed: %v", argv)
	}
	if got := containerArgv([]string{"/bin/zsh", "-c", "x"}); got[0] != "/usr/bin/zsh" {
		t.Fatalf("container zsh: %v", got)
	}
}

func TestHostSpecificEnv(t *testing.T) {
	for k, want := range map[string]bool{
		"PATH": true, "HOME": true, "XDG_RUNTIME_DIR": true, "SSH_AUTH_SOCK": true, "DYLD_LIBRARY_PATH": true,
		"KRAN_JOB_ID": true, "KRAN_OWNER": false, "GOFLAGS": false, "NODE_ENV": false, "CI": false, "DOCKER_SOCK": true,
		"TERM": false, "TERM_PROGRAM": true, "LANG": false,
	} {
		if got := hostSpecificEnv(k); got != want {
			t.Errorf("hostSpecificEnv(%s) = %v", k, got)
		}
	}
}

func TestNativeEnv(t *testing.T) {
	e := &hostEnv{path: "/usr/bin:/bin", home: "/home/u", user: "u", shell: "/bin/zsh", vars: map[string]string{"GOPATH": "/gp"}}
	req := &StartRequest{Spec: proto.JobSpec{ID: "j1", Env: map[string]string{"FOO": "bar", "PATH": "/origin/bin", "HOME": "/Users/x", "KRAN_OWNER": "o"},
		TTY: &proto.TTY{Rows: 1, Cols: 1}, Workspace: proto.Workspace{Root: "/src/x"}}, Origin: "mac"}
	env := nativeEnv(e, req, "gx10", "/slot", "/tmp/j1", toolPlan{env: map[string]string{"GOTOOLCHAIN": "go1.25.0+auto"}})
	get := func(k string) string { return envGet(env, k) }
	checks := map[string]string{
		"FOO": "bar", "PATH": "/usr/bin:/bin", "HOME": "/home/u", "TMPDIR": "/tmp/j1", "GOPATH": "/gp",
		"GOMODCACHE": "/gp/pkg/mod", "GOTOOLCHAIN": "go1.25.0+auto", "KRAN_JOB_ID": "j1", "KRAN_NODE": "gx10",
		"KRAN_ORIGIN": "mac", "KRAN_ORIGIN_ROOT": "/src/x", "KRAN_OWNER": "o", "TERM": "xterm-256color", "USER": "u",
	}
	for k, want := range checks {
		if got := get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestMemCapAndUnits(t *testing.T) {
	if c := memCap(3*gib, 100*gib); c != 6*gib {
		t.Errorf("memCap = %d", c)
	}
	if c := memCap(512<<20, 100*gib); c != 2*gib {
		t.Errorf("min 2 GiB: %d", c)
	}
	if c := memCap(4*gib, 5*gib); c != 5*gib {
		t.Errorf("bounded by free: %d", c)
	}
	if c := memCap(0, 5*gib); c != 0 {
		t.Errorf("no estimate, no cap: %d", c)
	}
	for b, want := range map[int64]string{6 * gib: "6 GiB", 5 * gib / 2: "2.5 GiB", 512 << 20: "512 MiB"} {
		if got := fmtGiB(b); got != want {
			t.Errorf("fmtGiB(%d) = %q", b, got)
		}
	}
	if s := suggestMem(6 * gib); s != "12G" {
		t.Errorf("suggestMem = %q", s)
	}
}

func TestCaptureHostEnv(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the login shell")
	}
	e := captureHostEnv(context.Background())
	if e.path == "" || e.home == "" {
		t.Fatalf("env %+v", e)
	}
	if v := e.tools["go"]; v == "" || !strings.HasPrefix(runtime.Version(), "go"+v[:3]) && !strings.HasPrefix(runtime.Version(), "go1") {
		t.Errorf("go toolchain not probed: %v", e.tools)
	}
}

func TestCtlPathFallback(t *testing.T) {
	short := ctlPath("/tmp/j", "j")
	if short != "/tmp/j/ctl.sock" {
		t.Fatal(short)
	}
	long := ctlPath("/"+strings.Repeat("d", 120), "j")
	if len(long) >= 100 || !strings.HasSuffix(long, ".sock") {
		t.Fatalf("fallback %q", long)
	}
}

func TestRuntimesNative(t *testing.T) {
	te := newTestEnv(t)
	rts := te.m.Runtimes(context.Background())
	if len(rts) == 0 || rts[0].Kind != proto.RuntimeNative || !rts[0].Available || rts[0].OS != runtime.GOOS || rts[0].CPUs <= 0 || rts[0].Mem <= 0 {
		t.Fatalf("runtimes %+v", rts)
	}
}
