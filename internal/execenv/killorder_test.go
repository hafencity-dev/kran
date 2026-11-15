package execenv

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
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
// only at create time: to job containers it creates, in their `docker run`
// arguments, and to containers a job creates, in the proxy's rewrite of
// that create request: the
// literal --oom-score-adj lives in ctrLimitArgs alone, ctrLimitArgs is
// called only where containerSpec builds a job's `docker run`, and no code
// writes oom_score_adj, runs `docker update`, sets OOMScoreAdjust or
// --oom-kill-disable. Containers kran finds (reaper, re-index) or did not
// start (the machine's own) are never touched that way.
func TestOOMScoreOnlyAtCreateGuard(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var flagAt, callers, apiAt, envAt []string
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
					case strings.Contains(v, "OomScoreAdj"), strings.Contains(v, "OomKillDisable"):
						// Docker API fields: only the proxy's rewrite of a
						// create request a job sends may name them.
						apiAt = append(apiAt, rel+":"+fn)
					case v == "KRAN_INIT_OOM_SCORE":
						envAt = append(envAt, rel+":"+fn)
					case strings.Contains(v, "/update"):
						t.Errorf("%s: %s calls Docker's update API: %q", fset.Position(x.Pos()), fn, v)
					}
				case *ast.Ident:
					if x.Name == "OomScoreAdj" || x.Name == "OomKillDisable" {
						t.Errorf("%s: %s names %s in Go (a struct field would reach Docker's API)", fset.Position(x.Pos()), fn, x.Name)
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
	// The job's score reaches its proxy: set by containerSpec next to the
	// container's own score, read by the proxy, unset before the job runs.
	if want := []string{filepath.Join("internal", "execenv", "container.go") + ":containerSpec",
		filepath.Join("internal", "execenv", "dockerproxy.go") + ":DockerProxy"}; !reflect.DeepEqual(envAt, want) {
		t.Errorf("KRAN_INIT_OOM_SCORE is used at %q, want %q", envAt, want)
	}
	if b, err := os.ReadFile("runner-init.sh"); err != nil || !regexp.MustCompile(`(?m)^unset .*\bKRAN_INIT_OOM_SCORE\b`).Match(b) {
		t.Errorf("runner-init.sh does not unset KRAN_INIT_OOM_SCORE before the job starts (%v)", err)
	}
	// applyOOMScore sets them; checkCreateKeys only refuses other
	// spellings of them.
	proxy := filepath.Join("internal", "execenv", "dockerproxy.go") + ":applyOOMScore"
	check := filepath.Join("internal", "execenv", "dockerproxy.go") + ":checkCreateKeys"
	only := len(apiAt) > 0
	for _, at := range apiAt {
		only = only && (at == proxy || at == check)
	}
	if !only {
		t.Errorf("Docker's OomScoreAdj/OomKillDisable are named at %q, want only in the proxy's create rewrite (%s)", apiAt, proxy)
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

// TestProxyOOMScoreRewriteInvariant: what a job creates through kran's
// Docker proxy (a test database, a compose service) gets at least the
// job's own score, exactly: (a) none asked → the job's, (b) lower → the
// job's, (c) higher → kept, (d) negative → the job's, (e) OomKillDisable →
// dropped. Score 0 (rootless Docker, or no score) changes nothing.
func TestProxyOOMScoreRewriteInvariant(t *testing.T) {
	type tc struct {
		name, hc string
		want     map[string]any
	}
	for _, score := range []int{ctrOOMScoreDefault, ctrOOMScoreExplicit} {
		s := float64(score)
		higher := float64(score)
		if score < 1000 {
			higher = 700
		}
		for _, c := range []tc{
			{"(a) none asked", `{"Memory":1073741824}`, map[string]any{"Memory": float64(1 << 30), "OomScoreAdj": s}},
			{"(a) no HostConfig", ``, map[string]any{"OomScoreAdj": s}},
			{"(b) lower asked", `{"OomScoreAdj":100}`, map[string]any{"OomScoreAdj": s}},
			// At 1000 nothing higher exists (Docker refuses more): equal is kept.
			{"(c) higher (or equal at 1000) asked", `{"OomScoreAdj":` + strconv.Itoa(int(higher)) + `}`, map[string]any{"OomScoreAdj": higher}},
			{"(d) negative asked", `{"OomScoreAdj":-1000}`, map[string]any{"OomScoreAdj": s}},
			{"(e) OomKillDisable", `{"OomKillDisable":true,"OomScoreAdj":-500}`, map[string]any{"OomKillDisable": false, "OomScoreAdj": s}},
		} {
			body := `{"Image":"postgres:17"}`
			if c.hc != "" {
				body = `{"Image":"postgres:17","HostConfig":` + c.hc + `}`
			}
			out, _, err := rewriteCreateContainer(nil, score, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Image      string
				HostConfig map[string]any
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if got.Image != "postgres:17" || !reflect.DeepEqual(got.HostConfig, c.want) {
				t.Errorf("score %d %s: HostConfig %v, want %v", score, c.name, got.HostConfig, c.want)
			}
		}
	}
	for _, body := range []string{`{"Image":"postgres:17"}`, `{"Image":"x","HostConfig":{"OomScoreAdj":-1000,"OomKillDisable":true}}`} {
		if out, changed, _ := rewriteCreateContainer(nil, 0, []byte(body)); changed || string(out) != body {
			t.Errorf("no score: %s became %s", body, out)
		}
	}
	for v, want := range map[string]int{"1000": 1000, "500": 500, "": 0, "0": 0, "-500": 0, "1001": 0, "x": 0} {
		if got := proxyOOMScore(v); got != want {
			t.Errorf("KRAN_INIT_OOM_SCORE=%q: %d, want %d", v, got, want)
		}
	}
}

// TestProxyOOMScoreOnlyOnCreate: through the running proxy, a create gets
// the score; an update, an exec and a list of containers pass unchanged.
func TestProxyOOMScoreOnlyOnCreate(t *testing.T) {
	dir, err := os.MkdirTemp("", "kdo")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	upSock, proxySock := filepath.Join(dir, "up.sock"), filepath.Join(dir, "p.sock")
	seen := fakeDockerUpstream(t, upSock)
	ln, err := net.Listen("unix", proxySock)
	if err != nil {
		t.Fatal(err)
	}
	go serveDockerProxy(ln, upSock, testMaps, ctrOOMScoreExplicit)
	t.Cleanup(func() { ln.Close() })
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", proxySock)
	}}}
	post := func(p, body string) string {
		resp, err := cl.Post("http://docker"+p, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		return <-seen
	}
	if got := post("/v1.47/containers/create?name=pg", `{"Image":"postgres:17","HostConfig":{"OomScoreAdj":-900}}`); !strings.Contains(got, `"OomScoreAdj":500`) || strings.Contains(got, "-900") {
		t.Errorf("create: upstream saw %s", got)
	}
	for _, c := range []struct{ path, body string }{
		{"/v1.47/containers/pg/update", `{"OomKillDisable":true}`},
		{"/v1.47/containers/pg/exec", `{"Cmd":["true"]}`},
	} {
		if got := post(c.path, c.body); got != "POST "+c.path+" "+c.body {
			t.Errorf("%s changed: upstream saw %s", c.path, got)
		}
	}
	resp, err := cl.Get("http://docker/v1.47/containers/json")
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := <-seen; got != "GET /v1.47/containers/json " {
		t.Errorf("list changed: upstream saw %q", got)
	}
}

// TestProxyOOMScoreRealSibling (KRAN_TEST_DOCKER=1): kran's proxy in front
// of this machine's real Docker, as a job's /var/run/docker.sock; a sibling
// the "job" creates asking a negative score reads the job's score inside.
func TestProxyOOMScoreRealSibling(t *testing.T) {
	dockerTests(t)
	const image = "alpine:3.24.3"
	cli := dockerCLIForTests()
	if exec.Command(cli, "image", "inspect", image).Run() != nil {
		t.Skipf("image %s not present (no pull in tests)", image)
	}
	up := "/var/run/docker.sock"
	if _, err := os.Stat(up); err != nil {
		t.Skip("no Docker socket at /var/run/docker.sock")
	}
	dir, err := os.MkdirTemp("", "kdr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "p.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go serveDockerProxy(ln, up, nil, ctrOOMScoreDefault)
	t.Cleanup(func() { ln.Close() })
	cmd := exec.Command(cli, "--host", "unix://"+sock, "run", "--rm", "--pull", "never", "--label", "kran.ns=kran-test",
		"--oom-score-adj", "-500", image, "cat", "/proc/self/oom_score_adj")
	cmd.Env = append(os.Environ(), "DOCKER_CONTEXT=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sibling through the proxy: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1000" {
		t.Errorf("sibling's oom_score_adj %q, want the job's 1000", got)
	}
}
