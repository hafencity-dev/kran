package execenv

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestCtrLimitArgsKillOrder: a job container's resource arguments, exactly,
// for the kinds of job (SPEC §6.8 kill order). A job without --mem (the
// node's default cap, or none) gets oom_score_adj 1000, one with --mem 500;
// on rootless Docker kran sets neither the score nor the nofile limit.
// Never --oom-kill-disable.
func TestCtrLimitArgsKillOrder(t *testing.T) {
	for _, c := range []struct {
		name     string
		spec     superSpec
		rootless bool
		want     []string
	}{
		{"default cap (no --mem)", superSpec{MemMax: 8 * gib, MemDefault: true, CPUs: 2, Nice: 10}, false,
			[]string{"--ulimit", "nofile=65536:524288", "--memory", "8589934592", "--memory-swap", "8589934592",
				"--cpus", "2.00", "--cpu-shares", "256", "--oom-score-adj", "1000"}},
		{"explicit --mem 8G", superSpec{MemMax: 16 * gib, CPUs: 4}, false,
			[]string{"--ulimit", "nofile=65536:524288", "--memory", "17179869184", "--memory-swap", "17179869184",
				"--cpus", "4.00", "--oom-score-adj", "500"}},
		{"no cap at all", superSpec{CPUs: 1}, false,
			[]string{"--ulimit", "nofile=65536:524288", "--cpus", "1.00", "--oom-score-adj", "1000"}},
		{"rootless Docker", superSpec{MemMax: 8 * gib, MemDefault: true}, true,
			[]string{"--memory", "8589934592", "--memory-swap", "8589934592"}},
	} {
		got := ctrLimitArgs(&c.spec, c.rootless)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
		for _, a := range got {
			if strings.Contains(a, "oom-kill-disable") {
				t.Errorf("%s: %q", c.name, a)
			}
		}
	}
	if ctrOOMScoreDefault <= ctrOOMScoreExplicit || ctrOOMScoreExplicit <= 0 || ctrOOMScoreDefault > 1000 {
		t.Errorf("kill order %d (no --mem) > %d (--mem) > 0 (everything else) broken", ctrOOMScoreDefault, ctrOOMScoreExplicit)
	}
}

// TestSyncHelperArgsKeepDefaultOOMScore: the long-lived sync helper, which
// every container job depends on, gets no score (Docker's 0): after every
// job, never before one.
func TestSyncHelperArgsKeepDefaultOOMScore(t *testing.T) {
	m := newTestEnv(t).m
	got := m.syncHelperArgs("kran-test-sync")
	want := []string{"run", "--detach", "--name", "kran-test-sync", "--label", "kran.ns=kran-test", "--label", "kran.role=sync",
		"--restart", "unless-stopped", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--network", "none",
		"--mount", "type=volume,src=" + m.volume("slots") + ",dst=" + ctrSlots,
		"--entrypoint", "/bin/sh", m.imageTag(), "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sync helper:\n got  %q\n want %q", got, want)
	}
}

// scoreSetting matches a systemd OOMScoreAdjust= with a value (a unit's
// comment that only names the setting does not set it).
var scoreSetting = regexp.MustCompile(`OOMScoreAdjust=\s*[-+0-9]`)

