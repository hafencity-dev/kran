package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func tmpFile(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func runLocalJob(t *testing.T, argv ...string) (proto.Exit, string, string, *localError) {
	t.Helper()
	out, errf := tmpFile(t, "out"), tmpFile(t, "err")
	p, lerr := startLocal(localJob{JobID: "t1", Argv: argv, Dir: t.TempDir(), Env: os.Environ(),
		Stdio: localStdio{In: nil, Out: out, Err: errf}})
	if lerr != nil {
		return proto.Exit{}, "", "", lerr
	}
	var ex proto.Exit
	select {
	case ex = <-p.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("timeout")
	}
	o, _ := os.ReadFile(out.Name())
	e, _ := os.ReadFile(errf.Name())
	return ex, string(o), string(e), nil
}

func TestLocalExitCodesAndOutput(t *testing.T) {
	ex, out, errs, lerr := runLocalJob(t, "sh", "-c", "printf 'a\\0b'; printf err >&2; exit 7")
	if lerr != nil || ex.Code != 7 || out != "a\x00b" || errs != "err" || ex.State != proto.JobFailed {
		t.Fatalf("%+v %q %q %v", ex, out, errs, lerr)
	}
	ex, _, _, _ = runLocalJob(t, "true")
	if ex.Code != 0 || ex.State != proto.JobSucceeded || ex.Usage.WallSeconds <= 0 {
		t.Fatalf("%+v", ex)
	}
}

func TestLocalSignalExit(t *testing.T) {
	ex, _, _, _ := runLocalJob(t, "sh", "-c", "kill -TERM $$; sleep 5")
	if ex.Code != 128+int(syscall.SIGTERM) || ex.Signal != "TERM" {
		t.Fatalf("%+v", ex)
	}
}

func TestLocalNotFoundAndNotExecutable(t *testing.T) {
	_, _, _, lerr := runLocalJob(t, "definitely-not-a-command-kran")
	if lerr == nil || lerr.Code != 127 {
		t.Fatalf("%v", lerr)
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "plain")
	os.WriteFile(f, []byte("x"), 0o644)
	_, _, _, lerr = runLocalJob(t, f)
	if lerr == nil || lerr.Code != 126 {
		t.Fatalf("%v", lerr)
	}
	_, _, _, lerr = runLocalJob(t, dir)
	if lerr == nil || lerr.Code != 126 {
		t.Fatalf("dir: %v", lerr)
	}
}

func TestLocalSignalForwardingKillsGroup(t *testing.T) {
	out := tmpFile(t, "out")
	p, lerr := startLocal(localJob{JobID: "t2", Argv: []string{"sh", "-c", "sleep 30 & echo $! ; wait"},
		Dir: t.TempDir(), Env: os.Environ(), Stdio: localStdio{Out: out, Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	time.Sleep(300 * time.Millisecond)
	if err := p.Signal("TERM"); err != nil {
		t.Fatal(err)
	}
	select {
	case ex := <-p.Done():
		if ex.Code != 143 {
			t.Fatalf("%+v", ex)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job did not stop")
	}
	b, _ := os.ReadFile(out.Name())
	pid := strings.TrimSpace(string(b))
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat("/proc/" + pid); err == nil {
		// Child may still be a zombie being reaped by init; check state.
		st, _ := os.ReadFile("/proc/" + pid + "/stat")
		if !strings.Contains(string(st), ") Z") {
			t.Fatalf("background child %s survived the group signal", pid)
		}
	}
	if err := p.Signal("BOGUS"); err == nil {
		t.Fatal("unknown signal must error")
	}
}

func TestLocalEnvelopeEnvAndScopeFallback(t *testing.T) {
	out := tmpFile(t, "out")
	p, lerr := startLocal(localJob{JobID: "t3", Argv: []string{"sh", "-c", "printf %s \"$KRAN_JOB_ID\""}, Dir: t.TempDir(),
		Env:        []string{"PATH=" + os.Getenv("PATH")},
		Envelope:   &proto.LocalEnvelope{MemMax: 1 << 30, Env: map[string]string{"KRAN_JOB_ID": "abc"}},
		ScopeProbe: func(int64) bool { return false },
		Stdio:      localStdio{Out: out, Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	ex := <-p.Done()
	b, _ := os.ReadFile(out.Name())
	if ex.Code != 0 || string(b) != "abc" || p.Started.Scope != "" {
		t.Fatalf("%+v %q %+v", ex, b, p.Started)
	}
}

func TestLocalScopeCommandLine(t *testing.T) {
	// With a working probe the command is wrapped; verify via a fake systemd-run.
	bin := t.TempDir()
	fake := filepath.Join(bin, "systemd-run")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+filepath.Join(bin, "args")+"\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	p, lerr := startLocal(localJob{JobID: "t4", Argv: []string{"true"}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope:   &proto.LocalEnvelope{MemMax: 4096, Slice: "kran.slice"},
		ScopeProbe: func(int64) bool { return true }, Stdio: localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if ex := <-p.Done(); ex.Code != 0 {
		t.Fatalf("%+v", ex)
	}
	b, _ := os.ReadFile(filepath.Join(bin, "args"))
	for _, want := range []string{"--user", "--scope", "--slice=kran.slice", "--unit=kran-local-t4", "MemoryMax=4096", "MemorySwapMax=0", "--quiet"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %q", want, b)
		}
	}
	if p.Started.Scope != "kran-local-t4.scope" {
		t.Fatal(p.Started.Scope)
	}
}

func TestLookPath(t *testing.T) {
	if p, e := lookPath("sh", os.Environ(), "/"); e != nil || !filepath.IsAbs(p) {
		t.Fatal(p, e)
	}
	if _, e := lookPath("./nope", os.Environ(), t.TempDir()); e == nil || e.Code != 127 {
		t.Fatal(e)
	}
}
