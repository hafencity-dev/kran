package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// hookDecision runs the hook for cmd in project dir proj and reports whether
// it wrapped and with which permission decision.
func hookDecision(t *testing.T, proj, cmd string) (wrapped bool, decision string) {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "cwd": proj, "tool_input": map[string]any{"command": cmd}})
	out, _ := runHook(t, string(in))
	if out == "" {
		return false, ""
	}
	var got struct {
		H struct {
			Perm string `json:"permissionDecision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	return true, got.H.Perm
}

func writeSettings(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The wrapped command must get exactly the permission treatment the
// original would have had: deny/ask rules keep the command unwrapped, allow
// rules carry over as an explicit allow, and nothing else is approved.
func TestHookHonorsPermissionRules(t *testing.T) {
	type files map[string]string // "user", "project", "local", "managed", "config" → settings.json content
	perm := func(kind string, rules ...string) string {
		b, _ := json.Marshal(map[string]any{"permissions": map[string]any{kind: rules}})
		return string(b)
	}
	cases := []struct {
		name     string
		files    files
		cmd      string
		wrap     bool
		decision string
	}{
		{"no settings", nil, "go test ./...", true, ""},
		{"unrelated rules", files{"user": perm("allow", "Read(./src/**)", "WebFetch")}, "go test ./...", true, ""},
		{"allow prefix", files{"user": perm("allow", "Bash(go test:*)")}, "go test ./...", true, "allow"},
		{"allow with filter", files{"user": perm("allow", "Bash(go test:*)")}, "go test ./... 2>&1 | tail -5", true, "allow"},
		{"allow exact", files{"user": perm("allow", "Bash(make test)")}, "make test", true, "allow"},
		{"allow covers only part", files{"user": perm("allow", "Bash(go test:*)")}, "cd web && go test ./...", true, ""},
		{"allow prefix is a word", files{"user": perm("allow", "Bash(go te:*)")}, "go test ./...", true, ""},
		{"allow other command", files{"user": perm("allow", "Bash(go build:*)")}, "go test ./...", true, ""},
		{"deny prefix", files{"user": perm("deny", "Bash(go test:*)")}, "go test ./...", false, ""},
		{"ask exact", files{"user": perm("ask", "Bash(make test)")}, "make test", false, ""},
		{"deny one part", files{"user": perm("deny", "Bash(pnpm build:*)")}, "pnpm test && pnpm build", false, ""},
		{"deny glob", files{"user": perm("deny", "Bash(*-race*)")}, "go test -race ./...", false, ""},
		{"deny bare Bash", files{"user": perm("deny", "Bash")}, "go test ./...", false, ""},
		{"deny after env prefix", files{"user": perm("deny", "Bash(go test:*)")}, "CGO_ENABLED=0 go test ./...", false, ""},
		{"deny wins over allow", files{"user": perm("allow", "Bash(go test:*)"), "project": perm("deny", "Bash(go test:*)")}, "go test ./...", false, ""},
		{"deny in local settings", files{"local": perm("deny", "Bash(cargo test:*)")}, "cargo test", false, ""},
		{"deny in managed settings", files{"managed": perm("deny", "Bash(cargo test:*)")}, "cargo test", false, ""},
		{"deny in CLAUDE_CONFIG_DIR", files{"config": perm("deny", "Bash(cargo test:*)")}, "cargo test", false, ""},
		{"deny of kran itself", files{"user": perm("deny", "Bash(kran run:*)")}, "go test ./...", false, ""},
		// An allow rule for kran would approve what the user never allowed.
		{"allow kran run only", files{"user": perm("allow", "Bash(*kran run:*)")}, "go test ./...", false, ""},
		{"invalid json", files{"user": "{"}, "go test ./...", false, ""},
		{"permissions not an object", files{"user": `{"permissions": []}`}, "go test ./...", false, ""},
		{"rules not strings", files{"user": `{"permissions": {"deny": [1]}}`}, "go test ./...", false, ""},
		{"malformed Bash rule", files{"user": perm("deny", "Bash(go test")}, "go test ./...", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, proj, cfg, managed := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			t.Setenv("CLAUDE_PROJECT_DIR", "")
			old := managedSettings
			managedSettings = []string{filepath.Join(managed, "managed-settings.json")}
			defer func() { managedSettings = old }()
			for k, v := range c.files {
				switch k {
				case "user":
					writeSettings(t, filepath.Join(home, ".claude", "settings.json"), v)
				case "project":
					writeSettings(t, filepath.Join(proj, ".claude", "settings.json"), v)
				case "local":
					writeSettings(t, filepath.Join(proj, ".claude", "settings.local.json"), v)
				case "managed":
					writeSettings(t, managedSettings[0], v)
				case "config":
					t.Setenv("CLAUDE_CONFIG_DIR", cfg)
					writeSettings(t, filepath.Join(cfg, "settings.json"), v)
				}
			}
			wrapped, decision := hookDecision(t, proj, c.cmd)
			if wrapped != c.wrap || decision != c.decision {
				t.Fatalf("wrapped=%v decision=%q, want %v %q", wrapped, decision, c.wrap, c.decision)
			}
		})
	}
}

// Project settings are found from CLAUDE_PROJECT_DIR, or from the command's
// cwd and its parents (an agent may have cd'ed into a subdirectory).
func TestHookFindsProjectSettings(t *testing.T) {
	home, proj := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	writeSettings(t, filepath.Join(proj, ".claude", "settings.json"), `{"permissions":{"deny":["Bash(go test:*)"]}}`)
	sub := filepath.Join(proj, "a", "b")
	os.MkdirAll(sub, 0o755)
	if wrapped, _ := hookDecision(t, sub, "go test ./..."); wrapped {
		t.Fatal("deny rule of the enclosing project ignored")
	}
	t.Setenv("CLAUDE_PROJECT_DIR", proj)
	if wrapped, _ := hookDecision(t, t.TempDir(), "go test ./..."); wrapped {
		t.Fatal("deny rule of CLAUDE_PROJECT_DIR ignored")
	}
}
