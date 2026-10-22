package agents

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	daemonUp = func() bool { return true }
	// The hook reads Claude Code settings: never the developer's own.
	home, _ := os.MkdirTemp("", "kran-agents-home-")
	os.Setenv("HOME", home)
	os.Unsetenv("CLAUDE_CONFIG_DIR")
	os.Unsetenv("CLAUDE_PROJECT_DIR")
	// Nor the developer's opt-out: tests often run with KRAN_NO_HOOK=1 so
	// the hook leaves the test command itself alone.
	os.Unsetenv("KRAN_NO_HOOK")
	managedSettings = []string{home + "/managed-settings.json"}
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestHookStaysOutWithoutDaemon(t *testing.T) {
	daemonUp = func() bool { return false }
	defer func() { daemonUp = func() bool { return true } }()
	out, code := runHook(t, `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)
	if out != "" || code != 0 {
		t.Fatalf("without a daemon the hook must stay silent, got code=%d out=%q", code, out)
	}
}

func runHook(t *testing.T, in string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := HookClaude(strings.NewReader(in), &out, "/opt/kran/bin/kran")
	return out.String(), code
}

func TestHookWraps(t *testing.T) {
	in := `{"session_id":"s","cwd":"/r","hook_event_name":"PreToolUse","tool_name":"Bash","tool_use_id":"t",
	 "tool_input":{"command":"go test ./... 2>&1 | tail -5","description":"Run tests","timeout":120000,"run_in_background":false,"future_field":{"a":1}}}`
	out, code := runHook(t, in)
	if code != 0 || out == "" {
		t.Fatal(code, out)
	}
	var got struct {
		H struct {
			Event   string                     `json:"hookEventName"`
			Updated map[string]json.RawMessage `json:"updatedInput"`
			Perm    *string                    `json:"permissionDecision"`
			Ctx     string                     `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.H.Event != "PreToolUse" || got.H.Perm != nil || got.H.Ctx == "" {
		t.Fatalf("%+v", got.H)
	}
	var cmd string
	json.Unmarshal(got.H.Updated["command"], &cmd)
	if cmd != "/opt/kran/bin/kran run --sh 'go test ./... 2>&1 | tail -5'" {
		t.Fatalf("command %q", cmd)
	}
	// The whole input must be carried over: updatedInput replaces it.
	for _, k := range []string{"description", "timeout", "run_in_background", "future_field"} {
		if _, ok := got.H.Updated[k]; !ok {
			t.Errorf("updatedInput lost %q", k)
		}
	}
	if string(got.H.Updated["future_field"]) != `{"a":1}` {
		t.Errorf("future_field changed: %s", got.H.Updated["future_field"])
	}
}

func TestHookStaysSilent(t *testing.T) {
	cases := map[string]string{
		"background":   `{"tool_name":"Bash","tool_input":{"command":"go test ./...","run_in_background":true}}`,
		"not heavy":    `{"tool_name":"Bash","tool_input":{"command":"ls"}}`,
		"already kran": `{"tool_name":"Bash","tool_input":{"command":"kran run -- go test ./..."}}`,
		"other tool":   `{"tool_name":"Edit","tool_input":{"command":"go test ./..."}}`,
		"malformed":    `{"tool_name":"Bash",`,
		"empty":        ``,
		"no input":     `{"tool_name":"Bash"}`,
		"cmd not str":  `{"tool_name":"Bash","tool_input":{"command":7}}`,
		"dev server":   `{"tool_name":"Bash","tool_input":{"command":"npm run dev"}}`,
	}
	for name, in := range cases {
		out, code := runHook(t, in)
		if out != "" || code != 0 {
			t.Errorf("%s: want silence, got code=%d out=%q", name, code, out)
		}
	}
}

func TestHookDisabledByEnv(t *testing.T) {
	t.Setenv("KRAN_NO_HOOK", "1")
	out, code := runHook(t, `{"tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)
	if out != "" || code != 0 {
		t.Fatal(out, code)
	}
}
