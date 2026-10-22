package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOpts(t *testing.T) Options {
	t.Helper()
	home := t.TempDir()
	return Options{Home: home, KranPath: "/opt/kran/bin/kran", Now: func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }}
}

func states(r Report) map[string]string {
	m := map[string]string{}
	for _, it := range r.Items {
		m[it.Name] = it.State
	}
	return m
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(s), 0o640); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, home string) map[string]string {
	t.Helper()
	m := map[string]string{}
	filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			m[strings.TrimPrefix(p, home)] = string(b)
		}
		return nil
	})
	return m
}

func TestInstallFreshIsIdempotent(t *testing.T) {
	o := testOpts(t)
	rep, err := Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for n, s := range states(rep) {
		if s != StateInstalled {
			t.Errorf("%s: %s", n, s)
		}
	}
	first := snapshot(t, o.Home)
	if len(first) != 4 {
		t.Fatalf("files: %v", first)
	}
	skill := read(t, filepath.Join(o.Home, ".claude/skills/kran/SKILL.md"))
	if !strings.HasPrefix(skill, "---\nname: kran\ndescription: ") || !strings.Contains(skill, "125") {
		t.Fatalf("skill: %s", skill)
	}
	rep, err = Install(o)
	if err != nil {
		t.Fatal(err)
	}
	for n, s := range states(rep) {
		if s != StateUnchanged {
			t.Errorf("second install %s: %s", n, s)
		}
	}
	second := snapshot(t, o.Home)
	if len(second) != len(first) {
		t.Fatalf("second install created files: %v", second)
	}
	for k, v := range first {
		if second[k] != v {
			t.Errorf("%s changed on second install", k)
		}
	}
	st, _ := Status(o)
	for n, s := range states(st) {
		if s != StateInstalled {
			t.Errorf("status %s: %s", n, s)
		}
	}
}

func TestUninstallRestoresOriginals(t *testing.T) {
	o := testOpts(t)
	claudeMD := filepath.Join(o.Home, ".claude/CLAUDE.md")
	codexMD := filepath.Join(o.Home, ".codex/AGENTS.md")
	settings := filepath.Join(o.Home, ".claude/settings.json")
	write(t, claudeMD, "# My rules\n\nBe nice.\n")
	write(t, codexMD, "codex notes\n")
	write(t, settings, `{
  "model": "opus",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/bin/audit"}]}
    ],
    "Stop": [{"hooks": [{"type": "command", "command": "notify"}]}]
  },
  "zzz": [1, 2, {"a": null}]
}
`)
	before := snapshot(t, o.Home)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(claudeMD); st.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", st.Mode())
	}
	cm := read(t, claudeMD)
	if !strings.HasPrefix(cm, "# My rules\n\nBe nice.\n\n"+blockBegin+"\n") || !strings.HasSuffix(cm, blockEnd+"\n") {
		t.Fatalf("CLAUDE.md:\n%s", cm)
	}
	rep, err := Uninstall(o)
	if err != nil {
		t.Fatal(err)
	}
	for n, s := range states(rep) {
		if s != StateRemoved {
			t.Errorf("%s: %s", n, s)
		}
	}
	after := snapshot(t, o.Home)
	for k, v := range before {
		if after[k] != v && !strings.Contains(k, "settings.json") {
			t.Errorf("%s not restored:\n%q\nvs\n%q", k, after[k], v)
		}
	}
	// settings.json: semantically identical, our hook gone
	var a, b any
	json.Unmarshal([]byte(before["/.claude/settings.json"]), &a)
	json.Unmarshal([]byte(after["/.claude/settings.json"]), &b)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Errorf("settings changed:\n%s\n%s", ja, jb)
	}
	for k := range after {
		if strings.Contains(k, "skills") {
			t.Errorf("skill left behind: %s", k)
		}
	}
	// second uninstall is a no-op
	rep, _ = Uninstall(o)
	for n, s := range states(rep) {
		if s != StateUnchanged {
			t.Errorf("second uninstall %s: %s", n, s)
		}
	}
}

func TestUninstallFreshLeavesNothing(t *testing.T) {
	o := testOpts(t)
	Install(o)
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	for k := range snapshot(t, o.Home) {
		if !strings.Contains(k, "kran-backup") {
			t.Errorf("left behind: %s", k)
		}
	}
}

