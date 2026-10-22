package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxPlainFiles bounds the non-git walk: running kran from a huge non-repo
// dir (say $HOME) must fail fast with advice instead of syncing the world.
const maxPlainFiles = 200_000

// gitList returns the paths git considers part of the work tree below
// root/prefix: tracked files plus untracked, non-ignored ones. Paths are
// returned relative to root. Entries ending in "/" are nested repositories
// (untracked) and submodules show up as plain paths that turn out to be
// directories; the caller recurses into both.
func gitList(ctx context.Context, root, prefix string) ([]string, error) {
	dir := root
	if prefix != "" {
		dir = filepath.Join(root, filepath.FromSlash(prefix))
	}
	out, err := git(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, fmt.Errorf("list files in %s: %w", dir, err)
	}
	var names []string
	for len(out) > 0 {
		i := bytes.IndexByte(out, 0)
		if i < 0 {
			i = len(out)
		}
		name := string(out[:i])
		if i < len(out) {
			out = out[i+1:]
		} else {
			out = nil
		}
		if name == "" {
			continue
		}
		if prefix != "" {
			name = prefix + "/" + name
		}
		names = append(names, name)
	}
	return names, nil
}

// plainWalk lists files below root without git, honoring .gitignore files
// and the hard excludes.
func plainWalk(ctx context.Context, root string) ([]string, error) {
	var names []string
	type frame struct {
		dir  string // slash form relative to root, "" = root
		sets []*patternSet
	}
	stack := []frame{{dir: ""}}
	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		abs := filepath.Join(root, filepath.FromSlash(f.dir))
		sets := f.sets
		if ps := loadIgnoreFile(filepath.Join(abs, ".gitignore"), f.dir); ps != nil {
			sets = append(append([]*patternSet(nil), sets...), ps)
		}
		ents, err := os.ReadDir(abs)
		if err != nil {
			if f.dir == "" {
				return nil, fmt.Errorf("read %s: %w", abs, err)
			}
			continue // vanished or unreadable subdir: skip, like git would
		}
		for _, e := range ents {
			rel := e.Name()
			if f.dir != "" {
				rel = f.dir + "/" + rel
			}
			isDir := e.IsDir()
			if HardExcluded(rel, isDir) || ignoredBy(sets, rel, isDir) {
				continue
			}
			if isDir {
				stack = append(stack, frame{dir: rel, sets: sets})
				continue
			}
			names = append(names, rel)
			if len(names) > maxPlainFiles {
				return nil, fmt.Errorf("%s is not a git repository and has more than %d files; run kran from a repository or narrow the workspace", root, maxPlainFiles)
			}
		}
	}
	return names, nil
}

// ignoredBy evaluates .gitignore sets deepest first; the deepest file with
// a matching pattern decides, as in git.
func ignoredBy(sets []*patternSet, rel string, isDir bool) bool {
	for i := len(sets) - 1; i >= 0; i-- {
		if ok, ex := sets[i].verdict(rel, isDir); ok {
			return ex
		}
	}
	return false
}

// includeList resolves sync.include entries to concrete paths. Entries are
// paths or gitignore-style globs relative to the root; directories are
// included recursively (minus hard excludes).
func includeList(ctx context.Context, root string, include []string) ([]string, error) {
	var names []string
	for _, inc := range include {
		inc = strings.TrimSpace(inc)
		if inc == "" {
			continue
		}
		clean := path.Clean(strings.TrimPrefix(strings.TrimRight(inc, "/"), "/"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("sync.include entry %q is outside the workspace; use a path relative to the repository root", inc)
		}
		var matches []string
		switch {
		case strings.Contains(clean, "**"):
			p, _ := parsePattern("/" + clean)
			base := literalPrefix(clean)
			err := walkAll(ctx, root, base, func(rel string, isDir bool) {
				if !isDir && p.match(rel, false) {
					names = append(names, rel)
				}
			})
			if err != nil {
				return nil, err
			}
			continue
		case strings.ContainsAny(clean, "*?["):
			g, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(clean)))
			if err != nil {
				return nil, fmt.Errorf("sync.include entry %q: %w", inc, err)
			}
			for _, m := range g {
				if r, err := filepath.Rel(root, m); err == nil {
					matches = append(matches, filepath.ToSlash(r))
				}
			}
		default:
			matches = []string{clean}
		}
		for _, m := range matches {
			fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(m)))
			if err != nil {
				continue // an include that does not exist yet is not an error
			}
			if !fi.IsDir() {
				names = append(names, m)
				continue
			}
			err = walkAll(ctx, root, m, func(rel string, isDir bool) {
				if !isDir {
					names = append(names, rel)
				}
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return names, nil
}

func literalPrefix(p string) string {
	segs := strings.Split(p, "/")
	var out []string
	for _, s := range segs {
		if strings.ContainsAny(s, "*?[") {
			break
		}
		out = append(out, s)
	}
	return strings.Join(out, "/")
}

// walkAll visits everything below root/base (no symlink following), skipping
// hard-excluded dirs.
func walkAll(ctx context.Context, root, base string, fn func(rel string, isDir bool)) error {
	start := filepath.Join(root, filepath.FromSlash(base))
	count := 0
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, like git does
		}
		if count++; count%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if HardExcluded(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		fn(rel, d.IsDir())
		if count > maxPlainFiles {
			return fmt.Errorf("sync.include below %s matches more than %d files; narrow it", base, maxPlainFiles)
		}
		return nil
	})
	return err
}
