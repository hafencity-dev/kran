package workspace

import (
	"context"
	"path"
	"strings"
)

// Synced reports whether rel (slash form, relative to the git work tree at
// root) reaches a target: tracked or untracked-but-not-ignored, named by
// sync.include, or inside a directory the target maintains itself
// (DefaultKeep: node_modules/.bin/vitest exists there after setup). An
// error (git failing) counts as synced: the check only exists to keep a job
// home that would fail remotely, never to keep one home on a guess.
func Synced(ctx context.Context, root, rel string, include []string) bool {
	rel = path.Clean(strings.TrimPrefix(rel, "./"))
	for _, k := range DefaultKeep {
		k = strings.TrimSuffix(k, "/")
		if strings.Contains(k, "/") {
			if rel == k || strings.HasPrefix(rel, k+"/") {
				return true
			}
			continue
		}
		if rel == k || strings.HasPrefix(rel, k+"/") || strings.Contains(rel, "/"+k+"/") {
			return true
		}
	}
	if HardExcluded(rel, false) {
		return false
	}
	for _, inc := range include {
		inc = path.Clean(strings.TrimPrefix(strings.TrimRight(strings.TrimSpace(inc), "/"), "/"))
		if inc == "." || inc == "" {
			continue
		}
		if rel == inc || strings.HasPrefix(rel, inc+"/") {
			return true
		}
		if ok, _ := path.Match(inc, rel); ok {
			return true
		}
	}
	out, err := git(ctx, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", rel)
	if err != nil {
		return true
	}
	return len(out) > 0
}
