package agents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Claude Code checks its permission rules against the command it runs. Once
// the hook wraps a command, that is `kran run --sh '…'`, so the user's rules
// for the original would no longer apply. The hook therefore evaluates them
// itself and keeps the outcome identical:
//
//   - a deny or ask rule matches the original (or the wrapped form) → leave
//     the command alone, Claude Code applies the rule;
//   - allow rules cover the original → wrap and answer "allow";
//   - otherwise → wrap without a decision (Claude Code asks as it would
//     have), unless an allow rule would approve the wrapped form, which the
//     user never allowed for this command → leave it alone.
//
// Settings that cannot be read or understood leave the command alone.
// Rule syntax (code.claude.com/docs/en/iam): `Bash` or `Bash(*)` matches
// every command, `Bash(cmd:*)` commands starting with cmd, `*` matches any
// text, anything else the exact command.

// managedSettings are the enterprise policy files Claude Code reads.
var managedSettings = func() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/Library/Application Support/ClaudeCode/managed-settings.json"}
	}
	return []string{"/etc/claude-code/managed-settings.json"}
}()

type bashRule struct {
	text   string // as written, for the decision reason
	all    bool
	glob   string // pattern with * wildcards
	prefix bool   // written as <glob>:*
}

type ruleSet struct{ allow, deny, ask []bashRule }

// settingsFiles lists the settings files whose rules apply in project dir
// cwd: managed policy, the user's, and the project's (CLAUDE_PROJECT_DIR,
// else cwd and its parents, since the agent may have cd'ed).
func settingsFiles(cwd string) []string {
	files := append([]string{}, managedSettings...)
	cfg := os.Getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		if h, err := os.UserHomeDir(); err == nil {
			cfg = filepath.Join(h, ".claude")
		}
	}
	if cfg != "" {
		files = append(files, filepath.Join(cfg, "settings.json"))
	}
	var dirs []string
	if p := os.Getenv("CLAUDE_PROJECT_DIR"); p != "" {
		dirs = []string{p}
	} else if filepath.IsAbs(cwd) {
		for d := filepath.Clean(cwd); ; d = filepath.Dir(d) {
			dirs = append(dirs, d)
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	for _, d := range dirs {
		files = append(files, filepath.Join(d, ".claude", "settings.json"), filepath.Join(d, ".claude", "settings.local.json"))
	}
	return files
}

func loadRules(files []string) (ruleSet, error) {
	var rs ruleSet
	for _, f := range files {
		data, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) && strings.HasPrefix(f, "/etc/") {
			continue
		}
		if err != nil {
			return rs, err
		}
		var doc struct {
			Permissions json.RawMessage `json:"permissions"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			return rs, fmt.Errorf("%s: %w", f, err)
		}
		if len(doc.Permissions) == 0 || string(doc.Permissions) == "null" {
			continue
		}
		var p struct {
			Allow, Deny, Ask []string
		}
		if err := json.Unmarshal(doc.Permissions, &p); err != nil {
			return rs, fmt.Errorf("%s: permissions: %w", f, err)
		}
		for _, l := range []struct {
			in  []string
			out *[]bashRule
		}{{p.Allow, &rs.allow}, {p.Deny, &rs.deny}, {p.Ask, &rs.ask}} {
			for _, s := range l.in {
				r, ok, err := parseBashRule(s)
				if err != nil {
					return rs, fmt.Errorf("%s: %w", f, err)
				}
				if ok {
					*l.out = append(*l.out, r)
				}
			}
		}
	}
	return rs, nil
}

// parseBashRule parses one permission rule; ok=false for other tools.
func parseBashRule(s string) (bashRule, bool, error) {
	s = strings.TrimSpace(s)
	if s == "Bash" {
		return bashRule{text: s, all: true}, true, nil
	}
	inner, ok := strings.CutPrefix(s, "Bash(")
	if !ok {
		return bashRule{}, false, nil
	}
	inner, ok = strings.CutSuffix(inner, ")")
	if !ok {
		return bashRule{}, false, fmt.Errorf("permission rule %q is not closed", s)
	}
	inner = strings.TrimSpace(inner)
	r := bashRule{text: s}
	if inner == "" || inner == "*" || inner == ":*" {
		r.all = true
		return r, true, nil
	}
	r.glob, r.prefix = strings.CutSuffix(inner, ":*")
	return r, true, nil
}

// match reports whether the rule covers cmd. strict (for allow rules) makes
// a :* prefix end at a word boundary; deny and ask rules match more loosely.
func (r bashRule) match(cmd string, strict bool) bool {
	cmd = strings.TrimSpace(cmd)
	switch {
	case r.all:
		return true
	case !r.prefix:
		return globMatch(r.glob, cmd)
	case globMatch(r.glob, cmd):
		return true
	case strict:
		return globMatch(r.glob+" *", cmd)
	}
	return globMatch(r.glob+"*", cmd)
}

func anyMatch(rules []bashRule, strict bool, cmds ...string) (bashRule, bool) {
	for _, r := range rules {
		for _, c := range cmds {
			if r.match(c, strict) {
				return r, true
			}
		}
	}
	return bashRule{}, false
}

// globMatch matches s against p where * matches any run of characters.
func globMatch(p, s string) bool {
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi] == '*':
			star, mark = pi, si
			pi++
		case pi < len(p) && p[pi] == s[si]:
			pi++
			si++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// permissionVerdict decides what the hook does with cmd, given its wrapped
// forms. wrap=false leaves the command alone; allow carries an allow rule.
func permissionVerdict(rs ruleSet, cmd string, wrapped []string) (wrap bool, allow string) {
	cmds, ok := lexShell(cmd)
	if !ok {
		return false, ""
	}
	// Deny and ask: the whole command, each part as written, and each part
	// without VAR=… / env / time / nice prefixes.
	loose := []string{cmd}
	for _, c := range cmds {
		loose = append(loose, c.raw, strings.Join(c.words, " "))
		if w := stripPrefixes(c.words); len(w) > 0 {
			loose = append(loose, strings.Join(w, " "))
		}
	}
	loose = append(loose, wrapped...)
	if _, hit := anyMatch(rs.deny, false, loose...); hit {
		return false, ""
	}
	if _, hit := anyMatch(rs.ask, false, loose...); hit {
		return false, ""
	}
	// Allow: the whole command, or every part (pipe filters only read
	// output and need no rule of their own).
	if r, hit := anyMatch(rs.allow, true, cmd); hit {
		return true, r.text
	}
	var used string
	all := len(cmds) > 0
	for _, c := range cmds {
		if c.op == "|" && isFilter(c.words) {
			continue
		}
		r, hit := anyMatch(rs.allow, true, c.raw)
		if !hit {
			all = false
			break
		}
		used = r.text
	}
	if all && used != "" {
		return true, used
	}
	if _, hit := anyMatch(rs.allow, true, wrapped...); hit {
		return false, "" // the wrapped form would be approved, the original was not
	}
	return true, ""
}
