package cli

import (
	"fmt"
	"testing"

	"github.com/hafencity-dev/kran/internal/proto"
)

type fakeProcs map[int]Proc

func (f fakeProcs) Proc(pid int) (Proc, error) {
	p, ok := f[pid]
	if !ok {
		return Proc{}, fmt.Errorf("no pid %d", pid)
	}
	return p, nil
}

func TestDetectOwner(t *testing.T) {
	chain := func(comm string, args ...string) fakeProcs {
		return fakeProcs{
			100: {PID: 100, PPID: 90, Comm: "kran"},
			90:  {PID: 90, PPID: 80, Comm: "bash"},
			80:  {PID: 80, PPID: 70, Comm: comm, Args: args},
			70:  {PID: 70, PPID: 1, Comm: "systemd"},
		}
	}
	cases := []struct {
		name     string
		env      map[string]string
		procs    fakeProcs
		wantKind proto.OwnerKind
		wantID   string
	}{
		{"claude binary", nil, chain("claude"), proto.OwnerClaude, "80"},
		{"claude via node script", nil, chain("node", "node", "/usr/lib/node_modules/@anthropic-ai/claude-code/bin/claude.js"), proto.OwnerClaude, "80"},
		{"claude with session env", map[string]string{"CLAUDE_SESSION_ID": "abcdef123456"}, chain("claude"), proto.OwnerClaude, "abcdef123456"},
		{"codex", nil, chain("/opt/codex/codex"), proto.OwnerCodex, "80"},
		{"cursor mac app", nil, chain("/Applications/Cursor.app/Contents/MacOS/Cursor Helper"), proto.OwnerAgent, "80"},
		{"vs code electron", nil, chain("/Applications/Visual Studio Code.app/Contents/MacOS/Electron"), proto.OwnerAgent, "80"},
		{"code exact", nil, chain("code"), proto.OwnerAgent, "80"},
		{"not code: codec", nil, chain("codec"), proto.OwnerHuman, "u@pts/3"},
		{"env only claude", map[string]string{"CLAUDECODE": "1"}, chain("bash"), proto.OwnerClaude, "claude@u"},
		{"env only codex", map[string]string{"CODEX_SESSION_ID": "s1"}, chain("bash"), proto.OwnerCodex, "s1"},
		{"ci", map[string]string{"CI": "true"}, chain("bash"), proto.OwnerCI, "ci"},
		{"ci false", map[string]string{"CI": "false"}, chain("bash"), proto.OwnerHuman, "u@pts/3"},
		{"explicit kind", map[string]string{"KRAN_OWNER": "claude:nightly"}, chain("bash"), proto.OwnerClaude, "nightly"},
		{"explicit bare", map[string]string{"KRAN_OWNER": "my-script"}, chain("claude"), proto.OwnerAgent, "my-script"},
		{"no procs", nil, nil, proto.OwnerHuman, "u@pts/3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := OwnerInput{Env: c.env, PID: 100, User: "u", TTY: "pts/3"}
			if c.procs != nil {
				in.Procs = c.procs
			}
			got := DetectOwner(in)
			if got.Kind != c.wantKind || got.ID != c.wantID {
				t.Fatalf("got %+v want kind %s id %s", got, c.wantKind, c.wantID)
			}
			if got.Name == "" {
				t.Fatal("empty name")
			}
		})
	}
}

func TestDetectOwnerNearestAncestorAndCycle(t *testing.T) {
	procs := fakeProcs{
		100: {PID: 100, PPID: 90, Comm: "kran"},
		90:  {PID: 90, PPID: 80, Comm: "codex"},
		80:  {PID: 80, PPID: 80, Comm: "claude"}, // self-parent: must not loop forever
	}
	got := DetectOwner(OwnerInput{PID: 100, User: "u", Procs: procs})
	if got.Kind != proto.OwnerCodex || got.ID != "90" {
		t.Fatalf("%+v", got)
	}
}

func TestDetectOwnerRealProcs(t *testing.T) {
	// Smoke test against the real process table: must not panic or hang.
	_ = DetectOwner(OwnerInput{PID: 1<<22 + 1, User: "u", Procs: newProcReader()})
}
