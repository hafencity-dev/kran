package workspace

import "strings"

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

// sshKeyNames are private key file names (id_* in SPEC §6.7). Matching the
// whole id_* prefix would also drop source files like id_test.go.
var sshKeyNames = map[string]bool{
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	"id_ecdsa_sk": true, "id_ed25519_sk": true, "id_xmss": true,
}

// HardExcluded reports whether rel (slash form) must never leave the origin:
// VCS internals, dependency dirs and secrets. It applies even to paths that
// sync.include names explicitly.
func HardExcluded(rel string, isDir bool) bool {
	start := 0
	for i := 0; i <= len(rel); i++ {
		if i == len(rel) || rel[i] == '/' {
			comp := rel[start:i]
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
	return sshKeyNames[name]
}
