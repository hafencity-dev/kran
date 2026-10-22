package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:Org/Repo.git":            "github.com/org/repo",
		"https://github.com/org/repo":            "github.com/org/repo",
		"https://user:pw@github.com/org/repo/":   "github.com/org/repo",
		"ssh://git@github.com:22/org/repo.git":   "github.com/org/repo",
		"git+ssh://git@gitlab.example.com/a/b/c": "gitlab.example.com/a/b/c",
		"github.com:org/repo":                    "github.com/org/repo",
		"/srv/git/project.git":                   "/srv/git/project",
		"file:///srv/git/project.git":            "/srv/git/project",
		"":                                       "",
	}
	for in, want := range cases {
		if got := NormalizeRemote(in); got != want {
			t.Errorf("NormalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectGitWorktreesShareID(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	writeFile(t, root, "a.txt", "a")
	writeFile(t, root, "sub/dir/b.txt", "b")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "init")
	runGit(t, root, "remote", "add", "origin", "git@github.com:Org/Repo.git")

	ws, err := Detect(ctx, filepath.Join(root, "sub", "dir"))
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Git || ws.Root != root || ws.Rel != "sub/dir" || ws.Branch != "main" || ws.Name != "repo" {
		t.Fatalf("unexpected workspace %+v", ws)
	}
	if len(ws.RepoID) != 64 {
		t.Fatalf("repo id %q", ws.RepoID)
	}

	wt := filepath.Join(filepath.Dir(root), "repo-wt")
	runGit(t, root, "worktree", "add", "-q", "-b", "feature", wt)
	ws2, err := Detect(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if ws2.RepoID != ws.RepoID {
		t.Fatalf("worktree repo id differs: %s vs %s", ws2.RepoID, ws.RepoID)
	}
	if ws2.Root != wt || ws2.Branch != "feature" || ws2.Rel != "" || ws2.Name != "repo-wt" {
		t.Fatalf("unexpected worktree workspace %+v", ws2)
	}

	// A clone with a differently spelled remote URL is the same project.
	clone := filepath.Join(filepath.Dir(root), "clone")
	runGit(t, filepath.Dir(root), "clone", "-q", root, clone)
	runGit(t, clone, "remote", "set-url", "origin", "https://github.com/org/repo")
	ws3, err := Detect(ctx, clone)
	if err != nil {
		t.Fatal(err)
	}
	if ws3.RepoID != ws.RepoID {
		t.Fatalf("clone repo id differs")
	}
}

func TestDetectSymlinkedCwd(t *testing.T) {
	root := newRepo(t)
	writeFile(t, root, "pkg/x.go", "package x")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "init")
	link := filepath.Join(filepath.Dir(root), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	ws, err := Detect(context.Background(), filepath.Join(link, "pkg"))
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != root || ws.Rel != "pkg" {
		t.Fatalf("got %+v", ws)
	}
}

func TestDetectSubmodule(t *testing.T) {
	ctx := context.Background()
	lib := newRepo(t)
	writeFile(t, lib, "lib.go", "package lib")
	runGit(t, lib, "add", ".")
	runGit(t, lib, "commit", "-qm", "lib")

	root := filepath.Join(filepath.Dir(lib), "super")
	os.MkdirAll(root, 0o755)
	runGit(t, root, "init", "-q")
	writeFile(t, root, "main.go", "package main")
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "super")
	runGit(t, root, "submodule", "add", "-q", lib, "third_party/lib")
	runGit(t, root, "commit", "-qm", "add sub")

	ws, err := Detect(ctx, filepath.Join(root, "third_party", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if ws.Root != filepath.Join(root, "third_party", "lib") || !ws.Git {
		t.Fatalf("submodule workspace %+v", ws)
	}
	superWS, _ := Detect(ctx, root)
	if ws.RepoID == "" || ws.RepoID == superWS.RepoID {
		t.Fatalf("submodule must be its own repository")
	}
}

func TestDetectEmptyRepoAndPlain(t *testing.T) {
	ctx := context.Background()
	root := newRepo(t)
	ws, err := Detect(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.Git || ws.RepoID == "" {
		t.Fatalf("empty repo: %+v", ws)
	}
	plain := realTempDir(t)
	pws, err := Detect(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if pws.Git || pws.Root != plain || pws.RepoID == "" || pws.RepoID == ws.RepoID {
		t.Fatalf("plain: %+v", pws)
	}
	if _, err := Detect(ctx, filepath.Join(plain, "missing")); err == nil {
		t.Fatal("expected error for missing dir")
	}
}
