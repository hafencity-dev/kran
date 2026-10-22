//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/hafencity-dev/kran/internal/proto"
)

// envMapOf parses `env -0` output.
func envMapOf(out []byte) map[string]string {
	m := map[string]string{}
	for _, kv := range strings.Split(string(out), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// TestE2EEnvFiltering: secrets stay home unless forwarded explicitly;
// host-specific variables are the target's own (SPEC §6.5).
func TestE2EEnvFiltering(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{
		".kran.toml": "[env]\nforward = [\"DB_PASSWORD\"]\nset = { FROM_TOML = \"1\" }\n",
	})
	env := []string{"MY_VAR=visible", "FOO_TOKEN=s1", "AWS_REGION=eu-1", "GITHUB_TOKEN=gh", "DB_PASSWORD=pw",
		"OPENAI_API_KEY=k", "MY_SECRET=x", "LANG=C.UTF-8", "TZ=Europe/Berlin", "GOPATH=/origin/gopath",
		"SSH_AUTH_SOCK=/origin/agent.sock", "TMPDIR=/origin/tmp", "FORCE_COLOR=1"}
	r := a.kran(runOpts{dir: repo, env: env}, "run", "-n", "b", "--env", "FOO_TOKEN", "--", "env", "-0")
	if r.code != 0 {
		t.Fatalf("remote env: %s", r)
	}
	got := envMapOf(r.stdout)
	want := map[string]string{"MY_VAR": "visible", "FOO_TOKEN": "s1", "DB_PASSWORD": "pw", "FROM_TOML": "1",
		"LANG": "C.UTF-8", "TZ": "Europe/Berlin", "FORCE_COLOR": "1", "KRAN_OWNER": "human:e2e",
		"KRAN_NODE": "b", "KRAN_ORIGIN": "a", "KRAN_ORIGIN_ROOT": repo}
	for k, v := range want {
		if k == "KRAN_ORIGIN_ROOT" && got[k] != v {
			// kran reports the root git resolves: /private/tmp/… on macOS.
			if r, err := filepath.EvalSymlinks(v); err == nil && got[k] == r {
				continue
			}
		}
		if got[k] != v {
			t.Errorf("remote %s = %q, want %q", k, got[k], v)
		}
	}
	for _, k := range []string{"AWS_REGION", "GITHUB_TOKEN", "OPENAI_API_KEY", "MY_SECRET", "SSH_AUTH_SOCK",
		"KRAN_CONFIG_DIR", "KRAN_STATE_DIR", "KRAN_RUNTIME_DIR"} {
		if v, ok := got[k]; ok {
			t.Errorf("remote job sees %s=%q", k, v)
		}
	}
	for k, bad := range map[string]string{"GOPATH": "/origin/gopath", "TMPDIR": "/origin/tmp"} {
		if got[k] == bad {
			t.Errorf("host-specific %s forwarded: %q", k, got[k])
		}
	}
	if got["KRAN_JOB_ID"] == "" || got["PATH"] == "" || got["HOME"] == "" {
		t.Errorf("target env incomplete: job %q path %q home %q", got["KRAN_JOB_ID"], got["PATH"], got["HOME"])
	}
	// The API never shows the forwarded environment.
	rj := a.kran(runOpts{}, "jobs", "--json", "-a")
	if strings.Contains(string(rj.stdout), "visible") || strings.Contains(string(rj.stdout), "pw") {
		t.Errorf("kran jobs --json leaks the job environment")
	}
	// Local jobs get the full environment, as without kran.
	r = a.kran(runOpts{dir: repo, env: env}, "run", "--local", "--", "env", "-0")
	if got := envMapOf(r.stdout); got["GITHUB_TOKEN"] != "gh" || got["MY_VAR"] != "visible" {
		t.Errorf("local job env: GITHUB_TOKEN=%q MY_VAR=%q", got["GITHUB_TOKEN"], got["MY_VAR"])
	}
}

// TestE2EWorkspaceSync: secret files are never synced, ignored files stay
// home, modes, mtimes and symlinks survive, .kran.toml sync rules apply.
func TestE2EWorkspaceSync(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a := c.node("a")
	files := map[string]string{
		".gitignore": "*.log\nbuild/\n",
		".kran.toml": "[sync]\ninclude = [\"build/keep.txt\"]\nexclude = [\"testdata/huge/\"]\n",
		".env":       "S=1", ".env.local": "S=2", ".env.example": "S=", ".env.sample": "S=", "sub/.env.production": "S=3",
		"key.pem": "k", "tls.key": "k", "cert.p12": "k", "id_rsa": "k", "id_ed25519": "k",
		".kran/state":         "x",
		"main.go":             "package main\n",
		"sub/code.txt":        "code\n",
		"bin/run.sh":          "#!/bin/sh\necho run\n",
		"testdata/huge/a.bin": "huge",
		"testdata/small.txt":  "small",
		"node_modules/x/i.js": "x",
	}
	repo := makeRepo(t, filepath.Join(c.base, "repo"), files)
	must(t, os.Chmod(filepath.Join(repo, "bin/run.sh"), 0o755))
	// Untracked (not ignored) files are synced, ignored ones are not,
	// sync.include brings an ignored one back.
	must(t, os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("u"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "debug.log"), []byte("l"), 0o644))
	must(t, os.MkdirAll(filepath.Join(repo, "build"), 0o755))
	must(t, os.WriteFile(filepath.Join(repo, "build/keep.txt"), []byte("k"), 0o644))
	must(t, os.WriteFile(filepath.Join(repo, "build/drop.txt"), []byte("d"), 0o644))
	must(t, os.Symlink("sub/code.txt", filepath.Join(repo, "link.txt")))
	mt := time.Date(2001, 2, 3, 4, 5, 6, 123456789, time.UTC)
	for _, f := range []string{"main.go", "sub/code.txt", "bin/run.sh"} {
		must(t, os.Chtimes(filepath.Join(repo, f), mt, mt))
	}

	r := a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c",
		`pwd; find . -path ./.git -prune -o \( -type f -o -type l \) -print | LC_ALL=C sort`)
	if r.code != 0 {
		t.Fatalf("remote listing: %s", r)
	}
	lines := strings.Split(strings.TrimSpace(string(r.stdout)), "\n")
	remoteRoot, listed := lines[0], lines[1:]
	want := []string{"./.env.example", "./.env.sample", "./.gitignore", "./.kran.toml", "./bin/run.sh", "./build/keep.txt",
		"./link.txt", "./main.go", "./sub/code.txt", "./testdata/small.txt", "./untracked.txt"}
	sort.Strings(want)
	if strings.Join(listed, ",") != strings.Join(want, ",") {
		t.Errorf("synced files:\n got %v\nwant %v", listed, want)
	}
	if remoteRoot == repo {
		t.Logf("native runtime ran in the origin path itself (same machine)")
	}

	statCmd := `stat -c '%n %.9Y %a %F' main.go sub/code.txt bin/run.sh link.txt; readlink link.txt`
	if hostOS() == "darwin" {
		statCmd = `stat -f '%N %Fm %Lp %HT' main.go sub/code.txt bin/run.sh link.txt; readlink link.txt`
	}
	local, err := exec.Command("sh", "-c", "cd "+repo+" && "+statCmd).Output()
	if err != nil {
		t.Fatal(err)
	}
	r = a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", statCmd)
	if r.code != 0 || string(r.stdout) != string(local) {
		t.Errorf("mtime/mode/symlink differ:\nremote %q\nlocal  %q\n%s", r.stdout, local, r)
	}
	if !strings.Contains(string(local), "981173106.123456789") {
		t.Errorf("fixture mtime not set as expected: %q", local)
	}

	// Edits sync: change one file, delete another; the slot follows.
	must(t, os.WriteFile(filepath.Join(repo, "sub/code.txt"), []byte("changed\n"), 0o644))
	must(t, os.Remove(filepath.Join(repo, "untracked.txt")))
	r = a.kran(runOpts{dir: repo}, "run", "-n", "b", "--", "sh", "-c", "cat sub/code.txt; test -e untracked.txt && echo STALE; true")
	if r.code != 0 || string(r.stdout) != "changed\n" {
		t.Errorf("incremental sync: %s", r)
	}
}

