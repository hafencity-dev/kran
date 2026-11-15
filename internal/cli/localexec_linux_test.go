//go:build linux

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// TestLocalScopeOOMReason: a local job killed at its scope's memory cap
// says so (SPEC §6.8) and leaves no failed unit behind.
func TestLocalScopeOOMReason(t *testing.T) {
	const capBytes = 64 << 20
	if !scopeUsable(capBytes) {
		t.Skip("systemd user scopes with a memory cap are not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("needs python3")
	}
	p, lerr := startLocal(localJob{JobID: "toom" + randSuffix(), Node: "gx10", Argv: []string{"python3", "-c",
		"x = bytearray(256 << 20); x[::4096] = b'a' * len(x[::4096])"}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope: &proto.LocalEnvelope{MemMax: capBytes, Slice: "kran.slice"},
		Stdio:    localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	var ex proto.Exit
	select {
	case ex = <-p.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("timeout")
	}
	if ex.Code != 137 || !strings.Contains(ex.Reason, "job hit its 64.0 MiB memory cap on gx10") || !strings.Contains(ex.Reason, "--mem") {
		t.Fatalf("%+v", ex)
	}
	out, _ := exec.Command("systemctl", "--user", "show", "-p", "LoadState", "--value", p.Started.Scope).Output()
	if strings.TrimSpace(string(out)) != "not-found" {
		t.Errorf("unit %s left behind: %s", p.Started.Scope, out)
	}
	// A plain SIGKILL is not reported as a memory cap, but as a signal.
	errF := tmpFile(t, "e")
	p, _ = startLocal(localJob{JobID: "tkill" + randSuffix(), Argv: []string{"sh", "-c", "kill -9 $$"}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope: &proto.LocalEnvelope{MemMax: capBytes, Slice: "kran.slice"},
		Stdio:    localStdio{Out: tmpFile(t, "o"), Err: errF}})
	if ex := <-p.Done(); ex.Code != 137 || strings.Contains(ex.Reason, "memory") || !strings.Contains(ex.Reason, "job ended by signal KILL") {
		stderr, _ := os.ReadFile(errF.Name())
		t.Fatalf("%+v\nstderr: %s", ex, stderr)
	}
}

// fakeSystemd puts a systemd-run that runs the command after "--" (and
// records its arguments) and a systemctl that answers `show -p Result`
// with result (and records reset-failed) first on PATH.
func fakeSystemd(t *testing.T, result string) (argsFile, resetFile string) {
	t.Helper()
	bin := t.TempDir()
	argsFile, resetFile = filepath.Join(bin, "args"), filepath.Join(bin, "reset")
	os.WriteFile(filepath.Join(bin, "systemd-run"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argsFile+
		"\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\"\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\ncase \"$*\" in\n*reset-failed*) echo \"$*\" >> "+resetFile+
		";;\n*'show -p Result'*) echo "+result+";;\nesac\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	return argsFile, resetFile
}

// TestLocalScopeOOMKillThenTERMSaysSo: 2026-11-15 on gx10. The kernel's
// OOM killer ended a compiler inside a local job's scope, systemd stopped
// the rest of the scope with TERM, and kran said nothing: the job's own
// exit is a TERM. kran reads the scope's result for every ending, not only
// for a KILL, and names the OOM killer (with or without a cap of its own).
func TestLocalScopeOOMKillThenTERMSaysSo(t *testing.T) {
	_, resetFile := fakeSystemd(t, "oom-kill")
	note := ""
	run := func(id string, env *proto.LocalEnvelope, asked int64) proto.Exit {
		p, lerr := startLocal(localJob{JobID: id, Node: "gx10", Argv: []string{"sh", "-c", "kill -TERM $$"}, Dir: t.TempDir(), Env: os.Environ(),
			Envelope: env, AskedMem: asked, CapNote: note, ScopeProbe: func(int64) bool { return true },
			Stdio: localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
		if lerr != nil {
			t.Fatal(lerr)
		}
		if p.Started.Scope == "" {
			t.Fatalf("%s: no scope", id)
		}
		return <-p.Done()
	}
	// No --mem: no cap, but a scope, and the machine ran out of memory.
	ex := run("toomterm", &proto.LocalEnvelope{Slice: "kran.slice"}, 0)
	if ex.Code != 143 || !strings.Contains(ex.Reason, "out-of-memory killer ended a process of the job on gx10") ||
		!strings.Contains(ex.Reason, "no memory cap of its own") || !strings.Contains(ex.Reason, "journalctl --user -u kran-local-toomterm.scope") {
		t.Fatalf("%+v", ex)
	}
	// --mem 1G: the cap of 2 GiB was hit.
	ex = run("toomcap", &proto.LocalEnvelope{MemMax: 2 << 30, Slice: "kran.slice"}, 1<<30)
	if !strings.Contains(ex.Reason, "job hit its 2.0 GiB memory cap on gx10 (from --mem 1.0 GiB)") || !strings.Contains(ex.Reason, "--mem 4G") {
		t.Fatalf("%+v", ex)
	}
	// No --mem, no kran.slice above it: the node's default cap, not --mem.
	note = "this node's default for a job without --mem"
	ex = run("toomdef", &proto.LocalEnvelope{MemMax: 8 << 30}, 0)
	if strings.Contains(ex.Reason, "from --mem") || !strings.Contains(ex.Reason, "job hit its 8.0 GiB memory cap on gx10 (this node's default for a job without --mem)") {
		t.Fatalf("%+v", ex)
	}
	if b, _ := os.ReadFile(resetFile); !strings.Contains(string(b), "kran-local-toomterm.scope") || !strings.Contains(string(b), "kran-local-toomcap.scope") {
		t.Errorf("failed scopes not reset: %q", b)
	}
}

// TestLocalJobWithoutMemHasNoCap: without --mem the daemon's envelope has
// no cap; the job still gets its scope (so it is measured and killed
// whole, and an OOM kill in it is seen), with TasksMax but no MemoryMax.
func TestLocalJobWithoutMemHasNoCap(t *testing.T) {
	argsFile, _ := fakeSystemd(t, "success")
	p, lerr := startLocal(localJob{JobID: "tnocap", Argv: []string{"true"}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope:   &proto.LocalEnvelope{Slice: "kran.slice"},
		ScopeProbe: func(m int64) bool { return m == 0 }, Stdio: localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if ex := <-p.Done(); ex.Code != 0 || ex.Reason != "" {
		t.Fatalf("%+v", ex)
	}
	b, _ := os.ReadFile(argsFile)
	if p.Started.Scope != "kran-local-tnocap.scope" || !strings.Contains(string(b), "--scope") || !strings.Contains(string(b), "TasksMax=") {
		t.Fatalf("no scope: %q %+v", b, p.Started)
	}
	for _, bad := range []string{"MemoryMax", "MemorySwapMax", "MemoryHigh"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("%s set without --mem: %q", bad, b)
		}
	}
}

// TestLocalSignalEndingIsNamed: a job ended by a signal kran run did not
// pass on says so; one kran run passed on (Ctrl-C) stays quiet.
func TestLocalSignalEndingIsNamed(t *testing.T) {
	p, lerr := startLocal(localJob{JobID: "tsig", Node: "gx10", Argv: []string{"sh", "-c", "kill -TERM $$"}, Dir: t.TempDir(), Env: os.Environ(),
		Stdio: localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if ex := <-p.Done(); ex.Code != 143 || ex.Reason != "job ended by signal TERM on gx10" {
		t.Fatalf("%+v", ex)
	}
	p, lerr = startLocal(localJob{JobID: "tsig2", Argv: []string{"sh", "-c", "sleep 30"}, Dir: t.TempDir(), Env: os.Environ(),
		Stdio: localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if err := p.Signal("INT"); err != nil {
		t.Fatal(err)
	}
	if ex := <-p.Done(); ex.Signal != "INT" || ex.Reason != "" {
		t.Fatalf("a signal kran run passed on: %+v", ex)
	}
}

func randSuffix() string { return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") }

// TestLocalScopeKeepsTheCommandLine: newer systemd-run expands "$VAR" and
// "$$" in the command it runs unless told not to; a job's arguments must
// arrive exactly as given.
func TestLocalScopeKeepsTheCommandLine(t *testing.T) {
	const capBytes = 256 << 20
	if !scopeUsable(capBytes) {
		t.Skip("systemd user scopes with a memory cap are not available")
	}
	out := tmpFile(t, "o")
	arg := `a$$b$HOME${USER}c`
	p, lerr := startLocal(localJob{JobID: "tdollar" + randSuffix(), Argv: []string{"sh", "-c", `printf '%s' "$1"`, "sh", arg}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope: &proto.LocalEnvelope{MemMax: capBytes, Slice: "kran.slice"},
		Stdio:    localStdio{Out: out, Err: tmpFile(t, "e")}})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if ex := <-p.Done(); ex.Code != 0 {
		t.Fatalf("%+v", ex)
	}
	if got, _ := os.ReadFile(out.Name()); string(got) != arg {
		t.Fatalf("the job got %q, want %q", got, arg)
	}
}