// TestOOMScoreOnlyAtCreateGuard holds that kran gives a kill-order score
// only to job containers it creates, in their `docker run` arguments: the
// literal --oom-score-adj lives in ctrLimitArgs alone, ctrLimitArgs is
// called only where containerSpec builds a job's `docker run`, and no code
// writes oom_score_adj, runs `docker update`, sets OOMScoreAdjust or
// --oom-kill-disable. Containers kran finds (reaper, re-index) or did not
// start (the machine's own) are never touched that way.
func TestOOMScoreOnlyAtCreateGuard(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var flagAt, callers []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "third_party", "web", "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			fn := ""
			if ok {
				fn = fd.Name.Name
			}
			ast.Inspect(d, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if x.Kind != token.STRING {
						return true
					}
					v, _ := strconv.Unquote(x.Value)
					switch {
					case strings.Contains(v, "--oom-score-adj"):
						flagAt = append(flagAt, rel+":"+fn)
					case strings.Contains(v, "oom_score_adj"), strings.Contains(v, "oom-kill-disable"),
						scoreSetting.MatchString(v):
						t.Errorf("%s: %s sets a kill-order score outside a job's docker run: %q", fset.Position(x.Pos()), fn, v)
					}
				case *ast.CallExpr:
					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "ctrLimitArgs" {
						callers = append(callers, rel+":"+fn)
					}
					for _, a := range x.Args {
						if bl, ok := a.(*ast.BasicLit); ok && bl.Kind == token.STRING && bl.Value == `"update"` {
							if sel, ok := x.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "run" || sel.Sel.Name == "docker") {
								t.Errorf("%s: docker update of an existing container", fset.Position(x.Pos()))
							}
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join("internal", "execenv", "container.go") + ":ctrLimitArgs"}; !reflect.DeepEqual(flagAt, want) {
		t.Errorf("--oom-score-adj appears at %q, want only %q", flagAt, want)
	}
	if want := []string{filepath.Join("internal", "execenv", "container.go") + ":containerSpec"}; !reflect.DeepEqual(callers, want) {
		t.Errorf("ctrLimitArgs is called from %q, want only containerSpec's job docker run", callers)
	}
}

// TestVMOutOfMemoryReason: a container killed by the OOM killer far below
// its cap (sampled peak under half of it) was the kernel's pick in a full
// VM or host, not a job over its cap; exit 137 either way.
func TestVMOutOfMemoryReason(t *testing.T) {
	s := &supervisor{spec: superSpec{Node: "devmac", MemMax: 8 << 30, MemDefault: true, Docker: &dockerStep{CLI: "docker"}}}
	where := "devmac"
	if s.memWhere() != "devmac" {
		where = "devmac's Linux VM"
	}
	ex := s.mainExit(stepResult{code: 137, oom: true, peak: 1 << 30, peakSeen: true})
	want := "the kernel's out-of-memory killer ended the job on devmac: " + where + " ran out of memory and took this job (its highest measured use was 1 GiB of its 8 GiB cap)"
	if ex.Code != 137 || ex.Signal != "KILL" || ex.Reason != want {
		t.Errorf("far below its cap: %+v\nwant %q", ex, want)
	}
	// At its cap: the cap message.
	if ex := s.mainExit(stepResult{code: 137, oom: true, peak: 7 << 30, peakSeen: true}); ex.Code != 137 || !strings.HasPrefix(ex.Reason, "job hit its 8 GiB memory cap on devmac") {
		t.Errorf("at its cap: %+v", ex)
	}
	// No measurement of its own (only the docker CLI's rusage): no claim
	// that the VM ran out.
	if ex := s.mainExit(stepResult{code: 137, oom: true, peak: 40 << 20}); !strings.HasPrefix(ex.Reason, "job hit its 8 GiB memory cap") {
		t.Errorf("unmeasured: %+v", ex)
	}
}

// TestCtrOOMScoreInRealContainer (KRAN_TEST_DOCKER=1): Docker applies the
// score to the container's processes, as the kernel reads it.
func TestCtrOOMScoreInRealContainer(t *testing.T) {
	dockerTests(t)
	const image = "alpine:3.24.3"
	cli := dockerCLIForTests()
	if exec.Command(cli, "image", "inspect", image).Run() != nil {
		t.Skipf("image %s not present (no pull in tests)", image)
	}
	for _, c := range []struct {
		spec superSpec
		want string
	}{{superSpec{MemMax: 256 << 20, MemDefault: true, CPUs: 1}, "1000"}, {superSpec{MemMax: 256 << 20, CPUs: 1}, "500"}} {
		args := append([]string{"run", "--rm", "--pull", "never", "--label", "kran.ns=kran-test"}, ctrLimitArgs(&c.spec, false)...)
		args = append(args, image, "cat", "/proc/self/oom_score_adj")
		out, err := exec.CommandContext(context.Background(), cli, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %q: %v: %s", args, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("oom_score_adj in the container %q, want %s", got, c.want)
		}
	}
}