// TestE2EProjectRules: .kran.toml rules set resources and keep commands
// local.
func TestE2EProjectRules(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, map[string]nodeOpts{"a": {Prefer: "remote"}})
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{
		".kran.toml": `[[rule]]
match = "sh -c *rule-mem*"
cpus = 2
mem = "3GiB"
priority = "low"

[[rule]]
match = "sh -c *rule-local*"
local = true
`})
	r := a.kran(runOpts{dir: repo}, "run", "--json-events", "--", "sh", "-c", "echo rule-mem $KRAN_NODE")
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	id, _ := event(r.events(), "decision")["job_id"].(string)
	j, ok := a.job(id)
	if !ok || j.Spec.Req.Mem != 3<<30 || j.Spec.Req.CPUs != 2 || j.Spec.Priority != proto.PriorityLow || j.Estimate.Source != "rule" && j.Estimate.Source != "flag" {
		t.Errorf("rule not applied: req %+v prio %s estimate %+v", j.Spec.Req, j.Spec.Priority, j.Estimate)
	}
	// Flags beat rules.
	r = a.kran(runOpts{dir: repo}, "run", "--json-events", "--mem", "1G", "--", "sh", "-c", "echo rule-mem")
	id, _ = event(r.events(), "decision")["job_id"].(string)
	if j, _ := a.job(id); j.Spec.Req.Mem != 1<<30 {
		t.Errorf("--mem did not beat the rule: %d", j.Spec.Req.Mem)
	}
	// config prefer=remote sends jobs away, but the local rule wins.
	r = a.kran(runOpts{dir: repo}, "run", "--", "sh", "-c", `echo "rule-local on $KRAN_NODE"`)
	if r.code != 0 || string(r.stdout) != "rule-local on a\n" {
		t.Errorf("local rule: %s", r)
	}
	r = a.kran(runOpts{dir: repo}, "run", "--", "sh", "-c", `echo "plain on $KRAN_NODE"`)
	if r.code != 0 || string(r.stdout) != "plain on b\n" {
		t.Errorf("prefer=remote: %s", r)
	}
	// A broken .kran.toml is an error that says what to do.
	must(t, os.WriteFile(filepath.Join(repo, ".kran.toml"), []byte("[[rule]\nbroken"), 0o644))
	r = a.kran(runOpts{dir: repo}, "run", "--", "true")
	if r.code == 0 || !strings.Contains(string(r.stderr), ".kran.toml") {
		t.Errorf("broken .kran.toml: %s", r)
	}
}

