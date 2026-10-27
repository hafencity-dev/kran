//go:build linux

package cli

import (
	"os"
	"os/exec"
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
	// A plain SIGKILL is not reported as a memory cap.
	errF := tmpFile(t, "e")
	p, _ = startLocal(localJob{JobID: "tkill" + randSuffix(), Argv: []string{"sh", "-c", "kill -9 $$"}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope: &proto.LocalEnvelope{MemMax: capBytes, Slice: "kran.slice"},
		Stdio:    localStdio{Out: tmpFile(t, "o"), Err: errF}})
	if ex := <-p.Done(); ex.Code != 137 || ex.Reason != "" {
		stderr, _ := os.ReadFile(errF.Name())
		t.Fatalf("%+v\nstderr: %s", ex, stderr)
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
