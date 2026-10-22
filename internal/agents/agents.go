// Package agents installs kran's agent integration (AX): a Claude Code skill,
// a managed block in CLAUDE.md / AGENTS.md, and a PreToolUse hook that moves
// heavy Bash commands through `kran run`. Everything is idempotent and
// reversible, and edits outside kran's own markers are never touched.
package agents

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Options configure Install, Uninstall and Status.
type Options struct {
	Home     string    // default os.UserHomeDir(); tests use a temp dir
	KranPath string    // absolute path put in the hook command; default os.Executable()
	Stdout   io.Writer // optional: one line per item as it completes
	Now      func() time.Time
}

// Item names reported by Install, Uninstall and Status.
const (
	ItemSkill      = "claude-skill"
	ItemClaudeMD   = "claude-md"
	ItemCodexMD    = "codex-agents-md"
	ItemClaudeHook = "claude-hook"
)

// Item states.
const (
	StateInstalled = "installed"
	StateMissing   = "missing"
	StateOutdated  = "outdated"
	StateRemoved   = "removed"
	StateUnchanged = "unchanged"
	StateSkipped   = "skipped"
)

// Item is the outcome for one integration point.
type Item struct {
	Name   string
	State  string
	Path   string
	Detail string
}

// Report lists every integration point.
type Report struct{ Items []Item }

type env struct {
	home     string
	kranPath string
	now      func() time.Time
	out      io.Writer
}

func (o Options) env() (*env, error) {
	e := &env{home: o.Home, kranPath: o.KranPath, now: o.Now, out: o.Stdout}
	if e.home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot find your home directory: %w", err)
		}
		e.home = h
	}
	if e.kranPath == "" {
		p, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("cannot find the kran binary path (pass it explicitly): %w", err)
		}
		e.kranPath = p
	}
	if e.now == nil {
		e.now = time.Now
	}
	return e, nil
}

func (e *env) skillPath() string {
	return filepath.Join(e.home, ".claude", "skills", "kran", "SKILL.md")
}
func (e *env) claudeMD() string    { return filepath.Join(e.home, ".claude", "CLAUDE.md") }
func (e *env) codexMD() string     { return filepath.Join(e.home, ".codex", "AGENTS.md") }
func (e *env) settings() string    { return filepath.Join(e.home, ".claude", "settings.json") }
func (e *env) hookCommand() string { return quoteIfNeeded(e.kranPath) + " hook claude" }

type step struct {
	name string
	path func(*env) string
	run  func(*env, string) (state, detail string, err error)
}

func (e *env) run(steps []step) (Report, error) {
	var rep Report
	var errs []error
	for _, s := range steps {
		p := s.path(e)
		st, detail, err := s.run(e, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
			st, detail = StateSkipped, err.Error()
		}
		it := Item{Name: s.name, State: st, Path: p, Detail: detail}
		rep.Items = append(rep.Items, it)
		if e.out != nil {
			fmt.Fprintf(e.out, "%s: %s %s\n", it.Name, it.State, it.Path)
		}
	}
	return rep, errors.Join(errs...)
}

// Install sets up the skill, the instruction blocks and the hook.
func Install(opts Options) (Report, error) {
	e, err := opts.env()
	if err != nil {
		return Report{}, err
	}
	return e.run([]step{
		{ItemSkill, (*env).skillPath, (*env).installSkill},
		{ItemClaudeMD, (*env).claudeMD, func(e *env, p string) (string, string, error) { return installBlock(p) }},
		{ItemCodexMD, (*env).codexMD, func(e *env, p string) (string, string, error) { return installBlock(p) }},
		{ItemClaudeHook, (*env).settings, (*env).installHook},
	})
}

// Uninstall removes exactly what Install added.
func Uninstall(opts Options) (Report, error) {
	e, err := opts.env()
	if err != nil {
		return Report{}, err
	}
	return e.run([]step{
		{ItemSkill, (*env).skillPath, (*env).removeSkill},
		{ItemClaudeMD, (*env).claudeMD, func(e *env, p string) (string, string, error) { return removeBlock(p) }},
		{ItemCodexMD, (*env).codexMD, func(e *env, p string) (string, string, error) { return removeBlock(p) }},
		{ItemClaudeHook, (*env).settings, (*env).removeHook},
	})
}

// Status reports the state of each integration point without changing anything.
func Status(opts Options) (Report, error) {
	e, err := opts.env()
	if err != nil {
		return Report{}, err
	}
	return e.run([]step{
		{ItemSkill, (*env).skillPath, (*env).statusSkill},
		{ItemClaudeMD, (*env).claudeMD, func(e *env, p string) (string, string, error) { return statusBlock(p) }},
		{ItemCodexMD, (*env).codexMD, func(e *env, p string) (string, string, error) { return statusBlock(p) }},
		{ItemClaudeHook, (*env).settings, (*env).statusHook},
	})
}

// ---------------------------------------------------------------------------
// files

// writeKeepMode writes data atomically, following symlinks (dotfile managers
// link these files) and keeping the existing file mode.
func writeKeepMode(path string, data []byte, defPerm os.FileMode, dirPerm os.FileMode) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	perm := defPerm
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return err
	}
	return writeAtomic(path, data, perm)
}

func readIfExists(path string) (data []byte, exists bool, err error) {
	data, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (e *env) installSkill(path string) (string, string, error) {
	cur, exists, err := readIfExists(path)
	if err != nil {
		return "", "", err
	}
	if exists && string(cur) == skillText {
		return StateUnchanged, "", nil
	}
	if err := writeKeepMode(path, []byte(skillText), 0o644, 0o755); err != nil {
		return "", "", err
	}
	if exists {
		return StateInstalled, "updated", nil
	}
	return StateInstalled, "", nil
}

func (e *env) statusSkill(path string) (string, string, error) {
	cur, exists, err := readIfExists(path)
	switch {
	case err != nil:
		return "", "", err
	case !exists:
		return StateMissing, "", nil
	case string(cur) != skillText:
		return StateOutdated, "run `kran agents install` to update it", nil
	}
	return StateInstalled, "", nil
}

func (e *env) removeSkill(path string) (string, string, error) {
	cur, exists, err := readIfExists(path)
	if err != nil {
		return "", "", err
	}
	if !exists {
		return StateUnchanged, "not installed", nil
	}
	if !containsLine(string(cur), "name: kran") {
		return StateSkipped, "not a kran skill; left alone", nil
	}
	if err := os.Remove(path); err != nil {
		return "", "", err
	}
	_ = os.Remove(filepath.Dir(path)) // only succeeds when empty
	return StateRemoved, "", nil
}