// TestE2EFairShare: two owners competing for one slot are served in turn,
// not first come first served.
func TestE2EFairShare(t *testing.T) {
	c := newCluster(t, []string{"a"}, map[string]nodeOpts{"a": {MaxJobs: 1}})
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	order := filepath.Join(c.base, "order")
	var procs []*cliProc
	submit := func(owner, name string, secs string) {
		p := a.startKran(runOpts{dir: repo, owner: "agent:" + owner, env: []string{"ORDER=" + order}},
			"run", "--local", "--", "sh", "-c", `echo `+name+` >> "$ORDER"; echo ready; sleep `+secs)
		procs = append(procs, p)
	}
	queued := func(n int) {
		t.Helper()
		if !poll(20*time.Second, func() bool {
			st, err := a.state()
			return err == nil && len(st.Queue) >= n
		}) {
			t.Fatalf("queue did not reach %d", n)
		}
	}
	submit("x", "x1", "3")
	if !procs[0].waitStdout("ready", 30*time.Second) {
		t.Fatal(procs[0].wait())
	}
	for i, name := range []string{"x2", "x3", "x4", "x5"} {
		submit("x", name, "0.5")
		queued(i + 1)
	}
	submit("y", "y1", "0.5")
	queued(5)
	submit("y", "y2", "0.5")
	queued(6)
	// Owners show up in the state with their queued counts.
	st := a.statusJSON()
	counts := map[string]int{}
	for _, o := range st.Owners {
		counts[o.Owner.ID] = o.Queued
	}
	if counts["x"] != 4 || counts["y"] != 2 {
		t.Errorf("owner stats %+v", st.Owners)
	}
	for _, p := range procs {
		if r := p.wait(); r.code != 0 {
			t.Errorf("%s", r)
		}
	}
	data, _ := os.ReadFile(order)
	got := strings.Fields(string(data))
	pos := map[string]int{}
	for i, n := range got {
		pos[n] = i
	}
	if len(got) != 7 || pos["y1"] > 2 || pos["y2"] > 4 || pos["x2"] > pos["x3"] || pos["x3"] > pos["x4"] || pos["y1"] > pos["y2"] {
		t.Errorf("start order %v: y's jobs should interleave with x's (fair share, FIFO per owner)", got)
	}
	a.waitIdle(10 * time.Second)
}