func TestBlockUpdatedInPlace(t *testing.T) {
	o := testOpts(t)
	p := filepath.Join(o.Home, ".codex/AGENTS.md")
	write(t, p, "top\n\n"+blockBegin+"\nold text\n"+blockEnd+"\n\nbottom\n")
	st, _ := Status(o)
	if states(st)[ItemCodexMD] != StateOutdated {
		t.Fatalf("%v", st)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	want := "top\n\n" + blockText() + "\n\nbottom\n"
	if got := read(t, p); got != want {
		t.Fatalf("got:\n%s", got)
	}
}

func TestBlockBrokenMarkerRefused(t *testing.T) {
	o := testOpts(t)
	p := filepath.Join(o.Home, ".claude/CLAUDE.md")
	write(t, p, blockBegin+"\nhalf\n")
	rep, err := Install(o)
	if err == nil || states(rep)[ItemClaudeMD] != StateSkipped {
		t.Fatalf("expected refusal, got %v %v", rep, err)
	}
	if read(t, p) != blockBegin+"\nhalf\n" {
		t.Fatal("file modified")
	}
	// other items still installed
	if states(rep)[ItemSkill] != StateInstalled {
		t.Fatal("one failure must not stop the others")
	}
}

func TestBlockFileWithoutTrailingNewline(t *testing.T) {
	o := testOpts(t)
	p := filepath.Join(o.Home, ".claude/CLAUDE.md")
	write(t, p, "no newline")
	Install(o)
	if got := read(t, p); !strings.HasPrefix(got, "no newline\n\n"+blockBegin) {
		t.Fatalf("%q", got)
	}
}

func TestSkillOutdatedAndUserFileLeftAlone(t *testing.T) {
	o := testOpts(t)
	p := filepath.Join(o.Home, ".claude/skills/kran/SKILL.md")
	write(t, p, "---\nname: kran\n---\nold\n")
	st, _ := Status(o)
	if states(st)[ItemSkill] != StateOutdated {
		t.Fatal(st)
	}
	Install(o)
	if read(t, p) != skillText {
		t.Fatal("skill not updated")
	}
	write(t, p, "something else entirely\n")
	rep, _ := Uninstall(o)
	if states(rep)[ItemSkill] != StateSkipped || read(t, p) != "something else entirely\n" {
		t.Fatal("must not delete a file that is not ours")
	}
}

func settingsPath(o Options) string { return filepath.Join(o.Home, ".claude/settings.json") }

func loadJSON(t *testing.T, p string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(read(t, p)), &m); err != nil {
		t.Fatalf("invalid JSON written: %v\n%s", err, read(t, p))
	}
	return m
}

func preToolUse(t *testing.T, m map[string]any) []any {
	t.Helper()
	return m["hooks"].(map[string]any)["PreToolUse"].([]any)
}

func countOurs(pre []any) int {
	n := 0
	for _, g := range pre {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			if isOurCommand(h.(map[string]any)["command"].(string)) {
				n++
			}
		}
	}
	return n
}

func TestSettingsMissingAndEmpty(t *testing.T) {
	for name, content := range map[string]*string{"missing": nil, "empty": ptr(""), "blank": ptr("  \n"), "braces": ptr("{}")} {
		t.Run(name, func(t *testing.T) {
			o := testOpts(t)
			if content != nil {
				write(t, settingsPath(o), *content)
			}
			if _, err := Install(o); err != nil {
				t.Fatal(err)
			}
			m := loadJSON(t, settingsPath(o))
			pre := preToolUse(t, m)
			g := pre[0].(map[string]any)
			h := g["hooks"].([]any)[0].(map[string]any)
			if len(pre) != 1 || g["matcher"] != "Bash" || h["type"] != "command" || h["command"] != "/opt/kran/bin/kran hook claude" || h["timeout"] != float64(10) {
				t.Fatalf("%v", pre)
			}
		})
	}
}

func ptr(s string) *string { return &s }

