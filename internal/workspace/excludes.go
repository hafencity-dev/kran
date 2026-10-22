package workspace

import (
	"regexp"
	"strings"
)

// DefaultKeep lists target-side dirs that survive slot updates although they
// are not synced: dependency and cache dirs that setup or the tools maintain
// themselves (SPEC §6.7). Slash-less entries match at any depth.
var DefaultKeep = []string{"node_modules/", ".next/cache/", "target/", ".venv/", ".turbo/", ".pnpm-store/"}

// hardExcludedDir lists directory names that are never synced, at any depth.
var hardExcludedDir = map[string]bool{
	".git":         true,
	"node_modules": true,
	".kran":        true,
}

// sshKeyName matches private ssh key file names (id_* in SPEC §6.7),
// including copies like id_rsa_deploy or id_ed25519.bak. Matching the whole
// id_* prefix would also drop source files like id_test.go.
var sshKeyName = regexp.MustCompile(`^id_(rsa|dsa|ecdsa|ed25519|xmss)(_sk)?([._-].*)?$`)

// HardExcluded reports whether rel (slash form) must never leave the origin:
// VCS internals, dependency dirs and secrets. It applies even to paths that
// sync.include names explicitly.
func HardExcluded(rel string, isDir bool) bool {
	start := 0
	for i := 0; i <= len(rel); i++ {
		if i == len(rel) || rel[i] == '/' {
			// Lower case: macOS file systems ignore case, so .ENV is .env
			// and .GIT is the repository.
			comp := strings.ToLower(rel[start:i])
			last := i == len(rel)
			// Also matches files and symlinks of that name: a ".git" file
			// links a submodule or worktree, a node_modules symlink points
			// into a dependency tree; neither belongs on the target.
			if hardExcludedDir[comp] {
				return true
			}
			if last && !isDir && secretFile(comp) {
				return true
			}
			start = i + 1
		}
	}
	return false
}

// secretFile matches a lower-cased file name.
func secretFile(name string) bool {
	if name == ".env" {
		return true
	}
	if strings.HasPrefix(name, ".env.") {
		for _, ok := range []string{".example", ".sample", ".template"} {
			if strings.HasSuffix(name, ok) {
				return false
			}
		}
		return true
	}
	for _, ext := range []string{".pem", ".key", ".p12"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return sshKeyName.MatchString(name) && !strings.HasSuffix(name, ".pub")
}
