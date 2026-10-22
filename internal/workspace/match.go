package workspace

import (
	"bufio"
	"os"
	"path"
	"strings"
)

// pattern is one gitignore-style pattern. The same syntax is used for
// `.gitignore` files (non-git walk), `sync.include`/`sync.exclude` and the
// target's keep list, so users only have to learn one thing.
type pattern struct {
	neg      bool
	dirOnly  bool     // trailing "/": matches directories only
	anchored bool     // contains a "/" before the end: relative to base
	segs     []string // split on "/", "**" kept as its own segment
}

func parsePattern(line string) (pattern, bool) {
	line = strings.TrimRight(line, "\r")
	// Trailing spaces are ignored unless escaped (gitignore rule).
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}
	var p pattern
	if strings.HasPrefix(line, "!") {
		p.neg = true
		line = line[1:]
	} else if strings.HasPrefix(line, "\\!") || strings.HasPrefix(line, "\\#") {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if strings.HasPrefix(line, "./") {
		line = line[2:]
	}
	if strings.Contains(line, "/") {
		p.anchored = true
		line = strings.TrimLeft(line, "/")
	}
	if line == "" {
		return pattern{}, false
	}
	p.segs = strings.Split(line, "/")
	return p, true
}

// match reports whether rel (slash-separated, relative to the pattern's
// base dir) matches.
func (p pattern) match(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	parts := strings.Split(rel, "/")
	if !p.anchored {
		// A slash-less pattern matches the basename at any depth.
		return segMatch(p.segs[0], parts[len(parts)-1])
	}
	return matchSegs(p.segs, parts)
}

func matchSegs(pat, parts []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return len(parts) > 0
			}
			for i := 0; i <= len(parts); i++ {
				if matchSegs(pat, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 || !segMatch(pat[0], parts[0]) {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}

func segMatch(pat, name string) bool {
	if !strings.ContainsAny(pat, "*?[\\") {
		return pat == name
	}
	ok, err := path.Match(pat, name)
	return err == nil && ok
}

// patternSet is an ordered list of patterns; the last match wins, as in git.
type patternSet struct {
	base string // dir (slash form, relative to the workspace root) the patterns are relative to
	pats []pattern
}

func newPatternSet(base string, lines []string) *patternSet {
	ps := &patternSet{base: base}
	for _, l := range lines {
		if p, ok := parsePattern(l); ok {
			ps.pats = append(ps.pats, p)
		}
	}
	return ps
}

func loadIgnoreFile(file, base string) *patternSet {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	ps := newPatternSet(base, lines)
	if len(ps.pats) == 0 {
		return nil
	}
	return ps
}

// verdict returns (matched, excluded) for rel against this set.
func (ps *patternSet) verdict(rel string, isDir bool) (bool, bool) {
	if ps == nil {
		return false, false
	}
	if ps.base != "" {
		if !strings.HasPrefix(rel, ps.base+"/") {
			return false, false
		}
		rel = rel[len(ps.base)+1:]
	}
	for i := len(ps.pats) - 1; i >= 0; i-- {
		if ps.pats[i].match(rel, isDir) {
			return true, !ps.pats[i].neg
		}
	}
	return false, false
}

// Matcher answers "is this path (or one of its parent dirs) matched" for a
// flat list of patterns relative to the workspace root.
type Matcher struct{ ps *patternSet }

// NewMatcher compiles gitignore-style patterns. An empty list matches nothing.
func NewMatcher(patterns []string) *Matcher {
	ps := newPatternSet("", patterns)
	if len(ps.pats) == 0 {
		return &Matcher{}
	}
	return &Matcher{ps: ps}
}

// Match reports whether rel or any of its parent directories is matched.
func (m *Matcher) Match(rel string, isDir bool) bool {
	if m == nil || m.ps == nil {
		return false
	}
	// Walk prefixes so "build/" excludes everything below build/.
	for i := 0; i < len(rel); i++ {
		if rel[i] == '/' {
			if ok, ex := m.ps.verdict(rel[:i], true); ok && ex {
				return true
			}
		}
	}
	_, ex := m.ps.verdict(rel, isDir)
	return ex
}

// Empty reports whether the matcher has no patterns.
func (m *Matcher) Empty() bool { return m == nil || m.ps == nil }