func TestSettingsMergeKeepsOtherHooks(t *testing.T) {
	o := testOpts(t)
	orig := `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/bin/audit"}]},
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "fmt-check"}]}
    ],
    "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "log"}]}]
  },
  "env": {"A": "1"}
}`
	write(t, settingsPath(o), orig)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	m := loadJSON(t, settingsPath(o))
	pre := preToolUse(t, m)
	if len(pre) != 3 || countOurs(pre) != 1 {
		t.Fatalf("%v", pre)
	}
	if pre[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "/usr/bin/audit" {
		t.Fatal("existing hook disturbed")
	}
	if m["env"].(map[string]any)["A"] != "1" || m["permissions"] == nil || m["hooks"].(map[string]any)["PostToolUse"] == nil {
		t.Fatal("unrelated keys lost")
	}
	// key order preserved: permissions before hooks before env
	txt := read(t, settingsPath(o))
	if !(strings.Index(txt, `"permissions"`) < strings.Index(txt, `"hooks"`) && strings.Index(txt, `"hooks"`) < strings.Index(txt, `"env"`)) {
		t.Fatalf("order not preserved:\n%s", txt)
	}
	// one backup, containing the original
	matches, _ := filepath.Glob(settingsPath(o) + ".kran-backup-*")
	if len(matches) != 1 || read(t, matches[0]) != orig {
		t.Fatalf("backup: %v", matches)
	}
	// idempotent: no new backup, same bytes
	before := read(t, settingsPath(o))
	rep, _ := Install(o)
	if states(rep)[ItemClaudeHook] != StateUnchanged || read(t, settingsPath(o)) != before {
		t.Fatal("not idempotent")
	}
	matches, _ = filepath.Glob(settingsPath(o) + ".kran-backup-*")
	if len(matches) != 1 {
		t.Fatalf("extra backup: %v", matches)
	}
}

func TestSettingsUpdatesOldPath(t *testing.T) {
	o := testOpts(t)
	write(t, settingsPath(o), `{"hooks":{"PreToolUse":[{"matcher":"Bash","extra":true,"hooks":[{"type":"command","command":"/old/place/kran hook claude","timeout":5},{"type":"command","command":"other"}]}]}}`)
	st, _ := Status(o)
	if states(st)[ItemClaudeHook] != StateOutdated {
		t.Fatal(st)
	}
	rep, err := Install(o)
	if err != nil || states(rep)[ItemClaudeHook] != StateInstalled {
		t.Fatal(rep, err)
	}
	pre := preToolUse(t, loadJSON(t, settingsPath(o)))
	g := pre[0].(map[string]any)
	hs := g["hooks"].([]any)
	if len(pre) != 1 || len(hs) != 2 || g["extra"] != true || hs[0].(map[string]any)["command"] != "/opt/kran/bin/kran hook claude" || hs[1].(map[string]any)["command"] != "other" {
		t.Fatalf("%v", pre)
	}
	// uninstall removes only ours, keeps the group's other hook
	if _, err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	pre = preToolUse(t, loadJSON(t, settingsPath(o)))
	if countOurs(pre) != 0 || len(pre[0].(map[string]any)["hooks"].([]any)) != 1 {
		t.Fatalf("%v", pre)
	}
}

func TestSettingsInvalidJSONRefused(t *testing.T) {
	for name, body := range map[string]string{
		"garbage":    `{"hooks": `,
		"array":      `[1,2]`,
		"hooks type": `{"hooks": []}`,
		"pre type":   `{"hooks": {"PreToolUse": {}}}`,
		"trailing":   `{} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			o := testOpts(t)
			write(t, settingsPath(o), body)
			rep, err := Install(o)
			if err == nil || !strings.Contains(err.Error(), "settings.json") || states(rep)[ItemClaudeHook] != StateSkipped {
				t.Fatalf("expected refusal: %v %v", rep, err)
			}
			if read(t, settingsPath(o)) != body {
				t.Fatal("invalid file was modified")
			}
			if m, _ := filepath.Glob(settingsPath(o) + ".kran-backup-*"); len(m) != 0 {
				t.Fatal("backup of a file we did not change")
			}
		})
	}
}

func TestSettingsSymlinkFollowed(t *testing.T) {
	o := testOpts(t)
	real := filepath.Join(o.Home, "dotfiles", "settings.json")
	write(t, real, `{"a":1}`)
	if err := os.MkdirAll(filepath.Dir(settingsPath(o)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, settingsPath(o)); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(settingsPath(o)); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	if !strings.Contains(read(t, real), "hook claude") {
		t.Fatal("real file not updated")
	}
}

func TestKranPathWithSpacesQuoted(t *testing.T) {
	o := testOpts(t)
	o.KranPath = "/Users/a b/bin/kran"
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	st, _ := Status(o)
	if states(st)[ItemClaudeHook] != StateInstalled {
		t.Fatal(st)
	}
	if !strings.Contains(read(t, settingsPath(o)), `'/Users/a b/bin/kran' hook claude`) {
		t.Fatal(read(t, settingsPath(o)))
	}
}
