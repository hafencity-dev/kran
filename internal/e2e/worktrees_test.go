package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
	"github.com/hafencity-dev/kran/internal/workspace"
)

// Worktrees of one repository share its repo ID, so their jobs share the
// target's slots and blob store. Concurrent jobs from two worktrees with
// diverging content must each see exactly their own worktree, round after
// round (slots switch between worktrees, deltas build on each worktree's
// own last manifest).
func TestConcurrentWorktreesOfOneRepo(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "kran-test-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	os.Setenv("KRAN_CACHE_DIR", filepath.Join(base, "cache"))
	log := &logSink{}
	defer func() {
		log.mu.Lock()
		if t.Failed() {
			t.Logf("daemon logs:\n%s", strings.Join(log.lines, "\n"))
		}
		log.closed = true
		log.mu.Unlock()
	}()
	net := transport.NewMemNetwork(transport.SessionConfig{})
	startNode(t, net, log, "b", "a", base)
	a := startNode(t, net, log, "a", "b", base)

	files := map[string]string{"shared.txt": "same everywhere\n", "who.txt": "main\n"}
	for i := 0; i < 200; i++ {
		files[fmt.Sprintf("pkg/f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
	}
	main := gitRepo(t, base, files)
	wt := filepath.Join(base, "wt2")
	git := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git(main, "worktree", "add", "-q", "-b", "other", wt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	spec := func(root string) proto.JobSpec {
		ws, err := workspace.Detect(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		return proto.JobSpec{
			// Hold the slot a moment so the two jobs overlap.
			Argv: []string{"sh", "-c", "cat who.txt; ls pkg | wc -l | tr -d ' '; sleep 0.3"},
			Cwd:  root, Workspace: ws,
			Owner:    proto.Owner{Kind: proto.OwnerHuman, ID: "tester", Name: "tester"},
			Req:      proto.Requirements{OS: proto.OSSame, Prefer: proto.PreferRemote, Node: "b", CPUs: 1, Mem: 256 << 20},
			Priority: proto.PriorityNormal,
		}
	}
	sMain, sWt := spec(main), spec(wt)
	if sMain.Workspace.RepoID != sWt.Workspace.RepoID {
		t.Fatal("worktrees got different repo IDs")
	}
	sock := filepath.Join(a.dirs.Runtime, "kran.sock")

	// Wait until b is usable.
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := runJob(ctx, sock, sMain)
		if err == nil && res.exit != nil && res.exit.Node == "b" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("b never ran a job: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	nMain, nWt := 200, 200
	for round := 0; round < 4; round++ {
		// Diverge: different who.txt, and each round one worktree loses a
		// file the other keeps.
		os.WriteFile(filepath.Join(main, "who.txt"), []byte(fmt.Sprintf("main %d\n", round)), 0o644)
		os.WriteFile(filepath.Join(wt, "who.txt"), []byte(fmt.Sprintf("wt2 %d\n", round)), 0o644)
		if round%2 == 0 {
			os.Remove(filepath.Join(main, fmt.Sprintf("pkg/f%03d.txt", round)))
			nMain--
		} else {
			os.Remove(filepath.Join(wt, fmt.Sprintf("pkg/f%03d.txt", round)))
			nWt--
		}
		want := map[string]string{
			main: fmt.Sprintf("main %d\n%d\n", round, nMain),
			wt:   fmt.Sprintf("wt2 %d\n%d\n", round, nWt),
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		errs := []string{}
		for _, s := range []proto.JobSpec{sMain, sWt, sMain, sWt} {
			wg.Add(1)
			go func(s proto.JobSpec) {
				defer wg.Done()
				res, err := runJob(ctx, sock, s)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err != nil || res.exit == nil:
					errs = append(errs, fmt.Sprintf("%s: %v", filepath.Base(s.Cwd), err))
				case res.exit.Node != "b" || res.exit.Code != 0:
					errs = append(errs, fmt.Sprintf("%s: exit %+v", filepath.Base(s.Cwd), res.exit))
				case res.stdout.String() != want[s.Cwd]:
					errs = append(errs, fmt.Sprintf("%s saw %q, want %q", filepath.Base(s.Cwd), res.stdout.String(), want[s.Cwd]))
				}
			}(s)
		}
		wg.Wait()
		if len(errs) > 0 {
			t.Fatalf("round %d:\n%s", round, strings.Join(errs, "\n"))
		}
	}
}
