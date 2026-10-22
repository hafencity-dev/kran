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
	p, _ = startLocal(localJob{JobID: "tkill" + randSuffix(), Argv: []string{"sh", "-c", "kill -KILL $$"}, Dir: t.TempDir(), Env: os.Environ(),
		Envelope: &proto.LocalEnvelope{MemMax: capBytes, Slice: "kran.slice"},
		Stdio:    localStdio{Out: tmpFile(t, "o"), Err: tmpFile(t, "e")}})
	if ex := <-p.Done(); ex.Code != 137 || ex.Reason != "" {
		t.Fatalf("%+v", ex)
	}
}

func randSuffix() string { return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "") }