// TestE2ETTY: agents (no terminal) get plain pipes and stay normal
// priority; a terminal user gets a pty on the target and high priority.
func TestE2ETTY(t *testing.T) {
	c := newCluster(t, []string{"a", "b"}, nil)
	a := c.node("a")
	repo := makeRepo(t, filepath.Join(c.base, "repo"), map[string]string{"f": "x"})
	script := `if [ -t 0 ]; then i=tty; else i=pipe; fi; if [ -t 1 ]; then o=tty; else o=pipe; fi; echo "in=$i out=$o"; echo "err" >&2`

	r := a.kran(runOpts{dir: repo}, "run", "--json-events", "-n", "b", "--", "sh", "-c", script)
	if r.code != 0 || string(r.stdout) != "in=pipe out=pipe\n" {
		t.Errorf("no tty: %s", r)
	}
	id, _ := event(r.events(), "decision")["job_id"].(string)
	if j, _ := a.job(id); j.Spec.Priority != proto.PriorityNormal || j.Spec.TTY != nil {
		t.Errorf("agent job: priority %s tty %+v", j.Spec.Priority, j.Spec.TTY)
	}
	for _, l := range strings.Split(strings.TrimSpace(string(r.stderr)), "\n") {
		if l != "err" && !strings.HasPrefix(l, `{"kran":`) {
			t.Errorf("unexpected stderr line for an agent: %q", l)
		}
	}

	for _, where := range [][]string{{"-n", "b"}, {"--local"}} {
		out, code := runInPTY(t, a, repo, append(append([]string{"run"}, where...), "--", "sh", "-c", script+"; stty size")...)
		if code != 0 || !strings.Contains(out, "in=tty out=tty") || !strings.Contains(out, "40 100") {
			t.Errorf("%v under a terminal: code %d output %q", where, code, out)
		}
	}
	jobs := a.jobsJSON()
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].QueuedAt.Before(jobs[j].QueuedAt) })
	if last := jobs[len(jobs)-1]; last.Spec.Priority != proto.PriorityHigh || last.Spec.TTY == nil {
		t.Errorf("terminal job: priority %s tty %+v", last.Spec.Priority, last.Spec.TTY)
	}
}

// runInPTY runs the kran CLI on a 40x100 terminal and returns everything
// the terminal showed.
func runInPTY(t *testing.T, n *knode, dir string, args ...string) (string, int) {
	t.Helper()
	cmd := n.command(runOpts{dir: dir}, args...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out strings.Builder
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			k, err := f.Read(buf)
			mu.Lock()
			out.Write(buf[:k])
			mu.Unlock()
			if err != nil {
				close(done)
				return
			}
		}
	}()
	werr := make(chan error, 1)
	go func() { werr <- cmd.Wait() }()
	select {
	case err = <-werr:
	case <-time.After(2 * time.Minute):
		cmd.Process.Kill()
		t.Fatal("kran under a pty hung")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	return out.String(), exitCode(cmd, err)
}

var _ = io.EOF
var _ = json.Marshal
