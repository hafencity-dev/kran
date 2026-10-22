package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hafencity-dev/kran/internal/paths"
	"github.com/hafencity-dev/kran/internal/proto"
)

// gitTimeout bounds every git call made while detecting or scanning; a hung
// git (network filesystem, lock contention) must not hang `kran run`.
const gitTimeout = 10 * time.Second

func detect(ctx context.Context, cwd string) (proto.Workspace, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return proto.Workspace{}, fmt.Errorf("resolve %s: %w", cwd, err)
	}
	// git reports the toplevel with symlinks resolved; resolve cwd the same
	// way so Rel never starts with "..".
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	if fi, err := os.Stat(abs); err != nil {
		return proto.Workspace{}, fmt.Errorf("workspace %s: %w", abs, err)
	} else if !fi.IsDir() {
		return proto.Workspace{}, fmt.Errorf("workspace %s: not a directory", abs)
	}

	out, err := git(ctx, abs, "rev-parse", "--show-toplevel", "--git-common-dir")
	if err != nil {
		return plainWorkspace(abs), nil
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) < 2 || lines[0] == "" {
		return plainWorkspace(abs), nil
	}
	root := filepath.Clean(lines[0])
	commonDir := lines[1]
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(abs, commonDir)
	}

	ws := proto.Workspace{Root: root, Git: true, Name: filepath.Base(root)}
	ws.Rel = relWithin(root, abs)
	if b, err := git(ctx, root, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		ws.Branch = strings.TrimSpace(string(b))
	}
	ws.RepoID = gitRepoID(ctx, root, commonDir)
	return ws, nil
}

// plainWorkspace is the non-git fallback: the cwd itself, identified by
// path and host so two machines never share an ID by accident.
func plainWorkspace(abs string) proto.Workspace {
	host, _ := os.Hostname()
	return proto.Workspace{
		Root:   abs,
		RepoID: hashID("path", abs, strings.ToLower(host)),
		Name:   filepath.Base(abs),
	}
}

// gitRepoID follows SPEC §6.7: root commit + normalized origin URL, so every
// worktree and clone of the same project shares slots and blobs on targets.
func gitRepoID(ctx context.Context, root, commonDir string) string {
	rootCommit := rootCommitOf(ctx, root, commonDir)
	var remote string
	if b, err := git(ctx, root, "config", "--get", "remote.origin.url"); err == nil {
		remote = NormalizeRemote(strings.TrimSpace(string(b)))
	}
	if rootCommit == "" && remote == "" {
		// Empty repository without remote: the common dir is shared by all
		// linked worktrees, so it still identifies the repo on this host.
		host, _ := os.Hostname()
		if real, err := filepath.EvalSymlinks(commonDir); err == nil {
			commonDir = real
		}
		return hashID("gitdir", filepath.Clean(commonDir), strings.ToLower(host))
	}
	return hashID("git", rootCommit, remote)
}

// rootCommitOf finds the repository's original root commit. Walking the
// whole history costs tens of milliseconds on big repos (seconds on huge
// ones) and runs on every `kran run`, so the answer is cached per git common
// dir: a repository's root commit never changes.
func rootCommitOf(ctx context.Context, root, commonDir string) string {
	if real, err := filepath.EvalSymlinks(commonDir); err == nil {
		commonDir = real
	}
	cache := filepath.Join(paths.CacheDir(), "rootcommit", hashID(filepath.Clean(commonDir))[:24])
	if b, err := os.ReadFile(cache); err == nil {
		if c := strings.TrimSpace(string(b)); len(c) >= 40 {
			return c
		}
	}
	b, err := git(ctx, root, "rev-list", "--max-parents=0", "HEAD")
	if err != nil {
		return ""
	}
	// Several roots happen after merging unrelated histories; the last line
	// is the oldest (rev-list sorts newest first), which is the project's
	// original root and is shared by every branch.
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return ""
	}
	c := fields[len(fields)-1]
	if os.MkdirAll(filepath.Dir(cache), 0o700) == nil {
		if f, err := os.CreateTemp(filepath.Dir(cache), ".tmp-*"); err == nil {
			_, werr := f.WriteString(c + "\n")
			if f.Close() == nil && werr == nil {
				os.Rename(f.Name(), cache)
			}
			os.Remove(f.Name())
		}
	}
	return c
}

func hashID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// NormalizeRemote maps the many spellings of a remote URL to one form:
// "git@github.com:Org/Repo.git", "https://user@github.com/org/repo/" and
// "ssh://git@github.com:22/org/repo" all become "github.com/org/repo".
func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	if u == "" {
		return ""
	}
	if i := strings.Index(u, "://"); i >= 0 {
		scheme := strings.ToLower(u[:i])
		u = u[i+3:]
		if scheme == "file" {
			return filepath.Clean(strings.TrimSuffix(u, ".git"))
		}
	} else if strings.HasPrefix(u, "/") || strings.HasPrefix(u, ".") {
		return filepath.Clean(strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git"))
	} else if i := strings.Index(u, ":"); i >= 0 && !strings.Contains(u[:i], "/") {
		// scp-like syntax user@host:path
		u = u[:i] + "/" + strings.TrimPrefix(u[i+1:], "/")
	}
	if i := strings.Index(u, "@"); i >= 0 && i < strings.Index(u+"/", "/") {
		u = u[i+1:]
	}
	host, path, _ := strings.Cut(u, "/")
	if h, port, ok := strings.Cut(host, ":"); ok && isDigits(port) {
		host = h
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	return strings.ToLower(host + "/" + path)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// relWithin returns p relative to root in slash form, "" for root itself.
// Paths outside root yield "" as well; callers that care check for that
// separately (the CLI marks such jobs origin-only).
func relWithin(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

// git runs a git command in dir with optional locks disabled, so scanning
// never contends with the user's own git operations for index.lock.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
